package logdb

import (
	"testing"
	"time"

	"github.com/w3ft-net/weft/shard"
)

// TestCoordinator_RegistrySurvivesRestart verifies the "survives restart"
// property after the FSM switch: a shard persisted to the store is
// rehydrated into a fresh registry by LoadFromStore with its window,
// state, bucket, and striping intact. The live gRPC handle is
// deliberately absent post-restart (LoadFromStore can't redial) — the
// first post-restart heartbeat reattaches it via ShardSM.ObserveHeartbeat,
// which the SM-driven reconnect coverage in stage 4 exercises end-to-end.
func TestCoordinator_RegistrySurvivesRestart(t *testing.T) {
	db := openSMTestStore(t)
	seedDefaultBucket(t, db, 1, time.Hour) // id defaultBucketID, as the Postgres migrations seed it

	now := time.Now().UTC().Truncate(time.Second)
	windowEnd := now.Add(24 * time.Hour)
	stripe := 1

	// Phase 1: a shard is INGESTING and persisted (as the FSM mint +
	// promotion path would leave it).
	reg1 := NewShardRegistry()
	reg1.SetStore(db)
	reg1.Add(&Shard{
		ShardRow: ShardRow{
			ID:          "shard-restart",
			BucketID:    defaultBucketID,
			State:       StateIngesting,
			Tier:        "small",
			Addr:        "10.0.1.7:9000",
			InstanceID:  "i-restart",
			WindowStart: now,
			WindowEnd:   windowEnd,
			CreatedAt:   now,
			AZ:          "us-east-2a",
			StripeIndex: &stripe,
			EBSVolumeID: "vol-restart",
		},
	})
	// AZ/StripeIndex/instance/volume are FSM-owned columns persisted
	// through the narrow SetShardLaunchResult (as MintShard does), not the
	// registry's SaveShard upsert — stamp them so they survive the reload.
	if err := db.SetShardLaunchResult("shard-restart", "i-restart", "us-east-2a", "vol-restart", &stripe); err != nil {
		t.Fatalf("persist launch result: %v", err)
	}

	// Phase 2: simulate a coordinator restart — a brand-new registry
	// hydrating from the same store. The live Shard handle can't be
	// reconstructed for a non-archived row, so reconstruct returns an
	// error and the entry loads handle-less (mirrors production boot).
	reg2 := NewShardRegistry()
	reg2.SetStore(db)
	if err := reg2.LoadFromStore(func(*Shard) (shard.Shard, error) {
		return nil, errNoReconstruct
	}); err != nil {
		t.Fatalf("LoadFromStore: %v", err)
	}

	got := reg2.Get("shard-restart")
	if got == nil {
		t.Fatal("shard-restart not restored after restart")
	}
	if got.State != StateIngesting {
		t.Errorf("State: got %s want INGESTING", got.State)
	}
	if !got.WindowEnd.Equal(windowEnd) {
		t.Errorf("WindowEnd: got %v want %v", got.WindowEnd, windowEnd)
	}
	if got.BucketID != defaultBucketID {
		t.Errorf("BucketID: got %d want %d", got.BucketID, defaultBucketID)
	}
	if got.AZ != "us-east-2a" {
		t.Errorf("AZ: got %q want us-east-2a", got.AZ)
	}
	if got.StripeIndex == nil || *got.StripeIndex != stripe {
		t.Errorf("StripeIndex: got %v want %d", got.StripeIndex, stripe)
	}
	if got.EBSVolumeID != "vol-restart" {
		t.Errorf("EBSVolumeID: got %q want vol-restart", got.EBSVolumeID)
	}
}

// errNoReconstruct stands in for the reconstruct failure a non-archived
// row produces at boot (only ColdShards can be rebuilt from a record).
var errNoReconstruct = errShardReconstruct{}

type errShardReconstruct struct{}

func (errShardReconstruct) Error() string { return "no reconstruct for live shard" }
