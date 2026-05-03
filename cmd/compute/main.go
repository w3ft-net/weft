package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/kevlarcon/weft/compute"
	computesvc "github.com/kevlarcon/weft/compute/service"
	pb "github.com/kevlarcon/weft/proto/weftpb"
)

func main() {
	port := flag.Int("port", 9010, "gRPC listen port")
	// Accepted but unused until docs/plans/streaming-parallel-warp.md
	// lands. The systemd unit passes --parallelism=4 today; without
	// this stub flag.Parse rejects the binary on boot.
	_ = flag.Int("parallelism", 0, "(reserved) intra-stage parallelism; see streaming-parallel-warp.md")
	flag.Parse()

	pool := &compute.LocalComputePool{}
	srv := grpc.NewServer()
	pb.RegisterWeftComputeServer(srv, computesvc.NewServer(pool))

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("compute worker listening on :%d", *port)

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
		log.Printf("compute: received %s, gracefully stopping", s)
		srv.GracefulStop()
	}()

	if err := srv.Serve(lis); err != nil {
		log.Printf("serve error: %v", err)
		os.Exit(1)
	}
}
