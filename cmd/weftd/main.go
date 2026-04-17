package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"gopkg.in/yaml.v3"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/weftd"
)

// globalHostGroup is set at startup from config and stamped on every LogRow.
var globalHostGroup string

type Config struct {
	Endpoint string         `yaml:"endpoint"`  // coordinator address
	Token    string         `yaml:"token"`     // agent auth token (wft_...)
	Customer string         `yaml:"customer"`  // fallback if not returned by auth
	HostGroup string        `yaml:"host_group"` // logical group (e.g. "coordinator", "shard") — sent alongside hostname
	Socket   string         `yaml:"socket"`
	Sources  []SourceConfig `yaml:"sources"`
}

type SourceConfig struct {
	App     string        `yaml:"app,omitempty"`    // bare app name within namespace (e.g. "coordinator")
	Path    string        `yaml:"path"`             // file path or glob (file tailing)
	Unit    string        `yaml:"unit,omitempty"`    // systemd unit name (journal tailing)
	Metrics MetricsConfig `yaml:"metrics,omitempty"` // optional system metrics collection
}

func main() {
	configPath := flag.String("config", "/etc/weftd/config.yaml", "config file path")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("weftd " + Version)
		os.Exit(0)
	}

	log.Printf("weftd %s starting", Version)

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

	// Connect to coordinator. The bearer token is attached to every RPC
	// via PerRPCCredentials — IngestStream reads it from metadata.
	coordConn, err := grpc.NewClient(cfg.Endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(bearerToken{token: cfg.Token}),
	)
	if err != nil {
		log.Fatalf("connect to coordinator: %v", err)
	}
	coordClient := pb.NewWeftCoordinatorClient(coordConn)

	// Build metrics config to report to coordinator.
	var metricsProto *pb.MetricsConfig
	for _, src := range cfg.Sources {
		if src.Metrics.Enabled {
			collectors := src.Metrics.Collectors
			if len(collectors) == 0 {
				collectors = []string{"cpu", "memory", "disk", "load", "network"}
			}
			metricsProto = &pb.MetricsConfig{
				Enabled:    true,
				Interval:   src.Metrics.Interval,
				Collectors: collectors,
			}
			break // report first enabled source's config
		}
	}

	// Authenticate to verify token and get customer name.
	//   PermissionDenied → subscription inactive; retry in 24h.
	//   Unavailable      → coordinator not reachable yet; retry with backoff.
	//   anything else    → fatal (bad token, bad config).
	var customer, namespace string
	var transportBackoff = time.Second
	for {
		authResp, err := coordClient.Authenticate(ctx, &pb.AuthenticateRequest{
			Token:         cfg.Token,
			MetricsConfig: metricsProto,
		})
		if err != nil {
			switch status.Code(err) {
			case codes.PermissionDenied:
				log.Printf("subscription inactive: %v — retrying in 24h", status.Convert(err).Message())
				select {
				case <-ctx.Done():
					os.Exit(0)
				case <-time.After(24 * time.Hour):
				}
				continue
			case codes.Unavailable:
				log.Printf("coordinator unreachable, retrying in %v: %v", transportBackoff, err)
				select {
				case <-ctx.Done():
					os.Exit(0)
				case <-time.After(transportBackoff):
				}
				transportBackoff *= 2
				if transportBackoff > 30*time.Second {
					transportBackoff = 30 * time.Second
				}
				continue
			default:
				log.Fatalf("authenticate: %v", err)
			}
		}
		customer = authResp.Customer
		if customer == "" {
			customer = cfg.Customer
		}
		namespace = authResp.Namespace
		log.Printf("authenticated as customer=%s namespace=%s", customer, namespace)
		break
	}

	// Periodic re-auth: check subscription status daily.
	// On rejection, log and exit so systemd restarts us into the retry loop above.
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, err := coordClient.Authenticate(ctx, &pb.AuthenticateRequest{Token: cfg.Token})
				if err != nil && status.Code(err) == codes.PermissionDenied {
					log.Fatalf("subscription expired: %v", status.Convert(err).Message())
				}
			}
		}
	}()

	hostname, _ := os.Hostname()
	globalHostGroup = cfg.HostGroup

	// Start sources — each independently onboards then streams.
	for _, src := range cfg.Sources {
		src := src // capture loop var

		// Derive the app name for this source.
		app := src.App
		if app == "" && src.Unit != "" {
			app = strings.TrimSuffix(src.Unit, ".service")
		}
		if app == "" && src.Path != "" {
			// Derive from filename: /var/log/weft/coordinator.log → coordinator
			base := filepath.Base(src.Path)
			app = strings.TrimSuffix(base, filepath.Ext(base))
		}
		fullApp := namespace + "/" + app

		if src.Unit != "" {
			go func() {
				if err := onboardAndRun(ctx, coordClient, cfg.Token, app, src.Path, hostname, fullApp, func() {
					runJournalTailer(ctx, coordClient, customer, hostname, fullApp, src.Unit)
				}); err != nil {
					log.Printf("[%s] onboard failed: %v", fullApp, err)
				}
			}()
		} else if src.Path != "" {
			if strings.ContainsAny(src.Path, "*?[") {
				// Glob: each matched file is onboarded independently from inside watchGlob.
				// The app name derived from the pattern ("*") is meaningless here.
				go watchGlob(ctx, coordClient, cfg.Token, customer, hostname, namespace, src.Path)
			} else {
				// Single file: onboard once, then tail.
				go func() {
					if err := onboardAndRun(ctx, coordClient, cfg.Token, app, src.Path, hostname, fullApp, func() {
						runFileTailer(ctx, coordClient, customer, hostname, fullApp, src.Path)
					}); err != nil {
						log.Printf("[%s] onboard failed: %v", fullApp, err)
					}
				}()
			}
		}
		if src.Metrics.Enabled {
			go runMetricsCollector(ctx, coordClient, customer, hostname, namespace, src.Metrics)
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

// onboardAndRun checks if a source is already configured. If so, runs the
// tailer immediately. If not, submits a sample and polls until configured.
func onboardAndRun(ctx context.Context, client pb.WeftCoordinatorClient, token, app, path, hostname, fullApp string, run func()) error {
	// Check if already configured.
	status, err := weftd.CheckSourceStatus(ctx, client, token, app)
	if err != nil {
		log.Printf("[%s] status check failed: %v — proceeding to sample", fullApp, err)
	} else if status.Status == "configured" {
		log.Printf("[%s] already configured — starting", fullApp)
		run()
		return nil
	}

	// Submit sample.
	log.Printf("[%s] new source — sampling %s", fullApp, path)
	if err := weftd.SubmitSample(ctx, client, token, app, path, hostname, globalHostGroup); err != nil {
		return err
	}
	log.Printf("[%s] sample submitted — waiting for configuration", fullApp)

	// Poll until configured.
	schemaYAML, err := weftd.WaitForConfiguration(ctx, client, token, app)
	if err != nil {
		return err
	}
	_ = schemaYAML // TODO: apply schema to parser when weftd gains client-side parsing
	log.Printf("[%s] configured — starting", fullApp)
	run()
	return nil
}

// bearerToken implements grpc.PerRPCCredentials so every outgoing RPC
// carries `authorization: Bearer <token>`. The coordinator reads this in
// IngestStream (and any other streaming RPC) to validate the caller.
type bearerToken struct{ token string }

func (b bearerToken) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + b.token}, nil
}

func (bearerToken) RequireTransportSecurity() bool { return false }

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

// watchGlob watches a directory for new files matching a glob pattern. For
// each matching file — both files present at startup and files created later —
// it runs onboardAndRun with the file's real path and a per-file app name
// derived from the filename. This handles the case where log files don't
// exist yet at startup (e.g. gunicorn creates them on first request).
func watchGlob(ctx context.Context, client pb.WeftCoordinatorClient, token, customer, instance, namespace, pattern string) {
	dir := filepath.Dir(pattern)
	tailing := make(map[string]bool)

	startFile := func(p string) {
		if tailing[p] {
			return
		}
		tailing[p] = true
		base := filepath.Base(p)
		app := strings.TrimSuffix(base, filepath.Ext(base))
		fullApp := namespace + "/" + app
		go func() {
			if err := onboardAndRun(ctx, client, token, app, p, instance, fullApp, func() {
				runFileTailer(ctx, client, customer, instance, fullApp, p)
			}); err != nil {
				log.Printf("[%s] onboard failed: %v", fullApp, err)
			}
		}()
	}

	// Onboard + tail any files that already match.
	startMatching := func() {
		paths, _ := filepath.Glob(pattern)
		for _, p := range paths {
			startFile(p)
		}
	}

	startMatching()
	if len(tailing) > 0 {
		log.Printf("[%s] tailing %d files matching %s", namespace, len(tailing), pattern)
	}

	// Watch directory for new files.
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("[%s] watch %s: %v", namespace, dir, err)
		return
	}
	defer watcher.Close()

	if err := watcher.Add(dir); err != nil {
		// Directory doesn't exist yet — wait for it.
		log.Printf("[%s] directory %s not found, waiting...", namespace, dir)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := watcher.Add(dir); err == nil {
					log.Printf("[%s] directory %s appeared", namespace, dir)
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
				matched, _ := filepath.Match(filepath.Base(pattern), filepath.Base(event.Name))
				if matched {
					log.Printf("[%s] new file %s", namespace, event.Name)
					startFile(event.Name)
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("[%s] watch error: %v", namespace, err)
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
		HostGroup: globalHostGroup,
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
