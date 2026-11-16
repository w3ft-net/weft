package coordinator

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/shardstore"
	"github.com/w3ft-net/weft/compute"
	"github.com/w3ft-net/weft/logdb"
)

// The whole path: shards down -> DLQ; shard back -> replay -> rows land
// once, DLQ empty.
func TestDLQ_RowsFailedWhileDownLandOnceAfterRecovery(t *testing.T) {
	dlq, putter := testDLQ()
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, DLQ: dlq})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.StartDrain(ctx)

	c.Ingest.Write(dlqIngestRows("weft/coordinator", 7))
	if err := waitFor(time.Second, func() bool { return putter.Live() == 1 }); err != nil {
		t.Fatalf("rows never reached the DLQ: %v", err)
	}

	up := &countingShard{dlqFailingShard: dlqFailingShard{id: "shard-up"}}
	logdb.SeedShard(c.Provisioner, &logdb.Shard{
		ShardRow: logdb.ShardRow{
			ID:        "shard-up",
			State:     logdb.StateIngesting,
			CreatedAt: time.Now(),
			BucketID:  1,
		},
		Live:     up,
		Services: []shardstore.ShardService{{ShardID: "shard-up", Service: "coordinator"}},
	})

	n, err := c.replayDeadLetters(ctx)
	if err != nil || n != 7 {
		t.Fatalf("Replay = %d, %v; want 7, nil", n, err)
	}
	if err := waitFor(time.Second, func() bool { return up.Rows() == 7 }); err != nil {
		t.Fatalf("shard got %d of 7 replayed rows", up.Rows())
	}
	time.Sleep(50 * time.Millisecond)
	if up.Rows() != 7 || putter.Live() != 0 {
		t.Errorf("shard rows = %d (want 7 exactly once), DLQ objects left = %d (want 0)", up.Rows(), putter.Live())
	}
}

// Both coordinators run the loop; only the shard-fsm leader may replay, or a
// batch would be delivered once per coordinator.
func TestStartDLQReplay_ReplaysOnlyWhileLeader(t *testing.T) {
	dlq, putter := testDLQ()
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, DLQ: dlq})
	dlq.Write(context.Background(), dlqAdded, dlqTestRows("weft/coordinator", 4))

	var leader atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.StartDLQReplay(ctx, 10*time.Millisecond, leader.Load)

	time.Sleep(100 * time.Millisecond)
	if putter.Live() != 1 || c.Ingest.Drain(10) != nil {
		t.Fatal("a follower replayed the DLQ")
	}

	leader.Store(true)
	if err := waitFor(time.Second, func() bool { return putter.Live() == 0 }); err != nil {
		t.Fatalf("leader never replayed the DLQ: %v", err)
	}
	if got := c.Ingest.Drain(10); len(got) != 4 {
		t.Errorf("ring buffer got %d replayed rows, want 4", len(got))
	}
}
