package coordinator

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/dlq"
	"github.com/w3ft-net/weft/compute"
	"github.com/w3ft-net/weft/logdb"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/shape/shapetest"
	"github.com/w3ft-net/weft/shard"
)

// memDLQ is an in-memory dlq.Store that remembers every batch written to
// it, so a test can see what the drain dead-lettered and what a replay left.
type memDLQ struct {
	mu      sync.Mutex
	written []memBatch // every Write, kept
	held    []memBatch // written and not yet replayed
}

type memBatch struct {
	added shape.Added
	rows  []*pb.LogRow
}

func (m *memDLQ) Write(_ context.Context, added shape.Added, rows []*pb.LogRow) (string, error) {
	if len(rows) == 0 {
		return "", nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b := memBatch{added, rows}
	m.written = append(m.written, b)
	m.held = append(m.held, b)
	return fmt.Sprintf("mem/%d", len(m.written)), nil
}

func (m *memDLQ) Replay(_ context.Context, sink func([]dlq.Entry)) (int, error) {
	m.mu.Lock()
	batches := m.held
	m.mu.Unlock()
	total := 0
	for _, b := range batches {
		entries := make([]dlq.Entry, len(b.rows))
		for i, r := range b.rows {
			entries[i] = dlq.Entry{LogRow: r, Added: b.added}
		}
		sink(entries)
		total += len(entries)
		m.mu.Lock()
		m.held = m.held[1:]
		m.mu.Unlock()
	}
	return total, nil
}

// Calls is every batch written, however many were replayed since.
func (m *memDLQ) Calls() []memBatch {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]memBatch(nil), m.written...)
}

// Live is how many batches are held and not yet replayed.
func (m *memDLQ) Live() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.held)
}

// testDLQ is an empty in-memory store, as the Store the coordinator takes and
// as the memDLQ a test inspects.
func testDLQ() (dlq.Store, *memDLQ) {
	m := &memDLQ{}
	return m, m
}

// dlqFailingShard is a shard.Shard whose Ingest always errors. Kept
// local to this test file — its only consumer is the DLQ drain
// coverage below.
type dlqFailingShard struct{ id string }

func (s *dlqFailingShard) ID() string { return s.id }
func (s *dlqFailingShard) Query(context.Context, string, ...any) ([]*pb.ResultRow, error) {
	return nil, nil
}
func (s *dlqFailingShard) QueryToSink(context.Context, string, []any, shard.RowSink) {}
func (s *dlqFailingShard) Archive(_ context.Context, _ string) (string, []shard.PartitionStat, error) {
	return "", nil, nil
}
func (s *dlqFailingShard) Close() error { return nil }
func (s *dlqFailingShard) Ingest(_ context.Context, _ []*pb.LogRow, _ shard.IngestRecorder) error {
	return fmt.Errorf("shard %s down", s.id)
}

// dlqAdded is the added columns of the rows dlqTestRows makes: customer 1. The
// LogRows themselves carry no customer_id.
var dlqAdded = shapetest.Added(1)

// dlqIngestRows is dlqTestRows as the coordinator holds them.
func dlqIngestRows(app string, n int) []Row { return RowsOf(dlqAdded, dlqTestRows(app, n)) }

func dlqTestRows(app string, n int) []*pb.LogRow {
	rows := make([]*pb.LogRow, n)
	now := time.Now().UnixNano()
	ns, svc, kind := shard.SplitApp(app)
	for i := range rows {
		rows[i] = &pb.LogRow{
			Namespace: ns,
			Service:   svc,
			Kind:      kind,
			Raw:       fmt.Sprintf("row-%d", i),
			TsUnixNs:  now,
		}
	}
	return rows
}

// TestDrain_NoHealthyShards_RoutesToDLQ verifies the drain loop's
// "no candidate at all" branch: with the registry empty, every batch
// pulled off the ring buffer lands in the DLQ.
func TestDrain_NoHealthyShards_RoutesToDLQ(t *testing.T) {
	dlq, putter := testDLQ()
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, DLQ: dlq})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.StartDrain(ctx)

	c.Ingest.Write(dlqIngestRows("weft/coordinator", 4))

	if err := waitFor(500*time.Millisecond, func() bool {
		return len(putter.Calls()) >= 1
	}); err != nil {
		t.Fatalf("DLQ never received the batch: %v", err)
	}

	rows := putter.Calls()[0].rows
	if len(rows) != 4 {
		t.Errorf("expected 4 rows in DLQ, got %d", len(rows))
	}
}

// TestDrain_PrimaryAndFallbackFail_RoutesToDLQ verifies the post-retry
// DLQ branch: with one failing shard (primary and fallback resolve to
// the same instance), every Ingest attempt errors and the batch lands
// in the DLQ.
func TestDrain_PrimaryAndFallbackFail_RoutesToDLQ(t *testing.T) {
	dlq, putter := testDLQ()
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, DLQ: dlq})
	// Apps must include the row's app, otherwise IngestingShard
	// returns nil and we'd hit the "no shard owns this app" branch
	// instead of the primary+fallback-fail one we're pinning here.
	e := &logdb.Shard{
		ShardRow: logdb.ShardRow{
			ID:        "shard-down",
			State:     logdb.StateIngesting,
			CreatedAt: time.Now(),
		},
		Live: &dlqFailingShard{id: "shard-down"},
	}
	logdb.SeedShard(c.Provisioner, e)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.StartDrain(ctx)

	c.Ingest.Write(dlqIngestRows("weft/coordinator", 5))

	if err := waitFor(500*time.Millisecond, func() bool {
		return len(putter.Calls()) >= 1
	}); err != nil {
		t.Fatalf("DLQ never received the batch: %v", err)
	}

	rows := putter.Calls()[0].rows
	if len(rows) != 5 {
		t.Errorf("expected 5 rows in DLQ, got %d", len(rows))
	}
	if rows[0].Service != "coordinator" {
		t.Errorf("wrong app in DLQ record: %v", rows[0])
	}
}

// waitFor polls cond until it returns true or the timeout elapses.
// Used to wait for the drain goroutine to do its work without a fixed
// sleep — keeps the test fast on a hot machine and tolerant on a busy
// one.
func waitFor(timeout time.Duration, cond func() bool) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	if cond() {
		return nil
	}
	return fmt.Errorf("condition not met within %s", timeout)
}

func (s *dlqFailingShard) IngestBatch(ctx context.Context, b shard.Batch, rec shard.IngestRecorder) error {
	return s.Ingest(ctx, b.Rows, rec)
}
