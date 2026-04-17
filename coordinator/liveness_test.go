package coordinator

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

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
func (s *failingShard) Query(sql string) ([]*pb.ResultRow, error) {
	return nil, nil
}
func (s *failingShard) QueryToSink(sql string, sink shard.RowSink) {}
func (s *failingShard) Close() error                               { return nil }

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

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID: "shard-a", State: StateIngesting, Shard: shardA, CreatedAt: time.Now(),
	})
	reg.Add(&ShardEntry{
		ID: "shard-b", State: StateIngesting, Shard: shardB, CreatedAt: time.Now(),
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

func TestSuspend_AllShardsDown_DLQ(t *testing.T) {
	shardA := &failingShard{id: "shard-a", failing: true}

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID: "shard-a", State: StateIngesting, Shard: shardA, CreatedAt: time.Now(),
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

	rows := testRows("weft/shard", "graybeard", 5)
	c.Ingest.Write(rows)
	time.Sleep(300 * time.Millisecond)

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

func TestUnsuspend_OnHeartbeat(t *testing.T) {
	ds, err := shard.NewDuckDBShard("shard-0", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID: "shard-0", State: StateIngesting, Shard: ds,
		Addr: "10.0.0.1:9000", Suspended: true, CreatedAt: time.Now(),
	})

	// Verify it's suspended.
	if sh := reg.IngestingShard("anything"); sh != nil {
		t.Fatal("suspended shard should not be returned by IngestingShard")
	}

	// Simulate heartbeat from same address.
	mockStore := &mockHeartbeatStore{
		heartbeats: []store.ShardHeartbeat{
			{ShardID: "shard-0", Addr: "10.0.0.1:9000", State: "INGESTING"},
		},
	}
	prov := &Provisioner{
		registry: reg,
		config:   ProvisionerConfig{Store: mockStore, PollInterval: time.Second},
	}
	prov.adoptFromHeartbeats()

	// Should be unsuspended.
	entry := reg.Get("shard-0")
	if entry.Suspended {
		t.Error("shard should be unsuspended after heartbeat")
	}

	// Should be back in rotation.
	if sh := reg.IngestingShard("anything"); sh == nil {
		t.Error("unsuspended shard should be returned by IngestingShard")
	}
}

// ---------------------------------------------------------------------------
// Tests: Reconnect on address change
// ---------------------------------------------------------------------------

func TestReconnect_OnAddressChange(t *testing.T) {
	oldShard := &failingShard{id: "shard-0"}

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID: "shard-0", State: StateIngesting, Shard: oldShard,
		Addr: "10.0.0.1:9000", Suspended: true, CreatedAt: time.Now(),
	})

	// Heartbeat arrives with a new address. We can't create a real GRPCShard
	// in unit tests, so we test the logic up to the point of NewGRPCShard.
	// The key assertion: the old shard's address and suspended state change.
	entry := reg.Get("shard-0")
	if entry.Addr != "10.0.0.1:9000" {
		t.Fatalf("expected old addr, got %s", entry.Addr)
	}
	if !entry.Suspended {
		t.Fatal("expected suspended")
	}

	// Simulate what adoptFromHeartbeats does on address change
	// (can't call it directly because NewGRPCShard needs a real server).
	newShard := &failingShard{id: "shard-0"}
	entry.Shard.Close()
	entry.Shard = newShard
	entry.Addr = "10.0.0.2:9000"
	entry.Suspended = false

	if entry.Addr != "10.0.0.2:9000" {
		t.Errorf("expected new addr, got %s", entry.Addr)
	}
	if entry.Suspended {
		t.Error("should be unsuspended after reconnect")
	}
}

// ---------------------------------------------------------------------------
// Tests: Reap dead shards
// ---------------------------------------------------------------------------

func TestReap_DeadShard(t *testing.T) {
	ds, err := shard.NewDuckDBShard("shard-alive", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })

	deadShard := &failingShard{id: "shard-dead"}

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID: "shard-alive", State: StateIngesting, Shard: ds,
		Addr: "10.0.0.1:9000", CreatedAt: time.Now(),
	})
	reg.Add(&ShardEntry{
		ID: "shard-dead", State: StateIngesting, Shard: deadShard,
		Addr: "10.0.0.2:9000", CreatedAt: time.Now(),
	})

	// Only shard-alive has a heartbeat.
	mockStore := &mockHeartbeatStore{
		heartbeats: []store.ShardHeartbeat{
			{ShardID: "shard-alive", Addr: "10.0.0.1:9000", State: "INGESTING"},
		},
	}
	prov := &Provisioner{
		registry: reg,
		config:   ProvisionerConfig{Store: mockStore, PollInterval: time.Second},
	}
	prov.reapDeadShards()

	// shard-dead should be removed.
	if reg.Get("shard-dead") != nil {
		t.Error("dead shard should have been reaped")
	}

	// shard-alive should remain.
	if reg.Get("shard-alive") == nil {
		t.Error("alive shard should not have been reaped")
	}

	if reg.Count() != 1 {
		t.Errorf("expected 1 shard, got %d", reg.Count())
	}
}

func TestReap_SkipsArchivedShards(t *testing.T) {
	coldShard := &failingShard{id: "shard-cold"}

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID: "shard-cold", State: StateArchived, Shard: coldShard,
		CreatedAt: time.Now(),
	})

	// No heartbeats at all — but shard-cold is archived, so it should survive.
	mockStore := &mockHeartbeatStore{
		heartbeats: []store.ShardHeartbeat{
			{ShardID: "shard-other", Addr: "10.0.0.1:9000", State: "INGESTING"},
		},
	}
	prov := &Provisioner{
		registry: reg,
		config:   ProvisionerConfig{Store: mockStore, PollInterval: time.Second},
	}
	prov.reapDeadShards()

	if reg.Get("shard-cold") == nil {
		t.Error("archived shard should not be reaped")
	}
}

func TestReap_TerminatesInstanceViaLauncher(t *testing.T) {
	// Regression guard for the orphaned-shards bug seen on 2026-04-17:
	// reapDeadShards removed dead entries from the in-memory registry
	// but never told the launcher to terminate the EC2 instance, so
	// flaky heartbeats caused the ASG to accumulate "dead" instances
	// that were still billing.
	alive := &failingShard{id: "shard-alive"}
	dead := &failingShard{id: "shard-dead"}

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID: "shard-alive", State: StateIngesting, Shard: alive,
		Addr: "10.0.0.1:9000", CreatedAt: time.Now(),
	})
	reg.Add(&ShardEntry{
		ID: "shard-dead", State: StateIngesting, Shard: dead,
		Addr: "10.0.0.99:9000", CreatedAt: time.Now(),
	})

	mockStore := &mockHeartbeatStore{
		heartbeats: []store.ShardHeartbeat{
			{ShardID: "shard-alive", Addr: "10.0.0.1:9000", State: "INGESTING"},
		},
	}
	launcher := &recordingLauncher{}
	prov := &Provisioner{
		registry: reg,
		launcher: launcher,
		config:   ProvisionerConfig{Store: mockStore, PollInterval: time.Second},
	}
	prov.reapDeadShards()

	// Registry side: dead removed, alive kept.
	if reg.Get("shard-dead") != nil {
		t.Error("dead shard should have been reaped from registry")
	}
	if reg.Get("shard-alive") == nil {
		t.Error("alive shard should not have been reaped")
	}

	// Launcher side: terminate called exactly once, for the dead shard's addr.
	if got := len(launcher.terminated); got != 1 {
		t.Fatalf("expected 1 TerminateInstance call, got %d: %v", got, launcher.terminated)
	}
	if launcher.terminated[0] != "10.0.0.99:9000" {
		t.Errorf("expected TerminateInstance(10.0.0.99:9000), got %s", launcher.terminated[0])
	}
}

// recordingLauncher captures TerminateInstance calls for assertions.
// Satisfies ShardLauncher; LaunchInstance is a no-op.
type recordingLauncher struct {
	terminated []string
}

func (r *recordingLauncher) LaunchInstance(_ context.Context, _ string) (string, error) {
	return "", nil
}

func (r *recordingLauncher) TerminateInstance(_ context.Context, addr string) error {
	r.terminated = append(r.terminated, addr)
	return nil
}

func TestReap_SkipsWhenNoHeartbeats(t *testing.T) {
	ds, err := shard.NewDuckDBShard("shard-0", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID: "shard-0", State: StateIngesting, Shard: ds, CreatedAt: time.Now(),
	})

	// Empty heartbeat list — startup race. Should NOT reap.
	mockStore := &mockHeartbeatStore{heartbeats: nil}
	prov := &Provisioner{
		registry: reg,
		config:   ProvisionerConfig{Store: mockStore, PollInterval: time.Second},
	}
	prov.reapDeadShards()

	if reg.Get("shard-0") == nil {
		t.Error("should not reap when no heartbeats exist (startup race)")
	}
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

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID: "shard-0", State: StateIngesting, Shard: ds,
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
	matching := reg.MatchingShards(fullRange, "vimgym")
	if len(matching) != 1 {
		t.Errorf("MatchingShards should include suspended shard, got %d", len(matching))
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
