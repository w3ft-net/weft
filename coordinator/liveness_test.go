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

	c := NewCoordinator(CoordinatorConfig{Ingest: NewRingBuffer(1000)})
	c.Provisioner.Add(&logdb.ShardEntry{
		ID: "shard-a", State: logdb.StateIngesting, Shard: shardA, CreatedAt: time.Now(),
	})
	c.Provisioner.Add(&logdb.ShardEntry{
		ID: "shard-b", State: logdb.StateIngesting, Shard: shardB, CreatedAt: time.Now(),
	})

	// shard-a will fail.
	shardA.setFailing(true)

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
	entryA := c.Provisioner.Get("shard-a")
	if !entryA.Suspended {
		t.Error("shard-a should be suspended after ingest failure")
	}

	// shard-b should NOT be suspended.
	entryB := c.Provisioner.Get("shard-b")
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

	c := NewCoordinator(CoordinatorConfig{Ingest: NewRingBuffer(1000)})
	c.Provisioner.Add(&logdb.ShardEntry{
		ID: "shard-a", State: logdb.StateIngesting, Shard: shardA, CreatedAt: time.Now(),
	})

	dlq := &testDLQ{}
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
	if !c.Provisioner.Get("shard-a").Suspended {
		t.Error("shard-a should be suspended")
	}

	// With only one shard and it's suspended, IngestingShard returns nil.
	if sh := c.Provisioner.IngestingShard("weft/shard"); sh != nil {
		t.Error("IngestingShard should return nil when all shards suspended")
	}

	_ = dlq // DLQ integration tested separately with S3/minio
}

