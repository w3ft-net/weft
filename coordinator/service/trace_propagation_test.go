package service

import (
	"context"
	"strings"
	"testing"

	weftsdk "github.com/w3ft-net/weft-go"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/w3ft-net/weft/coordinator"
	"github.com/w3ft-net/weft/logtest"
	pb "github.com/w3ft-net/weft/proto/weftpb"
)

// captureStdLog redirects the stdlib logger (what coordinator/service's
// Execute uses for its "query rpc: error..." line) for the duration of
// fn and returns everything written.
func captureStdLog(t *testing.T, fn func()) string {
	t.Helper()
	return logtest.Capture(fn)
}

// traceCaptureStream is a minimal grpc.ServerStream carrying a fixed
// ctx, modeled on execute_follow_flush_test.go's flushCaptureStream —
// only Context()/Send() are exercised for a non-follow Execute call
// that errors before any row is produced.
type traceCaptureStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *traceCaptureStream) Context() context.Context                  { return s.ctx }
func (s *traceCaptureStream) Send(*pb.CoordinatorExecuteResponse) error { return nil }

// TestExecuteRPC_TraceIDSurvivesGRPCMetadataBoundary proves the
// mechanism this dogfooding plan depends on: a `weft-trace-id` gRPC
// metadata value (the courier api/handlers.go's coordCtx ferries out,
// mirroring the pre-existing weft-dirstack pattern) survives the
// api→coordinator boundary and reaches the coordinator's own log
// output — context.Value doesn't cross a gRPC call on its own, so
// this is the thing that could silently be wrong.
//
// Uses a deliberately unparseable pipeline so Execute fails fast
// (bash.Parse error) without needing real shard/compute infra — the
// error path's log line is exactly where the propagated trace ID
// should surface.
func TestExecuteRPC_TraceIDSurvivesGRPCMetadataBoundary(t *testing.T) {
	env := newSvcEnv(t)
	srv := env.server(coordinator.CoordinatorConfig{}, nil)

	ctx := env.callerCtx(context.Background(), "trace-prop")
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("weft-trace-id", "test-trace-xyz"))
	ctx = weftsdk.GRPC.Incoming(ctx)

	req := &pb.CoordinatorExecuteRequest{
		Pipeline: "(", // deliberately unparseable — fails fast, no shard/compute needed
	}

	stream := &traceCaptureStream{ctx: ctx}
	out := captureStdLog(t, func() {
		if err := srv.Execute(req, stream); err == nil {
			t.Fatal("expected Execute to error on an unparseable pipeline")
		}
	})

	if !strings.Contains(out, `"trace_id":"test-trace-xyz"`) {
		t.Fatalf("expected the propagated trace ID in the error log; got %q", out)
	}
}
