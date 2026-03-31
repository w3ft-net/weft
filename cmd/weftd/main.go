package main

import (
	"context"
	"flag"
	"log"
	"os"
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
	Endpoint string         `yaml:"endpoint"`
	Token    string         `yaml:"token"`
	Customer string         `yaml:"customer"`
	Socket   string         `yaml:"socket"` // Unix socket path
	Sources  []SourceConfig `yaml:"sources"`
}

type SourceConfig struct {
	App  string `yaml:"app"`
	Path string `yaml:"path"` // file path or glob
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
		log.Fatal("endpoint is required in config")
	}

	conn, err := grpc.NewClient(cfg.Endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("connect to %s: %v", cfg.Endpoint, err)
	}
	defer conn.Close()
	client := pb.NewWeftCoordinatorClient(conn)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hostname, _ := os.Hostname()

	// Start file tailers.
	for _, src := range cfg.Sources {
		paths, err := filepath.Glob(src.Path)
		if err != nil || len(paths) == 0 {
			log.Printf("[%s] no files matching %s", src.App, src.Path)
			continue
		}
		for _, p := range paths {
			go runFileTailer(ctx, client, cfg.Customer, hostname, src.App, p)
		}
		log.Printf("[%s] tailing %d files matching %s", src.App, len(paths), src.Path)
	}

	// Start socket listener.
	if cfg.Socket != "" {
		sock, err := weftd.NewSocketListener(cfg.Socket, "default")
		if err != nil {
			log.Fatalf("socket listener: %v", err)
		}
		defer sock.Close()
		go runSocketIngester(ctx, client, cfg.Customer, hostname, sock)
		log.Printf("listening on socket %s", cfg.Socket)
	}

	log.Printf("weftd started: %d file sources, endpoint=%s", len(cfg.Sources), cfg.Endpoint)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("shutting down")
	cancel()
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

	type appLine struct {
		app  string
		line string
	}
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
	stream.Send(&pb.IngestStreamMessage{
		App:       app,
		Customer:  customer,
		Instance:  instance,
		Raw:       line,
		TsUnixNs: time.Now().UnixNano(),
		Fields:    strings.Fields(line),
	})
}
