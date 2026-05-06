package coordinator

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/store"
)

// closeTrackingShard is a shard.Shard whose Close() flips an atomic so
// the test can observe whether it ran synchronously or async.
type closeTrackingShard struct {
	id     string
	closed atomic.Bool
}

func (s *closeTrackingShard) ID() string                                            { return s.id }
func (s *closeTrackingShard) Query(string) ([]*pb.ResultRow, error)                 { return nil, nil }
func (s *closeTrackingShard) QueryToSink(string, shard.RowSink)                     {}
func (s *closeTrackingShard) Ingest(_ []*pb.LogRow) error                           { return nil }
func (s *closeTrackingShard) Archive(_ context.Context, _ string) (string, error)   { return "", nil }
func (s *closeTrackingShard) Close() error                                          { s.closed.Store(true); return nil }

// TestProcessHeartbeat_RebindDefersOldClose pins the close-during-RPC
// fix from docs/plans/shard-grpc-resilience.md Phase 1. When a heartbeat
// arrives at a new address for a known shard id, ProcessHeartbeat
// rebinds the gRPC connection. If it closes the old conn synchronously,
// any in-flight query that already grabbed a reference to the old
// shard.Shard via MatchingEntries has its stream cut with code=Canceled.
// Phase 2's retry softens the user-visible impact, but the right fix
// is to defer the close so RPCs can drain on the old conn first.
func TestProcessHeartbeat_RebindDefersOldClose(t *testing.T) {
	old := &closeTrackingShard{id: "shard-rebound"}

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID:    "shard-rebound",
		State: StateIngesting,
		Addr:  "10.0.0.1:9000",
		Shard: old,
	})

	const grace = 50 * time.Millisecond
	prov := &Provisioner{
		registry: reg,
		config: ProvisionerConfig{
			CloseRebindGrace: grace,
		},
	}

	prov.ProcessHeartbeat(store.ShardHeartbeat{
		ShardID: "shard-rebound",
		Addr:    "10.0.0.2:9000",
		State:   "INGESTING",
	})

	// The synchronous path would close before ProcessHeartbeat
	// returned. In-flight queries holding `old` would see a Canceled
	// error mid-stream.
	if old.closed.Load() {
		t.Fatal("old shard closed synchronously; in-flight RPCs would be cut")
	}

	// After the grace window the deferred goroutine should have run.
	time.Sleep(grace + 100*time.Millisecond)
	if !old.closed.Load() {
		t.Fatal("old shard not closed after grace period")
	}

	// New shard is wired into the registry regardless of close timing.
	got := reg.Get("shard-rebound")
	if got == nil || got.Addr != "10.0.0.2:9000" {
		t.Fatalf("registry not updated to new addr; got %+v", got)
	}
}
