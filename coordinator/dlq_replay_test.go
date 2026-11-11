package coordinator

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/logdb"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/store"
)

// fullRow sets fields the old DLQ format dropped (the whole trace group,
// dimensions, event id), so a lossy round trip shows up as a diff.
func fullRow(i int) *pb.LogRow {
	return &pb.LogRow{
		Raw:         "full row",
		Namespace:   "weft",
		Service:     "coordinator",
		Kind:        "main",
		Level:       "info",
		TsUnixNs:    1_700_000_000_000_000_000 + int64(i),
		Instance:    "i-123",
		PayloadJson: []byte(`{"k":"v"}`),
		CustomerId:  1,
		Deployment:  "prod",
		Env:         "prod",
		Version:     "v1.2.3",
		Region:      "us-east-2",
		Node:        "n1",
		TraceId:     "b493e2ab050433a5a0ce2a52db32116e",
		SpanName:    "upgrade.pause",
		SpanId:      "s1",
		EventId:     "evt-1",
		LineNo:      int32(i),
	}
}

func TestDLQ_ReplayRestoresEveryRowExactly(t *testing.T) {
	d, putter := testDLQ()
	want := []*pb.LogRow{fullRow(1), fullRow(2), fullRow(3)}
	if d.Write(dlqAdded, want) == "" {
		t.Fatal("Write failed")
	}

	var got []Row
	n, err := d.Replay(context.Background(), func(rows []Row) { got = append(got, rows...) })
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if n != len(want) || len(got) != len(want) {
		t.Fatalf("replayed %d rows (sink saw %d), want %d", n, len(got), len(want))
	}
	for i := range want {
		if !proto.Equal(want[i], got[i].LogRow) || got[i].Added != dlqAdded {
			t.Errorf("row %d changed in the DLQ:\n want %v %+v\n  got %v %+v", i, want[i], dlqAdded, got[i].LogRow, got[i].Added)
		}
	}
	if live := putter.Live(); live != 0 {
		t.Errorf("%d DLQ objects remain after their rows were delivered", live)
	}
}

// Rows a batch can't deliver again must stay in the DLQ: an object is only
// deleted after the sink has taken its rows.
func TestDLQ_ReplayKeepsObjectUntilSinkTookRows(t *testing.T) {
	d, putter := testDLQ()
	d.Write(dlqAdded, []*pb.LogRow{fullRow(1)})

	var seenLive int
	_, err := d.Replay(context.Background(), func([]Row) { seenLive = putter.Live() })
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if seenLive != 1 {
		t.Errorf("object was deleted before the sink took its rows (live during sink = %d)", seenLive)
	}
}

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
		ID: "shard-up", State: logdb.StateIngesting, Live: up, CreatedAt: time.Now(), BucketID: 1,
		Services: []store.ShardService{{ShardID: "shard-up", Service: "coordinator"}},
	})

	n, err := dlq.Replay(ctx, c.Ingest.Write)
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
	dlq.Write(dlqAdded, dlqTestRows("weft/coordinator", 4))

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
