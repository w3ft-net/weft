package coordinator

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/kevlarcon/weft/store"
)

// These tests pin the v1.md hypothesis — that a `kevcons deploy`-
// triggered ASG instance refresh launches replacement instances
// outside the reconciler's intent model, and the lifecycle hook's
// strict ABANDON-on-no-row contract turns those launches into a
// self-healing-only-after-~minutes loop.
//
// Each test reproduces one slice of the scenario:
//
//   1. Healthy fleet, refresh terminates a shard, hook fires for
//      replacement BEFORE heartbeat goes stale → ABANDON because
//      RECOVER's recognisedAsDead filter excludes the live shard's
//      volume and CHRISTEN finds no LAUNCHING row.
//   2. Same setup but the prior shard's heartbeat has already gone
//      stale → RECOVER attaches the volume, no row needed.
//   3. Reconciler doesn't emit a LaunchShardOp while the registry
//      still counts the post-reaper-wipe entry as INGESTING — the
//      gap stays invisible until rebindTimeout (15 min) flushes the
//      zombie to ARCHIVED.
//
// If (1) and (3) pass against current code, v1.md's diagnosis is
// empirically confirmed. If they fail, the model is wrong and the
// real cause is somewhere else.

// Refresh-launched instance hits the hook for a tier whose fleet is
// at target. No LAUNCHING row exists. The just-terminated original's
// EBS volume is tagged with its shard_id, but the original's
// heartbeat is still within deadFor — so recognisedAsDead filters
// the volume out, RECOVER returns 0 candidates, ClaimLaunchingForTier
// returns (zero, false), the hook ABANDONs.
func TestInstanceRefresh_LiveHeartbeat_NoRow_Abandons(t *testing.T) {
	vm := &fakeVolumeManager{
		availableByTier: map[string][]VolumeInfo{
			"small/us-east-2a": {
				{
					VolumeID: "vol-orig-data",
					AZ:       "us-east-2a",
					ShardID:  "shard-original",
					State:    "available",
					Tier:     "small",
				},
			},
		},
	}
	asg := &fakeLifecycleASG{}

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID:          "shard-original",
		State:       StateIngesting,
		Addr:        "10.0.0.1:9000",
		EBSVolumeID: "vol-orig-data",
	})

	storeFake := &mockHeartbeatStore{
		heartbeats: []store.ShardHeartbeat{
			{ShardID: "shard-original", Addr: "10.0.0.1:9000", State: "INGESTING"},
		},
	}
	rows := newFakeRegistryStore()

	h := handlerWithShardOps(vm, asg, reg, storeFake, rows)

	var logbuf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logbuf)
	t.Cleanup(func() { log.SetOutput(prev) })

	decision, err := h.HandleLaunchEvent(context.Background(), LifecycleEvent{
		ASGName:             "weft-shard-small",
		InstanceID:          "i-refresh-replacement",
		LifecycleTransition: "autoscaling:EC2_INSTANCE_LAUNCHING",
	})

	if err == nil {
		t.Fatal("expected an error: refresh-launched instance with no row should ABANDON")
	}
	if decision != DecisionAbandon {
		t.Errorf("decision: got %s want %s", decision, DecisionAbandon)
	}
	if asg.lastResult() != "ABANDON" {
		t.Errorf("CompleteLifecycleAction: got %q want ABANDON", asg.lastResult())
	}
	if len(vm.attached) != 0 {
		t.Errorf("must not attach a volume on ABANDON; got %+v", vm.attached)
	}
	if len(vm.createdByShard) != 0 {
		t.Errorf("must not create a volume on ABANDON; got %v", vm.createdByShard)
	}
	if !strings.Contains(err.Error(), "no LAUNCHING row") {
		t.Errorf("error should mention 'no LAUNCHING row' so prod logs are\n"+
			"diagnosable; got: %v", err)
	}

	logs := logbuf.String()
	if !strings.Contains(logs, "no LAUNCHING row") {
		t.Errorf("expected stderr log to mention 'no LAUNCHING row'; got:\n%s", logs)
	}
}

// Same setup as the previous test but the heartbeat for shard-original
// is gone (instance terminated long enough ago that
// LiveShardHeartbeats no longer returns it). RECOVER picks up the
// orphan volume — no LAUNCHING row needed.
//
// This is the eventual self-heal path: once 2 min pass,
// recognisedAsDead returns true, the orphan volume becomes a recovery
// candidate, and the next refresh-launched (or
// difference-between-desired-and-actual-launched) instance can RECOVER.
func TestInstanceRefresh_StaleHeartbeat_NoRow_Recovers(t *testing.T) {
	vm := &fakeVolumeManager{
		availableByTier: map[string][]VolumeInfo{
			"small/us-east-2a": {
				{
					VolumeID: "vol-orig-data",
					AZ:       "us-east-2a",
					ShardID:  "shard-original",
					State:    "available",
					Tier:     "small",
				},
			},
		},
	}
	asg := &fakeLifecycleASG{}

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID:          "shard-original",
		State:       StateIngesting,
		Addr:        "10.0.0.1:9000",
		EBSVolumeID: "vol-orig-data",
	})

	// No live heartbeat for shard-original — it's been ≥deadFor since
	// the original instance died.
	storeFake := &mockHeartbeatStore{heartbeats: nil}
	rows := newFakeRegistryStore()

	h := handlerWithShardOps(vm, asg, reg, storeFake, rows)

	decision, err := h.HandleLaunchEvent(context.Background(), LifecycleEvent{
		ASGName:             "weft-shard-small",
		InstanceID:          "i-refresh-replacement",
		LifecycleTransition: "autoscaling:EC2_INSTANCE_LAUNCHING",
	})
	if err != nil {
		t.Fatalf("expected RECOVER once heartbeat stale, got err: %v", err)
	}
	if decision != DecisionRecover {
		t.Errorf("decision: got %s want %s", decision, DecisionRecover)
	}
	if len(vm.attached) != 1 || vm.attached[0].volumeID != "vol-orig-data" {
		t.Errorf("expected vol-orig-data attached, got %+v", vm.attached)
	}
	if asg.lastResult() != "CONTINUE" {
		t.Errorf("CompleteLifecycleAction: got %q want CONTINUE", asg.lastResult())
	}
}

// While the registry still has an INGESTING entry — even one whose
// instance_id has been wiped by reapDeadShards — the reconciler walks
// it as part of IngestingCount and does NOT emit a LaunchShardOp.
// This is what blocks recovery during the ~15-minute rebindTimeout
// window: the gap exists in reality (instance gone) but not in the
// reconciler's view (entry still INGESTING).
//
// Pre-fix, this was the orphan-christen-loop's underlying mechanism;
// the registry-as-DB-cache fix moved IngestingCount to a registry walk,
// which corrected one source of error but doesn't help when the
// registry itself contains the post-wipe zombie.
func TestReconciler_PostReaperWipeEntry_BlocksLaunchOp(t *testing.T) {
	now := time.Now().UTC()

	bucket := &AppBucket{
		ID:             defaultBucketID,
		Label:          "default",
		Apps:           []string{"weft"},
		Target:         2,
		Tier:           "small",
		WindowDuration: 7 * 24 * time.Hour,
	}

	// One healthy shard (live) + one zombie (post-reaper-wipe).
	// The zombie's State is INGESTING but instance_id+addr are empty.
	// This is exactly what reapDeadShards leaves behind on the
	// EBS-bound branch (provisioner.go's wipe-and-await-rebind).
	entries := []*ShardEntry{
		{
			ID:          "shard-healthy",
			BucketID:    defaultBucketID,
			State:       StateIngesting,
			Apps:        []string{"weft"},
			Addr:        "10.0.0.1:9000",
			InstanceID:  "i-alive",
			EBSVolumeID: "vol-healthy",
			WindowStart: now.Add(-time.Hour),
			WindowEnd:   now.Add(6 * 24 * time.Hour),
			CreatedAt:   now.Add(-time.Hour),
		},
		{
			ID:              "shard-zombie",
			BucketID:        defaultBucketID,
			State:           StateIngesting,
			Apps:            []string{"weft"},
			Addr:            "",
			InstanceID:      "",
			EBSVolumeID:     "vol-zombie",
			WindowStart:     now.Add(-time.Hour),
			WindowEnd:       now.Add(6 * 24 * time.Hour),
			CreatedAt:       now.Add(-time.Hour),
			LastHeartbeatAt: now.Add(-3 * time.Minute), // wiped 3 min ago
		},
	}

	snap := ReconcilerSnapshot{
		Bucket:          bucket,
		Entries:         entries,
		LaunchingCount:  0,
		IngestingCount:  2, // both entries count, including the zombie
		IsAsyncLauncher: true,
		Now:             now,
	}

	ops := reconcile(snap)

	launchOps := 0
	for _, op := range ops {
		if _, ok := op.(LaunchShardOp); ok {
			launchOps++
		}
	}

	// THIS IS THE BUG: only one healthy shard remains, but the
	// reconciler counts the zombie and thinks target is met.
	if launchOps != 0 {
		t.Errorf("expected 0 LaunchShardOps (the bug — reconciler can't tell\n"+
			"the zombie isn't a real shard); got %d", launchOps)
	}

	// And to be very explicit about what would fix it:
	// if the zombie's state were anything but INGESTING (LAUNCHING,
	// ARCHIVED, or removed), the reconciler would emit a LaunchOp.
	// Demonstrate by re-running with the zombie marked ARCHIVED:
	entries[1].State = StateArchived
	snap.Entries = entries
	snap.IngestingCount = 1
	ops = reconcile(snap)
	launchOps = 0
	for _, op := range ops {
		if _, ok := op.(LaunchShardOp); ok {
			launchOps++
		}
	}
	if launchOps != 1 {
		t.Errorf("with zombie marked ARCHIVED, expected 1 LaunchShardOp;\n"+
			"got %d (%v)", launchOps, ops)
	}
}

// Capturing log output for the diagnostic-coverage tests below.
// Returns a snapshot func — call it after the action to read the
// accumulated log buffer as a string.
func captureLog(t *testing.T) func() string {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return func() string { return buf.String() }
}

// asserts every key=value pair appears in s. Reports ALL missing
// pairs together — easier to fix in one pass than chasing one
// failure at a time.
func assertLogHas(t *testing.T, s string, kvs ...string) {
	t.Helper()
	var missing []string
	for _, kv := range kvs {
		if !strings.Contains(s, kv) {
			missing = append(missing, kv)
		}
	}
	if len(missing) > 0 {
		t.Errorf("log missing %d expected pairs:\n  missing: %v\n  log:\n%s",
			len(missing), missing, s)
	}
}

// On any lifecycle event, the prod log must show the inputs (asg,
// instance, transition) and the final decision with reason — enough
// to reconstruct what happened without needing the source code open.
//
// This is the diagnostic-coverage promise: a single ABANDONing event
// in prod produces a log line the on-call can grep for.
func TestLifecycle_LogStructuredOnAbandon(t *testing.T) {
	vm := &fakeVolumeManager{
		availableByTier: map[string][]VolumeInfo{
			"small/us-east-2a": {
				{VolumeID: "vol-orig", AZ: "us-east-2a", ShardID: "shard-original",
					State: "available", Tier: "small"},
			},
		},
	}
	asg := &fakeLifecycleASG{}
	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID: "shard-original", State: StateIngesting,
		Addr: "10.0.0.1:9000", EBSVolumeID: "vol-orig",
	})
	storeFake := &mockHeartbeatStore{
		heartbeats: []store.ShardHeartbeat{
			{ShardID: "shard-original", Addr: "10.0.0.1:9000", State: "INGESTING"},
		},
	}
	h := handlerWithShardOps(vm, asg, reg, storeFake, newFakeRegistryStore())

	getLog := captureLog(t)
	_, _ = h.HandleLaunchEvent(context.Background(), LifecycleEvent{
		ASGName:             "weft-shard-small",
		InstanceID:          "i-refresh-replacement",
		LifecycleTransition: "autoscaling:EC2_INSTANCE_LAUNCHING",
	})

	// The log must let an operator answer:
	//   what tier? which instance? AZ? how many candidates were
	//   considered? why each was filtered? what was the final call?
	assertLogHas(t, getLog(),
		"asg=weft-shard-small",
		"instance=i-refresh-replacement",
		"tier=small",
		"az=us-east-2a",
		"candidates=1",      // ListAvailableByTier returned vol-orig
		"recoverable=0",     // recognisedAsDead filtered it out
		"shard=shard-original",
		"decision=ABANDON",
	)
}

// On RECOVER, log a parallel breakdown: same volume considered, this
// time NOT filtered → recoverable=1, decision=RECOVER, volume=...
func TestLifecycle_LogStructuredOnRecover(t *testing.T) {
	vm := &fakeVolumeManager{
		availableByTier: map[string][]VolumeInfo{
			"small/us-east-2a": {
				{VolumeID: "vol-orig", AZ: "us-east-2a", ShardID: "shard-original",
					State: "available", Tier: "small"},
			},
		},
	}
	asg := &fakeLifecycleASG{}
	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID: "shard-original", State: StateIngesting,
		Addr: "10.0.0.1:9000", EBSVolumeID: "vol-orig",
	})
	storeFake := &mockHeartbeatStore{heartbeats: nil} // shard considered dead
	h := handlerWithShardOps(vm, asg, reg, storeFake, newFakeRegistryStore())

	getLog := captureLog(t)
	_, _ = h.HandleLaunchEvent(context.Background(), LifecycleEvent{
		ASGName:             "weft-shard-small",
		InstanceID:          "i-replacement",
		LifecycleTransition: "autoscaling:EC2_INSTANCE_LAUNCHING",
	})

	assertLogHas(t, getLog(),
		"asg=weft-shard-small",
		"instance=i-replacement",
		"tier=small",
		"az=us-east-2a",
		"candidates=1",
		"recoverable=1",
		"decision=RECOVER",
		"volume=vol-orig",
	)
}

// syncASGDesiredCapacity must log the per-tier components of its
// `desired` calculation so we can see at a glance whether the
// coordinator's intent matches the ASG's actual count. During an
// instance refresh, the coordinator-computed desired stays at target
// while AWS rolls instances in/out; the breakdown line lets us
// distinguish "coordinator computed wrong" from "ASG behaving
// independently."
func TestSyncASGDesired_LogsPerTierBreakdown(t *testing.T) {
	reg := NewShardRegistry()
	reg.Add(&ShardEntry{ID: "ro-1", BucketID: 1, Tier: "small", State: StateReadOnly})

	buckets := &manyBucketsStub{byLabel: map[string]*AppBucket{
		"default": {ID: 1, Label: "default", Target: 2, Tier: "small"},
	}}
	scaler := &fakeTierScaler{maxByTier: map[string]int32{"small": 8}}
	prov := &Provisioner{
		registry: reg,
		launcher: scaler,
		Buckets:  buckets,
	}

	getLog := captureLog(t)
	prov.syncASGDesiredCapacity(context.Background())

	// One log line per tier covering every input to the formula —
	// otherwise we can't tell why a particular tier ended up at the
	// observed desired value.
	assertLogHas(t, getLog(),
		"tier=small",
		"target=2",
		"read_only=1",
		"future_launching=0",
		"desired=3",
		"max=8",
	)
}

// Each per-bucket reconciler tick should log its snapshot inputs
// (target, ingesting, launching, future_launching) and the count of
// emitted ops. This is the reconciler's view of the world — the
// other half of the picture during a refresh-induced incident.
func TestReconcile_LogsPerBucketSummary(t *testing.T) {
	now := time.Now().UTC()
	bucket := &AppBucket{
		ID: defaultBucketID, Label: "default", Apps: []string{"weft"},
		Target: 2, Tier: "small", WindowDuration: 7 * 24 * time.Hour,
	}
	entries := []*ShardEntry{
		{
			ID: "shard-A", BucketID: defaultBucketID, State: StateIngesting,
			Apps: []string{"weft"}, WindowStart: now.Add(-time.Hour),
			WindowEnd: now.Add(6 * 24 * time.Hour), CreatedAt: now.Add(-time.Hour),
		},
	}

	reg := NewShardRegistry()
	for _, e := range entries {
		reg.Add(e)
	}
	stub := &stubBuckets{bucket: bucket}
	prov := &Provisioner{
		registry: reg,
		launcher: &countingLauncher{addr: "127.0.0.1:1"},
		config: ProvisionerConfig{
			ShardOps:       newFakeRegistryStore(),
			WindowDuration: bucket.WindowDuration,
		},
		Buckets: stub,
	}

	getLog := captureLog(t)
	prov.ReconcileBucket(context.Background(), bucket)

	assertLogHas(t, getLog(),
		"bucket=default",
		"tier=small",
		"target=2",
		"ingesting=1",
		"launching=0",
	)
}

// rebindTimeout is the only thing that flushes a post-wipe zombie to
// ARCHIVED, freeing the slot for the reconciler to fill. Default is
// 15 min. This test pins that bound — anything that lengthens it
// extends the v1.md outage window.
func TestReap_RebindTimeoutFlushesZombieToArchived(t *testing.T) {
	// Reduce the timeout to something fast so we can cross it in
	// the test. Restored on cleanup.
	prev := rebindTimeout
	rebindTimeout = 10 * time.Millisecond
	t.Cleanup(func() { rebindTimeout = prev })

	reg := NewShardRegistry()
	fake := newFakeRegistryStore()
	reg.SetStore(fake)

	const id = "shard-zombie"
	reg.Add(&ShardEntry{
		ID:              id,
		State:           StateIngesting,
		Addr:            "",
		InstanceID:      "",
		EBSVolumeID:     "vol-data",
		LastHeartbeatAt: time.Now().Add(-time.Hour), // wiped a long time ago
		CreatedAt:       time.Now().Add(-2 * time.Hour),
	})

	mockStore := &mockHeartbeatStore{
		heartbeats: []store.ShardHeartbeat{
			{ShardID: "other-live", Addr: "10.0.0.1:9000", State: "INGESTING"},
		},
	}
	prov := &Provisioner{
		registry: reg,
		launcher: &recordingLauncher{},
		config:   ProvisionerConfig{Store: mockStore, PollInterval: time.Second},
	}

	prov.reapDeadShards()

	got := reg.Get(id)
	if got == nil {
		t.Fatal("zombie entry was removed; expected ARCHIVED transition (entry preserved)")
	}
	if got.State != StateArchived {
		t.Errorf("after rebindTimeout, zombie state = %s; want ARCHIVED", got.State)
	}
}
