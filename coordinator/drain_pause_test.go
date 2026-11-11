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

// countingShard is a shard.Shard that counts the rows it is asked to ingest.
type countingShard struct {
	dlqFailingShard
	mu   sync.Mutex
	rows int
}

func (s *countingShard) Ingest(_ context.Context, rows []*pb.LogRow, _ shard.IngestRecorder) error {
	s.mu.Lock()
	s.rows += len(rows)
	s.mu.Unlock()
	return nil
}

func (s *countingShard) Rows() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows
}

func seedWriter(c *Coordinator, sh *countingShard, state logdb.ShardState, phase logdb.UpgradePhase) {
	e := &logdb.Shard{
		ID:        sh.id,
		State:     state,
		Live:      sh,
		CreatedAt: time.Now(),
		BucketID:  1,
		Services:  []store.ShardService{{ShardID: sh.id, Service: "coordinator"}},
	}
	if state == logdb.StateUpgrading {
		e.UpgradePhase = phase
		e.UpgradeOrigin = logdb.StateIngesting
	}
	logdb.SeedShard(c.Provisioner, e)
}

// writeBatches sends n one-row batches, one at a time, so the drain
// loop round-robins each over the writable candidates.
func writeBatches(c *Coordinator, n int) {
	for i := 0; i < n; i++ {
		c.Ingest.Write(dlqIngestRows("weft/coordinator", 1))
		time.Sleep(2 * time.Millisecond)
	}
}

// A shard mid-upgrade must not take new writes while a sibling in its
// bucket is INGESTING: the sibling absorbs them, so the paused shard's
// senders never stall waiting for the successor to open.
func TestDrain_PausedShardYieldsWritesToSibling(t *testing.T) {
	dlq, putter := testDLQ()
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, DLQ: dlq})
	paused := &countingShard{dlqFailingShard: dlqFailingShard{id: "shard-paused"}}
	sibling := &countingShard{dlqFailingShard: dlqFailingShard{id: "shard-sibling"}}
	seedWriter(c, paused, logdb.StateUpgrading, logdb.PhasePausing)
	seedWriter(c, sibling, logdb.StateIngesting, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.StartDrain(ctx)

	writeBatches(c, 20)

	if err := waitFor(2*time.Second, func() bool { return sibling.Rows() == 20 }); err != nil {
		t.Fatalf("sibling got %d of 20 rows, paused shard got %d", sibling.Rows(), paused.Rows())
	}
	if got := paused.Rows(); got != 0 {
		t.Errorf("paused shard was sent %d rows while a sibling was ingesting", got)
	}
	if n := len(putter.Calls()); n != 0 {
		t.Errorf("%d batches went to the DLQ; the sibling was healthy", n)
	}
}

// With no sibling the paused shard is still the only place the rows can go,
// so its writes keep routing to it and the shard client holds them.
func TestDrain_SolePausedShardStillTakesWrites(t *testing.T) {
	dlq, putter := testDLQ()
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, DLQ: dlq})
	paused := &countingShard{dlqFailingShard: dlqFailingShard{id: "shard-paused"}}
	seedWriter(c, paused, logdb.StateUpgrading, logdb.PhasePausing)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.StartDrain(ctx)

	writeBatches(c, 5)

	if err := waitFor(2*time.Second, func() bool { return paused.Rows() == 5 }); err != nil {
		t.Fatalf("sole paused shard got %d of 5 rows", paused.Rows())
	}
	if n := len(putter.Calls()); n != 0 {
		t.Errorf("%d batches went to the DLQ", n)
	}
}

func (s *countingShard) IngestBatch(ctx context.Context, b shard.Batch, rec shard.IngestRecorder) error {
	return s.Ingest(ctx, b.Rows, rec)
}
