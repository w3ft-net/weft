package logdb

import (
	"context"
	"sync"
	"testing"
	"time"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/store"
)

// cascadeConfigurer is a minimal ShardConfigurer that returns a
// DuckDBShard for each Dial, good enough for christen to complete.
type cascadeConfigurer struct {
	mu    sync.Mutex
	calls int
}

func (c *cascadeConfigurer) Dial(_ context.Context, _ string, req *pb.ConfigureRequest) (shard.Shard, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return shard.NewDuckDBShard(req.ShardId, "")
}

// asyncRecordingLauncher satisfies ShardLauncher + asyncLauncher +
// RecoveryTerminator. The async path mints LAUNCHING rows via
// provisionForBucket rather than dialling inline.
type asyncRecordingLauncher struct {
	terminatedForReplace []string
}

func (a *asyncRecordingLauncher) LaunchInstance(_ context.Context, _ string) (string, error) {
	return "", nil
}
func (a *asyncRecordingLauncher) TerminateInstance(_ context.Context, _, _ string) error { return nil }
func (a *asyncRecordingLauncher) UsesLaunchingRows() bool                               { return true }
func (a *asyncRecordingLauncher) TerminateForReplacement(_ context.Context, id string) error {
	a.terminatedForReplace = append(a.terminatedForReplace, id)
	return nil
}

// TestRefreshCascade_EndToEnd reproduces the full 2026-05-03 incident
// from deploy through data loss. The sequence:
//
//  1. Two healthy INGESTING shards (target=2) loaded from DB at boot
//     (simulating coordinator restart during deploy).
//  2. Old instances are dead (ASG refresh killed them). Their heartbeats
//     go stale.
//  3. reapDeadShards fires: TerminateForReplacement (no-op, already
//     gone), wipes InstanceID/Addr, state stays INGESTING.
//  4. Reconciler: counts both zombies as INGESTING → ingesting=2,
//     target=2 → ops=0. No LAUNCHING row minted.
//  5. Replacement instances heartbeat with ShardID="" (never configured).
//     christen() fails: "no LAUNCHING row for instance X."
//
// This test asserts the fix: SUSPENDED state breaks the cascade,
// reconciler mints LAUNCHING rows immediately, and SUSPENDED entries
// persist indefinitely for RECOVER.
func TestRefreshCascade_EndToEnd(t *testing.T) {
	now := time.Now().UTC()
	windowStart := now.Add(-2 * 24 * time.Hour)
	windowEnd := now.Add(5 * 24 * time.Hour)

	bucket := &AppBucket{
		ID:              1,
		Label:           "default",
		Apps:            []string{"weft"},
		Target:          2,
		Tier:            "small",
		WindowDuration:  7 * 24 * time.Hour,
	}

	// --- Phase 1: Simulate coordinator restart (LoadFromStore) ---
	// Two INGESTING entries exist in the DB from before the deploy.
	// Their instances are already dead (ASG refresh killed them), but
	// the DB rows still carry the old InstanceID/Addr.
	reg := NewShardRegistry()
	fake := newFakeRegistryStore()
	reg.SetStore(fake)

	reg.Add(&ShardEntry{
		ID:          "shard-A",
		BucketID:    1,
		State:       StateIngesting,
		Apps:        []string{"weft"},
		Addr:        "10.0.0.1:9000",
		InstanceID:  "i-dead-A",
		EBSVolumeID: "vol-A",
		Tier:        "small",
		WindowStart: windowStart,
		WindowEnd:   windowEnd,
		CreatedAt:   windowStart,
		RowCount:    52000,
	})
	reg.Add(&ShardEntry{
		ID:          "shard-B",
		BucketID:    1,
		State:       StateIngesting,
		Apps:        []string{"weft"},
		Addr:        "10.0.0.2:9000",
		InstanceID:  "i-dead-B",
		EBSVolumeID: "vol-B",
		Tier:        "small",
		WindowStart: windowStart,
		WindowEnd:   windowEnd,
		CreatedAt:   windowStart,
		RowCount:    40000,
	})

	// The heartbeat store: one unrelated live shard (so reapDeadShards
	// doesn't bail on "no heartbeats at all"). shard-A and shard-B are
	// NOT in the live set — their instances are dead.
	hbStore := &mockHeartbeatStore{
		heartbeats: []store.ShardHeartbeat{
			{ShardID: "shard-other", Addr: "10.0.0.99:9000", State: "INGESTING"},
		},
	}

	launcher := &recordingLauncher{}
	prov := &Provisioner{
		registry: reg,
		launcher: launcher,
		volumes:  &fakeVolumeManager{},
		config: ProvisionerConfig{
			Store:        hbStore,
			ShardOps:     fake,
			PollInterval: time.Second,
		},
		Buckets: &stubBuckets{bucket: bucket},
	}

	// --- Phase 2: First reap tick ---
	// Instances are dead (not in live heartbeats). reapDeadShards should
	// TerminateForReplacement and wipe the entries.
	prov.reapDeadShards()

	entryA := reg.Get("shard-A")
	entryB := reg.Get("shard-B")
	if entryA == nil || entryB == nil {
		t.Fatal("reap removed entries instead of preserving them for rebind")
	}
	if entryA.InstanceID != "" || entryA.Addr != "" {
		t.Fatalf("shard-A: expected wipe; got instance=%q addr=%q", entryA.InstanceID, entryA.Addr)
	}
	if entryB.InstanceID != "" || entryB.Addr != "" {
		t.Fatalf("shard-B: expected wipe; got instance=%q addr=%q", entryB.InstanceID, entryB.Addr)
	}
	if entryA.State != StateSuspended {
		t.Fatalf("shard-A: state after wipe = %q, want SUSPENDED", entryA.State)
	}
	if entryB.State != StateSuspended {
		t.Fatalf("shard-B: state after wipe = %q, want SUSPENDED", entryB.State)
	}

	// Confirm: TerminateForReplacement was called for both dead instances.
	if len(launcher.terminatedForReplace) != 2 {
		t.Fatalf("expected 2 TerminateForReplacement calls, got %d: %v",
			len(launcher.terminatedForReplace), launcher.terminatedForReplace)
	}

	// --- Phase 3: Reconciler tick (THE FIX) ---
	// SUSPENDED entries do NOT count toward ingesting → reconciler sees
	// the gap immediately and emits LaunchShardOps.
	snap := prov.snapshotForBucket(bucket)
	ops := reconcile(snap)

	ingesting := 0
	for _, e := range snap.Entries {
		if e.State == StateIngesting {
			ingesting++
		}
	}
	if ingesting != 0 {
		t.Fatalf("snapshot ingesting = %d, want 0 (SUSPENDED must not count)", ingesting)
	}

	launchOps := 0
	for _, op := range ops {
		if _, ok := op.(LaunchShardOp); ok {
			launchOps++
		}
	}
	if launchOps != 2 {
		t.Fatalf("expected 2 LaunchShardOps (one per missing slot); got %d", launchOps)
	}

	// --- Phase 4: SUSPENDED entries still queryable ---
	// Their EBS volumes exist; data isn't lost. A future rebind can
	// recover them. Meanwhile new shards handle fresh writes.
	if entryA.EBSVolumeID == "" || entryB.EBSVolumeID == "" {
		t.Error("SUSPENDED entries must retain their EBS volume ids for recovery")
	}

	// --- Phase 5: SUSPENDED entries persist indefinitely ---
	// No zombie-archive timeout. Entries stay SUSPENDED until a RECOVER
	// rebinds them. The volume remains mountable for recovery at any time.
	time.Sleep(60 * time.Millisecond)
	prov.reapDeadShards()

	entryA = reg.Get("shard-A")
	entryB = reg.Get("shard-B")
	if entryA == nil || entryB == nil {
		t.Fatal("entries vanished — SUSPENDED must be preserved")
	}
	if entryA.State != StateSuspended {
		t.Errorf("shard-A: state = %q, want SUSPENDED (no timeout-based archival)", entryA.State)
	}
	if entryB.State != StateSuspended {
		t.Errorf("shard-B: state = %q, want SUSPENDED (no timeout-based archival)", entryB.State)
	}
}

// TestRefreshCascade_SuspendedStateBreaksChain demonstrates the fix:
// when reapDeadShards transitions a wiped entry to SUSPENDED instead of
// leaving it INGESTING, the reconciler immediately sees the gap and
// mints LAUNCHING rows. The next instance can christen successfully.
//
// NOTE: This test will fail against the current code. It is the spec
// for the fix.
func TestRefreshCascade_SuspendedStateBreaksChain(t *testing.T) {
	now := time.Now().UTC()
	windowStart := now.Add(-2 * 24 * time.Hour)
	windowEnd := now.Add(5 * 24 * time.Hour)

	bucket := &AppBucket{
		ID:              1,
		Label:           "default",
		Apps:            []string{"weft"},
		Target:          2,
		Tier:            "small",
		WindowDuration:  7 * 24 * time.Hour,
	}

	reg := NewShardRegistry()
	fake := newFakeRegistryStore()
	reg.SetStore(fake)

	// Two shards whose instances just died.
	reg.Add(&ShardEntry{
		ID:          "shard-A",
		BucketID:    1,
		State:       StateIngesting,
		Apps:        []string{"weft"},
		Addr:        "10.0.0.1:9000",
		InstanceID:  "i-dead-A",
		EBSVolumeID: "vol-A",
		Tier:        "small",
		WindowStart: windowStart,
		WindowEnd:   windowEnd,
		CreatedAt:   windowStart,
		RowCount:    52000,
	})
	reg.Add(&ShardEntry{
		ID:          "shard-B",
		BucketID:    1,
		State:       StateIngesting,
		Apps:        []string{"weft"},
		Addr:        "10.0.0.2:9000",
		InstanceID:  "i-dead-B",
		EBSVolumeID: "vol-B",
		Tier:        "small",
		WindowStart: windowStart,
		WindowEnd:   windowEnd,
		CreatedAt:   windowStart,
		RowCount:    40000,
	})

	hbStore := &mockHeartbeatStore{
		heartbeats: []store.ShardHeartbeat{
			{ShardID: "shard-other", Addr: "10.0.0.99:9000", State: "INGESTING"},
		},
	}

	launcher := &asyncRecordingLauncher{}
	prov := &Provisioner{
		registry:   reg,
		launcher:   launcher,
		volumes:    &fakeVolumeManager{},
		configurer: &cascadeConfigurer{},
		config: ProvisionerConfig{
			Store:          hbStore,
			ShardOps:       fake,
			PollInterval:   time.Second,
			WindowDuration: 7 * 24 * time.Hour,
		},
		Buckets: &stubBuckets{bucket: bucket},
	}

	// First reap: wipes entries.
	prov.reapDeadShards()

	entryA := reg.Get("shard-A")
	entryB := reg.Get("shard-B")
	if entryA == nil || entryB == nil {
		t.Fatal("entries should still exist after reap")
	}

	// THE FIX: after wipe, state should be SUSPENDED (not INGESTING).
	if entryA.State != StateSuspended {
		t.Errorf("shard-A: state after wipe = %q, want SUSPENDED", entryA.State)
	}
	if entryB.State != StateSuspended {
		t.Errorf("shard-B: state after wipe = %q, want SUSPENDED", entryB.State)
	}

	// Reconciler: entries in the post-wipe state should NOT count
	// toward ingesting. With the fix, they'll be SUSPENDED.
	snap := prov.snapshotForBucket(bucket)
	ops := reconcile(snap)

	ingesting := 0
	for _, e := range snap.Entries {
		if e.State == StateIngesting {
			ingesting++
		}
	}
	if ingesting != 0 {
		t.Errorf("snapshot ingesting = %d, want 0 (post-wipe entries must not count)", ingesting)
	}

	launchOps := 0
	for _, op := range ops {
		if _, ok := op.(LaunchShardOp); ok {
			launchOps++
		}
	}
	if launchOps != 2 {
		t.Errorf("expected 2 LaunchShardOps (one per missing slot); got %d", launchOps)
	}

	// Apply the LaunchShardOps — with an async launcher this mints
	// LAUNCHING rows rather than trying to dial a shard inline.
	for _, op := range ops {
		if lo, ok := op.(LaunchShardOp); ok {
			lo.Apply(context.Background(), prov)
		}
	}

	// Verify LAUNCHING rows were minted.
	launchingCount, _ := fake.CountShardsInBucket(1, []string{string(StateLaunching)})
	if launchingCount < 2 {
		t.Errorf("expected ≥2 LAUNCHING rows after ops applied; got %d", launchingCount)
	}

	// Replacement heartbeat should now christen successfully
	// (FindLaunchingByInstance will find a row).
	// First: simulate lifecycle hook claiming a row for this instance.
	claimed, ok, err := fake.ClaimLaunchingForTier("small", "i-replacement-A")
	if err != nil || !ok {
		t.Fatalf("ClaimLaunchingForTier failed: ok=%v err=%v", ok, err)
	}

	hb := store.ShardHeartbeat{
		ShardID:    "",
		Addr:       "10.0.0.50:9000",
		InstanceID: "i-replacement-A",
		State:      "INGESTING",
	}
	prov.ProcessHeartbeat(hb)

	// The new shard should now be in the registry with the claimed id.
	newEntry := reg.Get(claimed.ID)
	if newEntry == nil {
		t.Fatalf("christen failed — new shard %s not in registry", claimed.ID)
	}
	if newEntry.State != StateIngesting {
		t.Errorf("new shard state = %q, want INGESTING", newEntry.State)
	}
	if newEntry.Addr != "10.0.0.50:9000" {
		t.Errorf("new shard addr = %q, want 10.0.0.50:9000", newEntry.Addr)
	}

	// Meanwhile the SUSPENDED entries remain in the registry (their EBS
	// volumes still exist; a rebind could still land). They don't block
	// capacity.
}
