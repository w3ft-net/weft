package main

import (
	"context"
	"net"
	"testing"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	weftsdk "github.com/w3ft-net/weft-go"
	"google.golang.org/grpc"
)

type traceRecorder struct {
	pb.UnimplementedWeftAdminServer
	seen chan [2]string // trace id, span id
}

func (r *traceRecorder) RelaunchShard(ctx context.Context, _ *pb.RelaunchShardRequest) (*pb.RelaunchShardResponse, error) {
	trace, _ := weftsdk.TraceIDFromContext(ctx)
	span, _ := weftsdk.CurrentSpanID(ctx)
	r.seen <- [2]string{trace, span}
	return &pb.RelaunchShardResponse{}, nil
}

// A roll runs in a trace so the coordinator's upgrade lines can be pulled up
// with `trace --id`: the id in the caller's ctx must reach RelaunchShard, in
// a span of the shard's own under the caller's.
func TestRollOne_RelaunchCarriesTheCallersTrace(t *testing.T) {
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	const rollSpan = "00f067aa0ba902b7"
	t.Setenv(adminTokenEnv, "tok")

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rec := &traceRecorder{seen: make(chan [2]string, 1)}
	srv := grpc.NewServer(grpc.UnaryInterceptor(weftsdk.GRPC.UnaryServerInterceptor()))
	pb.RegisterWeftAdminServer(srv, rec)
	go srv.Serve(lis)
	defer srv.Stop()

	client, closeConn, err := newAdminClient(lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer closeConn()

	// GetShard is unimplemented, so the wait fails fast; only the relaunch matters.
	_ = rollOne(weftsdk.ContextWithSpanID(weftsdk.ContextWithTraceID(context.Background(), traceID), rollSpan), client, "s-1", 200*time.Millisecond, 10*time.Millisecond, "")
	select {
	case got := <-rec.seen:
		if got[0] != traceID {
			t.Fatalf("RelaunchShard saw trace %q, want %q", got[0], traceID)
		}
		if got[1] == "" || got[1] == rollSpan {
			t.Fatalf("RelaunchShard saw span %q, want a new span under %s", got[1], rollSpan)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RelaunchShard never reached the server")
	}
}
