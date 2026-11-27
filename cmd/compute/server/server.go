package server

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/w3ft-net/weft/cluster"

	"google.golang.org/grpc"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/w3ft-net/weft/backends/logs/archive"
	"github.com/w3ft-net/weft/compute"
	computesvc "github.com/w3ft-net/weft/compute/service"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/transport"
)

// Options is what a deployment decides about its compute process.
type Options struct {
	// Readers are the archive readers this process can read with, by name: the
	// storage the deployment uses supplies them (compute.ArchiveReaderLambda,
	// compute.ArchiveReaderLocal, or its own). Built after the flags are parsed.
	Readers func() map[string]archive.Reader
	// DefaultReader is the reader of every archive format with no
	// -archive-reader of its own, when no -archive-reader-default names one. Empty:
	// there is none, and a process given no reader refuses to start.
	DefaultReader string
}

// Run is the compute process. It returns only when the process is to exit.
func Run(opts Options) {
	weftsdk.InitLogging("", os.Stderr).CaptureStdio()
	port := flag.Int("port", 9010, "gRPC listen port")
	// Accepted but unused until intra-stage
	// parallelism lands. The systemd unit passes --parallelism=4 today; without
	// this stub flag.Parse rejects the binary on boot.
	_ = flag.Int("parallelism", 0, "(reserved) intra-stage parallelism")
	// Which reader reads the archives written in each format. There is no
	// default: a compute process that starts with none could never read an
	// archived shard, and would find out on the first query that reached one.
	readers := readerFlag{}
	flag.Var(readers, "archive-reader", "FORMAT=NAME: the reader of the archives written in FORMAT (repeatable)")
	readerDefault := flag.String("archive-reader-default", "", "the reader of every archive format with no -archive-reader of its own (a reader name this build links)")
	cfgPath := cluster.DefineFlag(flag.CommandLine)
	tlsCfg := transport.DefineFlags(flag.CommandLine)
	flag.Parse()
	if err := tlsCfg.Apply(); err != nil {
		log.Fatalf("compute: %v", err)
	}
	if err := cluster.Overlay(flag.CommandLine, cluster.Compute, *cfgPath); err != nil {
		log.Fatalf("compute: %v", err)
	}

	// Bearer token for the gRPC-shard interceptor. Compute owns this
	// now — the coordinator no longer carries it in ExecuteQueryRequest.
	// Both sides (shard server, compute client) read SHARD_SECRET from
	// their own env so the credential never travels on the coord→compute
	// hop. cmd/shard/main.go reads the same var.
	shardSecret := os.Getenv("SHARD_SECRET")
	if shardSecret == "" {
		log.Fatal("no SHARD_SECRET set")
	}

	def := *readerDefault
	if def == "" && len(readers) == 0 {
		def = opts.DefaultReader
	}
	var available map[string]archive.Reader
	if opts.Readers != nil {
		available = opts.Readers()
	}
	archives, err := compute.NewArchiveRouter(archiveReaders(readers, def), available)
	if err != nil {
		log.Fatalf("compute: archive readers: %v (give -archive-reader-default NAME, or -archive-reader FORMAT=NAME)", err)
	}
	engine := &compute.Engine{Archive: archives, ShardSecret: shardSecret}
	srv := grpc.NewServer(append(transport.ServerOptions(),
		grpc.ChainUnaryInterceptor(weftsdk.GRPC.UnaryServerInterceptor()),
		grpc.ChainStreamInterceptor(weftsdk.GRPC.StreamServerInterceptor()),
	)...)
	pb.RegisterWeftComputeServer(srv, computesvc.NewServer(engine))

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	weftsdk.Infof(context.Background(), "compute worker listening on :%d", *port)

	sdkCtx, sdkCancel := context.WithCancel(context.Background())
	defer sdkCancel()
	wc, _ := weftsdk.New()
	defer wc.Close()
	go wc.Heartbeat(sdkCtx, "compute", weftsdk.DefaultHeartbeatInterval)
	go wc.RuntimeStats(sdkCtx, "compute", weftsdk.DefaultRuntimeStatsInterval)

	// Handle SIGTERM / SIGINT so the process exits cleanly from
	// main() — under `go build -cover` that's the only path that
	// flushes coverage counters to GOCOVERDIR. SIGKILL skips this.
	// docker compose sends SIGTERM on `compose down`, then SIGKILL
	// 10s later; the graceful-stop path has to fit inside that.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sigs
		weftsdk.Infof(context.Background(), "compute: received %s, gracefully stopping", s)
		srv.GracefulStop()
	}()

	if err := srv.Serve(lis); err != nil {
		weftsdk.Errorf(context.Background(), "serve error: %v", err)
		os.Exit(1)
	}
}
