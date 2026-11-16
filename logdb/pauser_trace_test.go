//go:build test

package logdb

import (
	"context"
	"net"
	"testing"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"google.golang.org/grpc"
)

type tracedShard struct {
	pb.UnimplementedShardQueryServer
	seen chan [2]string // trace id, span id
}

func (s *tracedShard) Pause(ctx context.Context, _ *pb.PauseRequest) (*pb.PauseResponse, error) {
	trace, _ := weftsdk.TraceIDFromContext(ctx)
	span, _ := weftsdk.CurrentSpanID(ctx)
	s.seen <- [2]string{trace, span}
	return &pb.PauseResponse{}, nil
}

// The shard's pause lines belong in the trace of the roll that paused it, so
// the leader's Pause call must carry the trace and span of its ctx.
func TestGRPCPauser_PauseCarriesTheCallersTrace(t *testing.T) {
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	const span = "00f067aa0ba902b7"
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rec := &tracedShard{seen: make(chan [2]string, 1)}
	srv := grpc.NewServer(grpc.UnaryInterceptor(weftsdk.GRPC.UnaryServerInterceptor()))
	pb.RegisterShardQueryServer(srv, rec)
	go srv.Serve(lis)
	defer srv.Stop()

	ctx := weftsdk.ContextWithSpanID(weftsdk.ContextWithTraceID(context.Background(), traceID), span)
	if err := (grpcPauser{}).Pause(ctx, lis.Addr().String(), "10.0.0.9:9000"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	select {
	case got := <-rec.seen:
		if got != [2]string{traceID, span} {
			t.Fatalf("shard saw trace/span %v, want %s/%s", got, traceID, span)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Pause never reached the shard")
	}
}
