package logdb

import (
	"context"
	"testing"
	"time"

	"github.com/kevlarcon/weft/store"
)

// TestLifecycle_EmptyRegistry_AbandonsSuspendedShard reproduces the
// deploy-time data loss bug: a SUSPENDED shard with an orphan EBS
// volume should be recovered, but the lifecycle hook fires before
// LoadFromStore populates the registry. recognisedAsDead returns
// false ("not-in-registry"), the volume is skipped, and the handler
// falls through to ABANDON — orphaning the volume permanently.
//
// The test constructs the exact state that exists during a deploy:
//   - DB has a persisted shard record (SUSPENDED, with an EBS volume)
//   - The orphan volume is available in the right tier/AZ
//   - The registry is empty (LoadFromStore has not been called)
//
// Expected (after fix): RECOVER — the registry is populated before
// any lifecycle hook fires, so the shard is found and its volume
// reattached.
//
// Actual (before fix): ABANDON — empty registry means the shard is
// invisible to recognisedAsDead.
func TestLifecycle_EmptyRegistry_AbandonsSuspendedShard(t *testing.T) {
	// The DB knows about this shard.
	dbStore := newFakeRegistryStore()
	if err := dbStore.SaveShard(&store.ShardRecord{
		ID:          "shard-suspended-01",
		State:       string(StateSuspended),
		Tier:        "small",
		EBSVolumeID: "vol-orphan-01",
		BucketID:    defaultBucketID,
		Apps:        `["myapp"]`,
		WindowStart: "2026-05-09T00:00:00Z",
		WindowEnd:   "2026-05-10T00:00:00Z",
		CreatedAt:   time.Now().Add(-6 * time.Hour).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	// The orphan EBS volume is sitting unattached in the right tier+AZ.
	vm := &fakeVolumeManager{
		availableByTier: map[string][]VolumeInfo{
			"small/us-east-2a": {
				{VolumeID: "vol-orphan-01", AZ: "us-east-2a", ShardID: "shard-suspended-01", State: "available", Tier: "small"},
			},
		},
	}

	// Simulate the boot-order bug: registry exists but LoadFromStore
	// has NOT been called yet. The registry is empty.
	reg := NewShardRegistry()
	reg.SetStore(dbStore)
	// NOTE: reg.LoadFromStore(...) is NOT called — this is the bug.

	asg := &fakeLifecycleASG{}
	h := handlerWithShardOps(vm, asg, reg, &mockHeartbeatStore{heartbeats: nil}, dbStore)

	ev := LifecycleEvent{
		ASGName:             "weft-shard-small",
		InstanceID:          "i-fresh-deploy",
		LifecycleTransition: "autoscaling:EC2_INSTANCE_LAUNCHING",
	}
	decision, err := h.HandleLaunchEvent(context.Background(), ev)

	// With the registry empty, recognisedAsDead("shard-suspended-01")
	// returns false → volume filtered out → no recoverable candidates
	// → falls to christen → no LAUNCHING row → ABANDON.
	//
	// This SHOULD be RECOVER. The shard is in the DB, the volume is
	// available, and the shard has no recent heartbeat. The only
	// reason it fails is that LoadFromStore hasn't run yet.
	if decision != DecisionRecover {
		t.Errorf("decision = %s, want RECOVER — empty registry caused the handler to miss a recoverable volume (err: %v)", decision, err)
	}
	if len(vm.attached) != 1 || vm.attached[0].volumeID != "vol-orphan-01" {
		t.Errorf("expected vol-orphan-01 to be attached, got %v", vm.attached)
	}
}
