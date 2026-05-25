package coordinator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kevlarcon/weft/compute"
	"github.com/kevlarcon/weft/planner"
	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// TestExecuteFollow_HistoricalThenBroker pins the splice contract:
// ExecuteFollow's stream emits the historical batch first; once the
// historical exhausts the broker takes over and live rows broadcast
// at runtime arrive on the same stream.
func TestExecuteFollow_HistoricalThenBroker(t *testing.T) {
	c := setupTestCoordinator(t)
	c.Tail = NewTailBroker()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// `tail vimgym.log` (no -f in the pipeline — ExecuteFollow's own
	// follow flag drives live tailing) to keep the historical batch
	// small and predictable.
	res, err := c.ExecuteFollow(ctx, "tail vimgym.log", fullRange, "kevlar", "")
	if err != nil {
		t.Fatalf("ExecuteFollow: %v", err)
	}

	// Drain historical lines. Each Next call has a short timeout —
	// once historical exhausts and the concat stream switches to the
	// broker, Next blocks until a Broadcast arrives. Loop until we hit
	// that timeout, treating it as the end of the historical phase.
	historical := drainHistorical(t, res.Stream, 500*time.Millisecond)
	if len(historical) == 0 {
		t.Fatal("historical phase emitted zero lines; ExecuteFollow stream is broken")
	}

	// Broadcast a live row that matches the follow's app filter
	// (vimgym). The brokerStream's filter is BuildLiveFilter(plan),
	// which for `tail vimgym.log` is a passthrough scoped to vimgym.
	c.Tail.Broadcast([]*pb.LogRow{{
		Raw:       "2026-03-28T12:00:00 INFO vimgym i-001 live broadcast",
		Namespace: "vimgym",
		Customer:  "kevlar",
	}})

	// Read one more line — should be the broadcast row.
	readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
	defer readCancel()
	line, ok, err := res.Stream.Next(readCtx)
	if err != nil {
		t.Fatalf("Next after broadcast: %v", err)
	}
	if !ok {
		t.Fatal("stream EOF before broadcast row arrived")
	}
	if line.Raw != "2026-03-28T12:00:00 INFO vimgym i-001 live broadcast" {
		t.Errorf("broker row Raw = %q, want the broadcast we sent", line.Raw)
	}
}

// TestExecuteFollow_NoBroker_Errors guards the precondition: if the
// coordinator has no Tail broker configured, ExecuteFollow must
// reject the call rather than return a stream that silently drops
// every live row.
func TestExecuteFollow_NoBroker_Errors(t *testing.T) {
	c := setupTestCoordinator(t)
	c.Tail = nil

	_, err := c.ExecuteFollow(context.Background(), "tail vimgym.log", fullRange, "kevlar", "")
	if err == nil {
		t.Fatal("expected error when Tail is nil")
	}
}

// TestExecuteFollow_NonLogPipeline_Rejected: ExecuteFollow can only
// route plannable log queries (the broker subscribes to log shards;
// non-log shapes have nothing to follow). A bare `echo hi` should
// surface ErrNotPlannable rather than wedging on a broker subscription
// that nothing will broadcast to.
func TestExecuteFollow_NonLogPipeline_Rejected(t *testing.T) {
	c := setupTestCoordinator(t)
	c.Tail = NewTailBroker()

	_, err := c.ExecuteFollow(context.Background(), "echo hi", fullRange, "kevlar", "")
	if !errors.Is(err, planner.ErrNotPlannable) {
		t.Fatalf("err = %v, want ErrNotPlannable", err)
	}
}

// drainHistorical reads stream until Next blocks for `quiet` without
// producing a line — that's the signal that the historical phase
// drained and the concat stream is now waiting on the broker.
func drainHistorical(t *testing.T, stream compute.LineStream, quiet time.Duration) []string {
	t.Helper()
	var out []string
	for {
		ctx, cancel := context.WithTimeout(context.Background(), quiet)
		line, ok, err := stream.Next(ctx)
		cancel()
		if err != nil {
			// Timeout = historical drained; broker is waiting.
			if errors.Is(err, context.DeadlineExceeded) {
				return out
			}
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			return out
		}
		out = append(out, line.Raw)
	}
}
