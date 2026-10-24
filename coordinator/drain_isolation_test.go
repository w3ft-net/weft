package coordinator

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/logdb"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
	"github.com/graybeardcorp/weft/store"
)

// heldShard is a shard whose Ingest blocks until released: a shard
// paused mid-upgrade, whose client holds the call until the successor
// opens.
type heldShard struct {
	dlqFailingShard
	release chan struct{}
	mu      sync.Mutex
	rows    int
}

func (s *heldShard) Ingest(ctx context.Context, rows []*pb.LogRow, _ shard.IngestRecorder) error {
	if s.release != nil {
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.mu.Lock()
	s.rows += len(rows)
	s.mu.Unlock()
	return nil
}

func (s *heldShard) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows
}

// A held Ingest (one shard paused) must stall only that shard's rows:
// rows for a service on another shard keep landing.
func TestDrain_APausedShardHoldsOnlyItsOwnRows(t *testing.T) {
	dlq, putter := testDLQ()
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, DLQ: dlq})
	paused := &heldShard{dlqFailingShard: dlqFailingShard{id: "shard-paused"}, release: make(chan struct{})}
	healthy := &heldShard{dlqFailingShard: dlqFailingShard{id: "shard-healthy"}}
	for _, s := range []struct {
		sh  *heldShard
		app string
	}{{paused, "weft/held"}, {healthy, "weft/flowing"}} {
		_, svc, _ := shard.SplitApp(s.app)
		logdb.SeedShard(c.Provisioner, &logdb.Shard{ID: s.sh.id, State: logdb.StateIngesting, Live: s.sh, CreatedAt: time.Now(),
			Services: []store.ShardService{{ShardID: s.sh.id, Service: svc}}})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.StartDrain(ctx)

	c.Ingest.Write(dlqTestRows("weft/held", 3))
	time.Sleep(50 * time.Millisecond) // the drain is now inside the held Ingest
	c.Ingest.Write(dlqTestRows("weft/flowing", 4))

	if err := waitFor(2*time.Second, func() bool { return healthy.count() == 4 }); err != nil {
		t.Fatalf("rows for the healthy shard stalled behind the paused one (landed %d of 4)", healthy.count())
	}
	close(paused.release)
	if err := waitFor(2*time.Second, func() bool { return paused.count() == 3 }); err != nil {
		t.Fatalf("the paused shard's rows never landed after release (landed %d of 3)", paused.count())
	}
	if n := len(putter.Calls()); n != 0 {
		t.Fatalf("%d batches went to the DLQ; a hold is not a failure", n)
	}
}

// Rows for one shard land in the order they were written, even with a
// per-shard send path.
func TestDrain_KeepsPerShardOrder(t *testing.T) {
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}})
	rec := &orderShard{dlqFailingShard: dlqFailingShard{id: "shard-order"}}
	logdb.SeedShard(c.Provisioner, &logdb.Shard{ID: rec.id, State: logdb.StateIngesting, Live: rec, CreatedAt: time.Now(),
		Services: []store.ShardService{{ShardID: rec.id, Service: "order"}}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.StartDrain(ctx)
	for i := 0; i < 20; i++ {
		rows := dlqTestRows("weft/order", 1)
		rows[0].Raw = string(rune('a' + i))
		c.Ingest.Write(rows)
	}
	if err := waitFor(2*time.Second, func() bool { return len(rec.seen()) == 20 }); err != nil {
		t.Fatalf("landed %d of 20 rows", len(rec.seen()))
	}
	for i, raw := range rec.seen() {
		if raw != string(rune('a'+i)) {
			t.Fatalf("row %d landed as %q, want %q (order lost)", i, raw, string(rune('a'+i)))
		}
	}
}

type orderShard struct {
	dlqFailingShard
	mu  sync.Mutex
	raw []string
}

func (s *orderShard) Ingest(_ context.Context, rows []*pb.LogRow, _ shard.IngestRecorder) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range rows {
		s.raw = append(s.raw, r.Raw)
	}
	return nil
}

func (s *orderShard) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.raw...)
}

// The ring buffer overwrites its oldest rows when full; every row it
// overwrites must be counted, not lost without a trace.
func TestRingBuffer_CountsOverwrittenRows(t *testing.T) {
	rb := NewRingBuffer(4)
	rb.Write(dlqTestRows("weft/x", 3))
	if got := rb.Dropped(); got != 0 {
		t.Fatalf("dropped %d rows before the buffer was full", got)
	}
	rb.Write(dlqTestRows("weft/x", 3))
	if got := rb.Dropped(); got != 2 {
		t.Fatalf("Dropped() = %d after overflowing a 4-row buffer by 2", got)
	}
}
