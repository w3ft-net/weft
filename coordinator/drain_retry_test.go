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

// downShard is an INGESTING shard whose instance is gone: every Ingest fails,
// and the registry keeps offering it until it has failed enough in a row.
type downShard struct {
	dlqFailingShard
	mu       sync.Mutex
	attempts int
}

func (s *downShard) Ingest(ctx context.Context, rows []*pb.LogRow, rec shard.IngestRecorder) error {
	s.mu.Lock()
	s.attempts++
	s.mu.Unlock()
	return s.dlqFailingShard.Ingest(ctx, rows, rec)
}

func seedIngesting(c *Coordinator, id string, live shard.Shard) {
	logdb.SeedShard(c.Provisioner, &logdb.Shard{
		ID:        id,
		State:     logdb.StateIngesting,
		Live:      live,
		CreatedAt: time.Now(),
		BucketID:  1,
		Services:  []store.ShardService{{ShardID: id, Service: "coordinator"}},
	})
}

// When a batch's shard fails, the retry goes to another shard that takes the
// service. It must never be the one that just failed: the registry
// round-robins from a counter every sender advances, so a retry that simply
// asks again lands on the failed shard about half the time, and the batch is
// DLQ'd with a healthy sibling idle.
func TestIngestBatch_RetriesOnASiblingNotTheShardThatFailed(t *testing.T) {
	dlq, putter := testDLQ()
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, DLQ: dlq})
	down := &downShard{dlqFailingShard: dlqFailingShard{id: "shard-down"}}
	up := &countingShard{dlqFailingShard: dlqFailingShard{id: "shard-up"}}
	seedIngesting(c, down.id, down)
	seedIngesting(c, up.id, up)

	const batches = 20
	for i := 0; i < batches; i++ {
		c.ingestBatch(context.Background(), drainBatch{
			key:  drainKey{namespace: "weft", service: "coordinator"},
			sh:   down,
			rows: dlqTestRows("weft/coordinator", 1),
		})
	}

	if got := up.Rows(); got != batches {
		t.Errorf("the healthy sibling took %d of %d batches", got, batches)
	}
	if n := len(putter.Calls()); n != 0 {
		t.Errorf("%d batches went to the DLQ with a healthy sibling available", n)
	}
	if down.attempts != batches {
		t.Errorf("the failed shard was tried %d times for %d batches: the retry went back to it", down.attempts, batches)
	}
}

// With no other shard to take it, the batch still lands in the DLQ.
func TestIngestBatch_WithNoSiblingTheBatchIsDLQd(t *testing.T) {
	dlq, putter := testDLQ()
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, DLQ: dlq})
	down := &downShard{dlqFailingShard: dlqFailingShard{id: "shard-down"}}
	seedIngesting(c, down.id, down)

	c.ingestBatch(context.Background(), drainBatch{
		key:  drainKey{namespace: "weft", service: "coordinator"},
		sh:   down,
		rows: dlqTestRows("weft/coordinator", 1),
	})
	if n := len(putter.Calls()); n != 1 {
		t.Fatalf("%d DLQ writes, want 1", n)
	}
	if down.attempts != 1 {
		t.Errorf("the only shard was tried %d times, want once (the retry has nowhere else to go)", down.attempts)
	}
}
