package coordinator

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kevlarcon/weft/logdb"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/store"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// failingShard is a shard.Shard that fails Ingest calls on demand.
type failingShard struct {
	id       string
	mu       sync.Mutex
	failing  bool
	ingested []*pb.LogRow
}

func (s *failingShard) ID() string { return s.id }
func (s *failingShard) Query(sql string, args ...any) ([]*pb.ResultRow, error) {
	return nil, nil
}
func (s *failingShard) QueryToSink(sql string, args []any, sink shard.RowSink) {}
func (s *failingShard) Close() error                                           { return nil }
func (s *failingShard) Archive(_ context.Context, _ string) (string, error)    { return "", nil }

func (s *failingShard) Ingest(rows []*pb.LogRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing {
		return fmt.Errorf("shard %s is down", s.id)
	}
	s.ingested = append(s.ingested, rows...)
	return nil
}

func (s *failingShard) setFailing(f bool) {
	s.mu.Lock()
	s.failing = f
	s.mu.Unlock()
}

func (s *failingShard) ingestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.ingested)
}

// testDLQ records DLQ writes in memory instead of S3.
type testDLQ struct {
	mu      sync.Mutex
	batches [][]*pb.LogRow
}

func (d *testDLQ) Write(rows []*pb.LogRow) string {
	if d == nil || len(rows) == 0 {
		return ""
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.batches = append(d.batches, rows)
	return fmt.Sprintf("dlq/test/%d.jsonl.gz", len(d.batches))
}

func (d *testDLQ) totalRows() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, b := range d.batches {
		n += len(b)
	}
	return n
}

func testRows(app, customer string, n int) []*pb.LogRow {
	rows := make([]*pb.LogRow, n)
	for i := range rows {
		rows[i] = &pb.LogRow{
			App:      app,
			Customer: customer,
			Raw:      fmt.Sprintf("test row %d", i),
			TsUnixNs: time.Now().UnixNano(),
		}
	}
	return rows
}

// ---------------------------------------------------------------------------
// Tests: Suspend on ingest failure
// ---------------------------------------------------------------------------

func TestSuspend_OnIngestFailure(t *testing.T) {
	shardA := &failingShard{id: "shard-a"}
	shardB := &failingShard{id: "shard-b"}

	reg := logdb.NewShardRegistry()
	reg.Add(&logdb.ShardEntry{
		ID: "shard-a", State: logdb.StateIngesting, Shard: shardA, CreatedAt: time.Now(),
	})
	reg.Add(&logdb.ShardEntry{
		ID: "shard-b", State: logdb.StateIngesting, Shard: shardB, CreatedAt: time.Now(),
	})

	// shard-a will fail.
	shardA.setFailing(true)

	c := &Coordinator{
		Registry: reg,
		Ingest:   NewRingBuffer(1000),
		SeenApps: NewSeenApps(),
	}

	// Start drain first, then write multiple batches so round-robin
	// hits both shards. Each batch is a separate ring buffer write.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.StartDrain(ctx)
	time.Sleep(50 * time.Millisecond)

	for i := 0; i < 10; i++ {
		c.Ingest.Write(testRows("weft/coordinator", "graybeard", 1))
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)

	// shard-a should be suspended (it failed on at least one batch).
	entryA := reg.Get("shard-a")
	if !entryA.Suspended {
		t.Error("shard-a should be suspended after ingest failure")
	}

	// shard-b should NOT be suspended.
	entryB := reg.Get("shard-b")
	if entryB.Suspended {
		t.Error("shard-b should not be suspended")
	}

	// shard-b should have received rows (direct + retry fallback).
	if shardB.ingestCount() == 0 {
		t.Error("shard-b should have received rows")
	}
}

func TestSuspend_RequiresConsecutiveFailures(t *testing.T) {
	// One flaky RPC shouldn't suspend a shard — that was the bug that caused
	// 2026-04-17's shard-count inflation (intermittent NLB-loop heartbeat
	// failures → immediate suspend → provisioner replaces → orphan
	// accumulation). Require a consistent string of failures instead.
	//
	// Drive the registry's ingest-failure counter directly rather than going
	// through StartDrain; the drain loop's routing to *other* shards on
	// failure makes it hard to deterministically deliver N consecutive
	// attempts to the same shard in a test.
	reg := logdb.NewShardRegistry()
	reg.Add(&logdb.ShardEntry{
		ID: "shard-a", State: logdb.StateIngesting, CreatedAt: time.Now(),
	})

	// First failure: recorded, but below threshold.
	reg.RecordIngestFailure("shard-a")
	if reg.Get("shard-a").Suspended {
		t.Fatal("one failure should not suspend")
	}

	// A successful ingest resets the counter.
	reg.ResetIngestFailures("shard-a")

	// Below-threshold run of consecutive failures — still healthy.
	for i := 0; i < logdb.SuspendFailureThreshold-1; i++ {
		reg.RecordIngestFailure("shard-a")
	}
	if reg.Get("shard-a").Suspended {
		t.Fatalf("suspended after only %d failures (threshold is %d)",
			logdb.SuspendFailureThreshold-1, logdb.SuspendFailureThreshold)
	}

	// One more failure trips the threshold.
	reg.RecordIngestFailure("shard-a")
	if !reg.Get("shard-a").Suspended {
		t.Fatalf("should be suspended after %d failures", logdb.SuspendFailureThreshold)
	}
}

func TestSuspend_AllShardsDown_DLQ(t *testing.T) {
	shardA := &failingShard{id: "shard-a", failing: true}

	reg := logdb.NewShardRegistry()
	reg.Add(&logdb.ShardEntry{
		ID: "shard-a", State: logdb.StateIngesting, Shard: shardA, CreatedAt: time.Now(),
	})

	dlq := &testDLQ{}
	c := &Coordinator{
		Registry: reg,
		Ingest:   NewRingBuffer(1000),
		SeenApps: NewSeenApps(),
	}
	// Inject test DLQ — the real DLQ is *DLQ but we need an interface here.
	// For now, test that the drain loop suspends and we can verify the behavior.

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.StartDrain(ctx)
	time.Sleep(50 * time.Millisecond)

	// Drive enough consecutive failures past the threshold so the shard
	// transitions to Suspended. Each Write triggers one drain cycle →
	// one failed Ingest call → one RecordIngestFailure.
	for i := 0; i < logdb.SuspendFailureThreshold; i++ {
		c.Ingest.Write(testRows("weft/shard", "graybeard", 5))
		time.Sleep(120 * time.Millisecond)
	}

	// shard-a should be suspended.
	if !reg.Get("shard-a").Suspended {
		t.Error("shard-a should be suspended")
	}

	// With only one shard and it's suspended, IngestingShard returns nil.
	if sh := reg.IngestingShard("weft/shard"); sh != nil {
		t.Error("IngestingShard should return nil when all shards suspended")
	}

	_ = dlq // DLQ integration tested separately with S3/minio
}

// ---------------------------------------------------------------------------
// Tests: Unsuspend on heartbeat
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Tests: Reconnect on address change
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Tests: Reap dead shards
// ---------------------------------------------------------------------------

// recordingLauncher captures TerminateInstance / TerminateForReplacement
// calls for assertions. Satisfies logdb.ShardLauncher and
// logdb.RecoveryTerminator; LaunchInstance is a no-op.
type recordingLauncher struct {
	terminated           []string // addr passed to TerminateInstance (ASG decrement semantics)
	terminatedForReplace []string // instance id passed to TerminateForReplacement (ASG replace semantics)
	replacementErr       error
}

func (r *recordingLauncher) LaunchInstance(_ context.Context, _ string) (string, error) {
	return "", nil
}

func (r *recordingLauncher) TerminateInstance(_ context.Context, _, addr string) error {
	r.terminated = append(r.terminated, addr)
	return nil
}

func (r *recordingLauncher) TerminateForReplacement(_ context.Context, instanceID string) error {
	r.terminatedForReplace = append(r.terminatedForReplace, instanceID)
	return r.replacementErr
}

// ---------------------------------------------------------------------------
// Tests: Suspended shards excluded from write routing but included in queries
// ---------------------------------------------------------------------------

func TestSuspended_ExcludedFromWrites_IncludedInQueries(t *testing.T) {
	ds, err := shard.NewDuckDBShard("shard-0", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })

	// Ingest some data so queries return results.
	ds.Ingest(testRows("vimgym", "kevlar", 5))

	reg := logdb.NewShardRegistry()
	reg.Add(&logdb.ShardEntry{
		ID: "shard-0", Apps: []string{"vimgym"},
		State: logdb.StateIngesting, Shard: ds,
		Suspended: true, WindowStart: t0, WindowEnd: t2, CreatedAt: time.Now(),
	})

	// Write routing should skip it.
	if sh := reg.IngestingShard("vimgym"); sh != nil {
		t.Error("IngestingShard should skip suspended shard")
	}
	if sh := reg.DefaultIngestingShard(); sh != nil {
		t.Error("DefaultIngestingShard should skip suspended shard")
	}

	// Query routing should include it.
	matching := reg.MatchingEntries(fullRange, "vimgym", "bolt")
	if len(matching) != 1 {
		t.Errorf("MatchingEntries should include suspended shard, got %d", len(matching))
	}
}

// ---------------------------------------------------------------------------
// Mock store for heartbeat tests
// ---------------------------------------------------------------------------

// mockHeartbeatStore implements the subset of store.Store used by the provisioner.
// It satisfies the provisioner's config.Store field via a wrapper.
type mockHeartbeatStore struct {
	heartbeats []store.ShardHeartbeat
}

func (m *mockHeartbeatStore) LiveShardHeartbeats(since string) ([]store.ShardHeartbeat, error) {
	return m.heartbeats, nil
}

func (m *mockHeartbeatStore) InsertHeartbeat(hb *store.ShardHeartbeat) error {
	return nil
}
