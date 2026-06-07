package service

import (
	"context"
	"io"
	"runtime"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/graybeardcorp/weft/coordinator"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// fakeIngestStream is a minimal grpc.ClientStreamingServer[LogRow,
// IngestStreamResponse] that replays a fixed slice of rows and then
// reports io.EOF — i.e. a well-behaved agent that opens a stream, ships
// a few rows, and disconnects. Every weftd reconnect is one of these.
type fakeIngestStream struct {
	ctx  context.Context
	rows []*pb.LogRow
	idx  int
}

func (f *fakeIngestStream) Recv() (*pb.LogRow, error) {
	if f.idx >= len(f.rows) {
		return nil, io.EOF
	}
	r := f.rows[f.idx]
	f.idx++
	return r, nil
}

func (f *fakeIngestStream) SendAndClose(*pb.IngestStreamResponse) error { return nil }
func (f *fakeIngestStream) Context() context.Context                    { return f.ctx }
func (f *fakeIngestStream) SetHeader(metadata.MD) error                 { return nil }
func (f *fakeIngestStream) SendHeader(metadata.MD) error                { return nil }
func (f *fakeIngestStream) SetTrailer(metadata.MD)                      {}
func (f *fakeIngestStream) SendMsg(any) error                           { return nil }
func (f *fakeIngestStream) RecvMsg(any) error                           { return nil }

// settleGoroutines polls runtime.NumGoroutine for up to ~3s and returns
// the lowest count observed minus baseline. Taking the minimum filters
// out transient goroutines (gRPC bookkeeping, pg pool churn) that are on
// their way out — what's left is the genuinely-stuck residue.
func settleGoroutines(baseline int) int {
	min := runtime.NumGoroutine() - baseline
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
		if d := runtime.NumGoroutine() - baseline; d < min {
			min = d
		}
		if min <= 0 {
			break
		}
	}
	return min
}

// TestIngestStream_NoGoroutineLeak pins the coordinator memory leak: each
// IngestStream call starts a periodic-flush goroutine (`for range
// ticker.C`) that the deferred ticker.Stop() does NOT terminate —
// Stop() halts the ticker but never closes its channel, so the range
// blocks forever. Every agent reconnect therefore strands one goroutine
// (and the closure state it pins), which is the ~250-goroutine/hour
// linear growth seen on the memory-pressure-coordinator dashboard.
//
// With the bug, driving N streams to clean EOF leaves ~N goroutines
// alive. Fixed, the flush goroutine exits when the handler returns and
// the count settles back to baseline.
func TestIngestStream_NoGoroutineLeak(t *testing.T) {
	tx, cleanup := testStore(t)
	defer cleanup()

	cust, err := tx.FindOrCreateCustomer("kevlar")
	if err != nil {
		t.Fatal(err)
	}
	tx.SeedDevSubscription(cust.ID)
	ns, _ := tx.FindOrCreateNamespace(cust.ID, "leaktest")
	rawTok, _, err := tx.CreateAgentToken(cust.ID, ns.ID, "leak-agent", "")
	if err != nil {
		t.Fatal(err)
	}

	coord := coordinator.NewCoordinator(coordinator.CoordinatorConfig{Store: tx})
	srv := &Server{Coord: coord, Store: tx}

	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer "+rawTok))

	runOne := func() {
		st := &fakeIngestStream{
			ctx:  ctx,
			rows: []*pb.LogRow{{Raw: "hello"}},
		}
		if err := srv.IngestStream(st); err != nil {
			t.Fatalf("IngestStream: %v", err)
		}
	}

	// Warm up so lazily-created singletons (pools, package-level state)
	// don't count against the baseline.
	runOne()
	settleGoroutines(runtime.NumGoroutine())
	baseline := runtime.NumGoroutine()

	const n = 25
	for i := 0; i < n; i++ {
		runOne()
	}

	leaked := settleGoroutines(baseline)
	if leaked > 5 {
		t.Fatalf("IngestStream leaked ~%d goroutines across %d streams (want ≈0); "+
			"the per-stream flush goroutine never exits because the deferred "+
			"ticker.Stop() does not close ticker.C, so `for range ticker.C` blocks forever",
			leaked, n)
	}
}
