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

	"github.com/w3ft-net/weft/cluster"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"google.golang.org/grpc"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/w3ft-net/weft/compute"
	computesvc "github.com/w3ft-net/weft/compute/service"
	pb "github.com/w3ft-net/weft/proto/weftpb"
)

func main() {
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
	readerDefault := flag.String("archive-reader-default", "", "the reader of every archive format with no -archive-reader of its own (the open build has "+compute.ArchiveReaderLambda+")")
	cfgPath := cluster.DefineFlag(flag.CommandLine)
	flag.Parse()
	if err := cluster.Overlay(flag.CommandLine, cluster.Compute, *cfgPath); err != nil {
		log.Fatalf("compute: %v", err)
	}

	// Region must be threaded explicitly: the SDK default chain does
	// not reliably resolve it from IMDS, and signing Lambda cold-shard
	// requests with an empty region yields a 403 from the function
	// URL's IAM authorizer. See compute.ResolveRegion.
	region := compute.ResolveRegion()

	// Bearer token for the gRPC-shard interceptor. Compute owns this
	// now — the coordinator no longer carries it in ExecuteQueryRequest.
	// Both sides (shard server, compute client) read SHARD_SECRET from
	// their own env so the credential never travels on the coord→compute
	// hop. cmd/shard/main.go reads the same var.
	shardSecret := os.Getenv("SHARD_SECRET")
	if shardSecret == "" {
		log.Fatal("no SHARD_SECRET set")
	}

	var creds aws.CredentialsProvider
	if awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(region)); err != nil {
		weftsdk.Warnf(context.Background(), "warning: AWS config unavailable — cold shard queries will fail: %v", err)
	} else {
		creds = awsCfg.Credentials
	}
	archives, err := compute.NewArchiveRouter(archiveReaders(readers, *readerDefault), creds, region)
	if err != nil {
		log.Fatalf("compute: archive readers: %v (give -archive-reader-default %s, or -archive-reader FORMAT=NAME)", err, compute.ArchiveReaderLambda)
	}
	engine := &compute.Engine{Archive: archives, ShardSecret: shardSecret}
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(weftsdk.GRPC.UnaryServerInterceptor()),
		grpc.ChainStreamInterceptor(weftsdk.GRPC.StreamServerInterceptor()),
	)
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
