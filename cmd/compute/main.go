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
	"time"

	"google.golang.org/grpc"

	weftsdk "github.com/w3ft-net/weft-go"
	weftruntime "github.com/w3ft-net/weft-go/runtime"

	"github.com/kevlarcon/weft/compute"
	computesvc "github.com/kevlarcon/weft/compute/service"
	pb "github.com/kevlarcon/weft/proto/weftpb"
)

func main() {
	port := flag.Int("port", 9010, "gRPC listen port")
	parallelism := flag.Int("parallelism", 2, "warp parallelism per job")
	flag.Parse()

	pool := &compute.LocalComputePool{Parallelism: *parallelism}
	srv := grpc.NewServer()
	pb.RegisterWeftComputeServer(srv, computesvc.NewServer(pool))

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("compute worker listening on :%d (parallelism=%d)", *port, *parallelism)

	sdkCtx, sdkCancel := context.WithCancel(context.Background())
	defer sdkCancel()
	wc, _ := weftsdk.New()
	defer wc.Close()
	go wc.HeartbeatLoop(sdkCtx, "compute", 30*time.Second)
	weftruntime.Start(sdkCtx, wc, "compute", weftruntime.DefaultInterval)

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
