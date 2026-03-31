package main

import (
	"context"
	"bufio"
	"flag"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

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
	App     string `yaml:"app"`
	Path    string `yaml:"path"`              // file path or glob (file tailing)
	Unit    string `yaml:"unit,omitempty"`     // systemd unit name (journal tailing)
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

	// Authenticate with coordinator to get shard address.
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

	// Stream logs through the coordinator (agents can't reach shards directly).
	// Keep the coordinator connection open for the lifetime of the agent.

	hostname, _ := os.Hostname()

	// Start sources.
	for _, src := range cfg.Sources {
		if src.Unit != "" {
			// Journal source — tail systemd unit logs.
			go runJournalTailer(ctx, coordClient, customer, hostname, src.App, src.Unit)
			log.Printf("[%s] tailing journal unit %s", src.App, src.Unit)
		} else if src.Path != "" {
			// File source — tail log files.
			paths, err := filepath.Glob(src.Path)
			if err != nil || len(paths) == 0 {
				log.Printf("[%s] no files matching %s", src.App, src.Path)
				continue
			}
			for _, p := range paths {
				go runFileTailer(ctx, coordClient, customer, hostname, src.App, p)
			}
			log.Printf("[%s] tailing %d files matching %s", src.App, len(paths), src.Path)
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

	log.Printf("weftd started: %d file sources, streaming to shard %s", len(cfg.Sources), authResp.ShardAddr)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("shutting down")
	cancel()
}

// runJournalTailer tails a systemd unit's journal and streams lines.
func runJournalTailer(ctx context.Context, client pb.WeftCoordinatorClient, customer, instance, app, unit string) {
	// Start journalctl following the unit from now.
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

	stream, err := client.IngestStream(ctx)
	if err != nil {
		log.Printf("[%s] open stream: %v", app, err)
		cmd.Process.Kill()
		return
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		sendLine(stream, line, customer, instance, app)
	}

	stream.CloseAndRecv()
	cmd.Wait()
}

func runFileTailer(ctx context.Context, client pb.WeftCoordinatorClient, customer, instance, app, path string) {
	tailer, err := weftd.NewTailer(path)
	if err != nil {
		log.Printf("[%s] open %s: %v", app, path, err)
		return
	}
	defer tailer.Close()

	stream, err := client.IngestStream(ctx)
	if err != nil {
		log.Printf("[%s] open stream: %v", app, err)
		return
	}

	lines := tailer.Lines()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	var buf []string

	for {
		select {
		case <-ctx.Done():
			flush(stream, buf, customer, instance, app)
			stream.CloseAndRecv()
			return
		case line, ok := <-lines:
			if !ok {
				flush(stream, buf, customer, instance, app)
				stream.CloseAndRecv()
				return
			}
			buf = append(buf, line)
			if len(buf) >= 100 {
				flush(stream, buf, customer, instance, app)
				buf = buf[:0]
			}
		case <-ticker.C:
			if len(buf) > 0 {
				flush(stream, buf, customer, instance, app)
				buf = buf[:0]
			}
		}
	}
}

func runSocketIngester(ctx context.Context, client pb.WeftCoordinatorClient, customer, instance string, sock *weftd.SocketListener) {
	stream, err := client.IngestStream(ctx)
	if err != nil {
		log.Printf("socket ingester: open stream: %v", err)
		return
	}

	lines := sock.Lines()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	type appLine struct{ app, line string }
	var buf []appLine

	for {
		select {
		case <-ctx.Done():
			for _, al := range buf {
				sendLine(stream, al.line, customer, instance, al.app)
			}
			stream.CloseAndRecv()
			return
		case sl, ok := <-lines:
			if !ok {
				stream.CloseAndRecv()
				return
			}
			buf = append(buf, appLine{app: sl.App, line: sl.Line})
			if len(buf) >= 100 {
				for _, al := range buf {
					sendLine(stream, al.line, customer, instance, al.app)
				}
				buf = buf[:0]
			}
		case <-ticker.C:
			if len(buf) > 0 {
				for _, al := range buf {
					sendLine(stream, al.line, customer, instance, al.app)
				}
				buf = buf[:0]
			}
		}
	}
}

func flush(stream pb.WeftCoordinator_IngestStreamClient, lines []string, customer, instance, app string) {
	for _, line := range lines {
		sendLine(stream, line, customer, instance, app)
	}
}

func sendLine(stream pb.WeftCoordinator_IngestStreamClient, line, customer, instance, app string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	stream.Send(&pb.LogRow{
		App:       app,
		Customer:  customer,
		Instance:  instance,
		Raw:       line,
		TsUnixNs: time.Now().UnixNano(),
		Fields:    strings.Fields(line),
	})
}
