package main

import (
	"bufio"
	"context"
	"flag"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"gopkg.in/yaml.v3"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/weftd"
)

type Config struct {
	Endpoint string         `yaml:"endpoint"` // coordinator address
	Token    string         `yaml:"token"`    // agent auth token (wft_...)
	Customer string         `yaml:"customer"` // fallback if not returned by auth
	Socket   string         `yaml:"socket"`
	Sources  []SourceConfig `yaml:"sources"`
}

type SourceConfig struct {
	App  string `yaml:"app"`
	Path string `yaml:"path"`          // file path or glob (file tailing)
	Unit string `yaml:"unit,omitempty"` // systemd unit name (journal tailing)
}

func main() {
	configPath := flag.String("config", "/etc/weftd/config.yaml", "config file path")
	flag.Parse()

	data, err := os.ReadFile(*configPath)
	if err != nil {
		log.Fatalf("read config: %v", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("parse config: %v", err)
	}

	if cfg.Endpoint == "" {
		log.Fatal("endpoint is required")
	}
	if cfg.Token == "" {
		log.Fatal("token is required")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Connect to coordinator.
	coordConn, err := grpc.NewClient(cfg.Endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("connect to coordinator: %v", err)
	}
	coordClient := pb.NewWeftCoordinatorClient(coordConn)

	// Authenticate to verify token and get customer name.
	authResp, err := coordClient.Authenticate(ctx, &pb.AuthenticateRequest{Token: cfg.Token})
	if err != nil {
		log.Fatalf("authenticate: %v", err)
	}

	customer := authResp.Customer
	if customer == "" {
		customer = cfg.Customer
	}
	log.Printf("authenticated as customer=%s", customer)

	hostname, _ := os.Hostname()

	// Start sources — all stream through the coordinator, never direct to shards.
	for _, src := range cfg.Sources {
		if src.Unit != "" {
			go runJournalTailer(ctx, coordClient, customer, hostname, src.App, src.Unit)
			log.Printf("[%s] tailing journal unit %s", src.App, src.Unit)
		} else if src.Path != "" {
			go watchGlob(ctx, coordClient, customer, hostname, src.App, src.Path)
		}
	}

	// Start socket listener.
	if cfg.Socket != "" {
		sock, err := weftd.NewSocketListener(cfg.Socket, "default")
		if err != nil {
			log.Fatalf("socket listener: %v", err)
		}
		defer sock.Close()
		go runSocketIngester(ctx, coordClient, customer, hostname, sock)
		log.Printf("listening on socket %s", cfg.Socket)
	}

	log.Printf("weftd started: %d sources, streaming to %s", len(cfg.Sources), cfg.Endpoint)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("shutting down")
	cancel()
}

// openStream opens an IngestStream to the coordinator with retry.
// Blocks until a stream is established or ctx is cancelled.
func openStream(ctx context.Context, client pb.WeftCoordinatorClient, label string) (pb.WeftCoordinator_IngestStreamClient, error) {
	var backoff time.Duration = time.Second

	for {
		stream, err := client.IngestStream(ctx)
		if err == nil {
			return stream, nil
		}

		log.Printf("[%s] stream failed, retrying in %v: %v", label, backoff, err)

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

// runJournalTailer tails a systemd unit's journal and streams lines.
// Reconnects automatically if the stream breaks.
func runJournalTailer(ctx context.Context, client pb.WeftCoordinatorClient, customer, instance, app, unit string) {
	cmd := exec.CommandContext(ctx, "journalctl", "-f", "-u", unit, "--no-pager", "-o", "cat", "-n", "0")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("[%s] journalctl pipe: %v", app, err)
		return
	}
	if err := cmd.Start(); err != nil {
		log.Printf("[%s] journalctl start: %v", app, err)
		return
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		if err := sendLineWithReconnect(ctx, client, &streamState{}, line, customer, instance, app); err != nil {
			return // context cancelled
		}
	}

	cmd.Wait()
}

func runFileTailer(ctx context.Context, client pb.WeftCoordinatorClient, customer, instance, app, path string) {
	tailer, err := weftd.NewTailer(path)
	if err != nil {
		log.Printf("[%s] open %s: %v", app, path, err)
		return
	}
	defer tailer.Close()

	lines := tailer.Lines()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	ss := &streamState{}
	var buf []string

	for {
		select {
		case <-ctx.Done():
			flushWithReconnect(ctx, client, ss, buf, customer, instance, app)
			if ss.stream != nil {
				ss.stream.CloseAndRecv()
			}
			return
		case line, ok := <-lines:
			if !ok {
				flushWithReconnect(ctx, client, ss, buf, customer, instance, app)
				if ss.stream != nil {
					ss.stream.CloseAndRecv()
				}
				return
			}
			buf = append(buf, line)
			if len(buf) >= 100 {
				flushWithReconnect(ctx, client, ss, buf, customer, instance, app)
				buf = buf[:0]
			}
		case <-ticker.C:
			if len(buf) > 0 {
				flushWithReconnect(ctx, client, ss, buf, customer, instance, app)
				buf = buf[:0]
			}
		}
	}
}

// watchGlob watches a directory for new files matching a glob pattern and
// starts tailers for each. Handles the case where log files don't exist at
// startup (e.g., gunicorn creates them on first request).
func watchGlob(ctx context.Context, client pb.WeftCoordinatorClient, customer, instance, app, pattern string) {
	dir := filepath.Dir(pattern)
	tailing := make(map[string]bool)

	// Tail any files that already match.
	startMatching := func() {
		paths, _ := filepath.Glob(pattern)
		for _, p := range paths {
			if !tailing[p] {
				tailing[p] = true
				go runFileTailer(ctx, client, customer, instance, app, p)
			}
		}
	}

	startMatching()
	if len(tailing) > 0 {
		log.Printf("[%s] tailing %d files matching %s", app, len(tailing), pattern)
	}

	// Watch directory for new files.
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("[%s] watch %s: %v", app, dir, err)
		return
	}
	defer watcher.Close()

	if err := watcher.Add(dir); err != nil {
		// Directory doesn't exist yet — wait for it.
		log.Printf("[%s] directory %s not found, waiting...", app, dir)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := watcher.Add(dir); err == nil {
					log.Printf("[%s] directory %s appeared", app, dir)
					startMatching()
					goto watch
				}
			}
		}
	}

watch:
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Op&fsnotify.Create == fsnotify.Create {
				// Check if the new file matches our glob.
				matched, _ := filepath.Match(filepath.Base(pattern), filepath.Base(event.Name))
				if matched && !tailing[event.Name] {
					tailing[event.Name] = true
					log.Printf("[%s] new file %s, starting tailer", app, event.Name)
					go runFileTailer(ctx, client, customer, instance, app, event.Name)
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("[%s] watch error: %v", app, err)
		}
	}
}

func runSocketIngester(ctx context.Context, client pb.WeftCoordinatorClient, customer, instance string, sock *weftd.SocketListener) {
	lines := sock.Lines()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	ss := &streamState{}

	type appLine struct{ app, line string }
	var buf []appLine

	for {
		select {
		case <-ctx.Done():
			for _, al := range buf {
				sendLineWithReconnect(ctx, client, ss, al.line, customer, instance, al.app)
			}
			if ss.stream != nil {
				ss.stream.CloseAndRecv()
			}
			return
		case sl, ok := <-lines:
			if !ok {
				if ss.stream != nil {
					ss.stream.CloseAndRecv()
				}
				return
			}
			buf = append(buf, appLine{app: sl.App, line: sl.Line})
			if len(buf) >= 100 {
				for _, al := range buf {
					sendLineWithReconnect(ctx, client, ss, al.line, customer, instance, al.app)
				}
				buf = buf[:0]
			}
		case <-ticker.C:
			if len(buf) > 0 {
				for _, al := range buf {
					sendLineWithReconnect(ctx, client, ss, al.line, customer, instance, al.app)
				}
				buf = buf[:0]
			}
		}
	}
}

// streamState holds a lazily-opened, reconnectable IngestStream.
type streamState struct {
	stream pb.WeftCoordinator_IngestStreamClient
}

// sendLineWithReconnect sends a line, opening or reconnecting the stream as needed.
func sendLineWithReconnect(ctx context.Context, client pb.WeftCoordinatorClient, ss *streamState, line, customer, instance, app string) error {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}

	msg := &pb.LogRow{
		App:       app,
		Customer:  customer,
		Instance:  instance,
		Raw:       line,
		TsUnixNs: time.Now().UnixNano(),
		Fields:    strings.Fields(line),
	}

	// Try sending on existing stream.
	if ss.stream != nil {
		if err := ss.stream.Send(msg); err == nil {
			return nil
		}
		// Stream is broken — will reconnect below.
		log.Printf("[%s] stream broken, reconnecting", app)
		ss.stream = nil
	}

	// Open a new stream.
	stream, err := openStream(ctx, client, app)
	if err != nil {
		return err // context cancelled
	}
	ss.stream = stream

	// Retry the send on the fresh stream.
	if err := ss.stream.Send(msg); err != nil {
		log.Printf("[%s] send failed after reconnect: %v", app, err)
		ss.stream = nil
	}
	return nil
}

func flushWithReconnect(ctx context.Context, client pb.WeftCoordinatorClient, ss *streamState, lines []string, customer, instance, app string) {
	for _, line := range lines {
		if err := sendLineWithReconnect(ctx, client, ss, line, customer, instance, app); err != nil {
			return
		}
	}
}
