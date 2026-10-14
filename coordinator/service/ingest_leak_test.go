package service

import (
	"context"
	"io"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/graybeardcorp/weft/compute"
	weftcfg "github.com/graybeardcorp/weft/config"
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

	cust, err := tx.FindOrCreateOrg("kevlar")
	if err != nil {
		t.Fatal(err)
	}
	tx.SeedDevSubscription(cust.ID)
	ns, _ := tx.FindOrCreateNamespace(cust.ID, "leaktest")
	rawTok, _, err := tx.CreateAgentToken(cust.ID, ns.ID, "leak-agent", "")
	if err != nil {
		t.Fatal(err)
	}

	coord := coordinator.NewCoordinator(coordinator.CoordinatorConfig{Compute: &compute.Engine{}, Store: tx})
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

// abandonedStream models the dominant prod leak: a client opens an
// ingest stream, ships a batch, then goes silent WITHOUT half-closing
// (process gone, NAT/idle drop, or a client that opens a fresh stream
// per batch and forgets the old one). Recv() returns the seeded rows
// and then blocks until block is closed — which, in real gRPC, is what
// happens when the handler returns and the transport tears the stream
// down. Until then the server-side Recv() never unblocks on its own.
type abandonedStream struct {
	ctx   context.Context
	rows  []*pb.LogRow
	idx   int
	block chan struct{}
}

func (f *abandonedStream) Recv() (*pb.LogRow, error) {
	if f.idx < len(f.rows) {
		r := f.rows[f.idx]
		f.idx++
		return r, nil
	}
	<-f.block
	return nil, status.Error(codes.Canceled, "stream closed")
}

func (f *abandonedStream) SendAndClose(*pb.IngestStreamResponse) error { return nil }
func (f *abandonedStream) Context() context.Context                    { return f.ctx }
func (f *abandonedStream) SetHeader(metadata.MD) error                 { return nil }
func (f *abandonedStream) SendHeader(metadata.MD) error                { return nil }
func (f *abandonedStream) SetTrailer(metadata.MD)                      {}
func (f *abandonedStream) SendMsg(any) error                           { return nil }
func (f *abandonedStream) RecvMsg(any) error                           { return nil }

// TestIngestStream_AbandonedStreamReaped pins the dominant coordinator
// leak found in the prod goroutine dump: 259 IngestStream handlers
// parked in stream.Recv() for up to 81 minutes because abandoned streams
// were never reaped. With no idle timeout the handler blocks forever; the
// idle reaper must close the stream and return DeadlineExceeded, after
// which both the handler and its reader/flush goroutines unwind.
func TestIngestStream_AbandonedStreamReaped(t *testing.T) {
	tx, cleanup := testStore(t)
	defer cleanup()

	cust, err := tx.FindOrCreateOrg("kevlar")
	if err != nil {
		t.Fatal(err)
	}
	tx.SeedDevSubscription(cust.ID)
	ns, _ := tx.FindOrCreateNamespace(cust.ID, "leaktest")
	rawTok, _, err := tx.CreateAgentToken(cust.ID, ns.ID, "abandon-agent", "")
	if err != nil {
		t.Fatal(err)
	}

	// 1s idle timeout so the test reaps quickly.
	cfg := weftcfg.Defaults()
	cfg.IngestStreamIdleTimeoutSeconds = 1
	var cfgPtr atomic.Pointer[weftcfg.Runtime]
	cfgPtr.Store(cfg)

	coord := coordinator.NewCoordinator(coordinator.CoordinatorConfig{Compute: &compute.Engine{}, Store: tx})
	srv := &Server{Coord: coord, Store: tx, CfgPtr: &cfgPtr}

	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer "+rawTok))

	baseline := runtime.NumGoroutine()

	st := &abandonedStream{
		ctx:   ctx,
		rows:  []*pb.LogRow{{Raw: "one batch then silence"}},
		block: make(chan struct{}),
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.IngestStream(st) }()

	select {
	case err := <-errCh:
		if status.Code(err) != codes.DeadlineExceeded {
			t.Fatalf("expected DeadlineExceeded from idle reaper, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("IngestStream never returned on an abandoned stream — handler is leaked in stream.Recv()")
	}

	// Real gRPC closes the stream when the handler returns, which unblocks
	// the server-side Recv(); model that so the reader goroutine can exit.
	close(st.block)

	if leaked := settleGoroutines(baseline); leaked > 5 {
		t.Fatalf("abandoned stream leaked ~%d goroutines after reaping (want ~0)", leaked)
	}
}
