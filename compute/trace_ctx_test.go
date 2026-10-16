package compute

import (
	"context"
	"net"
	"sync"
	"testing"

	weftsdk "github.com/w3ft-net/weft-go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
)

// ctxCaptureShard records the ctx handed to QueryToSink.
type ctxCaptureShard struct {
	shard.Shard
	mu  sync.Mutex
	ctx context.Context
}

func (s *ctxCaptureShard) ID() string { return "trace-shard" }
func (s *ctxCaptureShard) QueryToSink(ctx context.Context, _ string, _ []any, sink shard.RowSink) {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
	sink.SendSentinel("trace-shard")
}

type ctxCaptureResolver struct{ sh shard.Shard }

func (r ctxCaptureResolver) ShardID() string            { return "trace-shard" }
func (r ctxCaptureResolver) Dial() (shard.Shard, error) { return r.sh, nil }

// The trace ID on the ctx given to Engine.ExecuteQuery must reach the
// shard client call (QueryToSink, which couriers it over gRPC).
func TestEngine_ExecuteQuery_TraceIDReachesShardCall(t *testing.T) {
	sh := &ctxCaptureShard{}
	eng := &Engine{}
	eng.RegisterResolver(ctxCaptureResolver{sh: sh})

	ctx := weftsdk.ContextWithTraceID(context.Background(), "trace-engine-1")
	stream, err := eng.ExecuteQuery(ctx, &pb.ExecuteQueryRequest{
		Sql:    "SELECT 1",
		Shards: []*pb.ShardDescriptor{{ShardId: "trace-shard"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DrainLineStream(context.Background(), stream); err != nil {
		t.Fatal(err)
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	got, _ := weftsdk.TraceIDFromContext(sh.ctx)
	if got != "trace-engine-1" {
		t.Fatalf("shard call saw trace %q, want trace-engine-1", got)
	}
}

type mdComputeServer struct {
	pb.UnimplementedWeftComputeServer
	mu sync.Mutex
	md metadata.MD
}

func (s *mdComputeServer) ExecuteQueryStream(_ *pb.ExecuteQueryRequest, stream grpc.ServerStreamingServer[pb.ExecuteQueryStreamResponse]) error {
	md, _ := metadata.FromIncomingContext(stream.Context())
	s.mu.Lock()
	s.md = md
	s.mu.Unlock()
	return nil
}

// The compute Client must ferry the ctx trace ID as gRPC metadata.
func TestClient_ExecuteQuery_TraceIDInMetadata(t *testing.T) {
	srv := &mdComputeServer{}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	pb.RegisterWeftComputeServer(gs, srv)
	go gs.Serve(lis)
	defer gs.GracefulStop()

	c, err := NewClient(lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx := weftsdk.ContextWithTraceID(context.Background(), "trace-client-1")
	stream, err := c.ExecuteQuery(ctx, &pb.ExecuteQueryRequest{Sql: "SELECT 1"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = DrainLineStream(context.Background(), stream)

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if v := srv.md.Get("weft-trace-id"); len(v) != 1 || v[0] != "trace-client-1" {
		t.Fatalf("compute server metadata weft-trace-id = %v, want [trace-client-1]", v)
	}
}
