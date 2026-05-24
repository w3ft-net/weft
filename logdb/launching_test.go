package logdb

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kevlarcon/weft/store"
)

// A reconciler tick for a bucket under capacity must INSERT a
// LAUNCHING row before calling the ASG, and must not exceed the
// bucket's target across multiple ticks while the row is in-flight.
// This is the invariant §2 protects against the pre-rewrite ratchet:
// without a row, the DB count stays at 0 each tick and the ASG's
// desired_capacity climbs by one per tick until the first instance
// finishes booting.
func TestReconciler_MintsLaunchingRow_AndCountsAgainstTarget(t *testing.T) {
	reg := NewShardRegistry()
	fake := newFakeRegistryStore()
	reg.SetStore(fake)

	db := openSMTestStore(t)
	seedDefaultBucket(t, db, nil, 2, 24*time.Hour)

	launcher := &countingLauncher{addr: "127.0.0.1:1"}

	prov := &Provisioner{
		registry: reg,
		launcher: launcher,
		config: ProvisionerConfig{
			WindowDuration: 24 * time.Hour,
			ShardOps:       fake,
		},
		stats:      NewServiceStats(),
		Buckets:    NewBucketManager(db),
		configurer: &stubConfigurer{},
	}

	// First tick: bucket at 0 capacity, target 2 → two LAUNCHING
	// rows INSERTed. ASG scaling is handled separately by
	// syncASGDesiredCapacity; this test asserts on the LAUNCHING
	// rows, which are the coordinator's source of truth.
	prov.check(context.Background())

	launchingRows := func() int {
		n, _ := fake.CountShardsInBucket(1, []string{string(StateLaunching)})
		return n
	}
	if got := launchingRows(); got != 2 {
		t.Errorf("after tick 1: want 2 LAUNCHING rows, got %d", got)
	}

	// Second tick: the reconciler snapshot sees 2 LAUNCHING rows →
	// target met → no additional mints. Regression guard against
	// the pre-§2 ratchet.
	prov.check(context.Background())

	if got := launchingRows(); got != 2 {
		t.Errorf("after tick 2: want 2 LAUNCHING rows still, got %d — reconciler ratcheted", got)
	}
}

// The lifecycle hook fires when an ASG-launched instance enters
// Pending:Wait. It must claim the oldest unmatched LAUNCHING row for
// its tier, stamp instance_id onto the DB row, and pass the claimed
// shard id (not a "pending-<instance>" placeholder) to CreateForShard
// so the volume boots with the real tag in place.
func TestLifecycleHook_ClaimsOldestLaunchingRow(t *testing.T) {
	fake := newFakeRegistryStore()

	// Seed two LAUNCHING rows for tier=small; row-A is older.
	older := time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339)
	newer := time.Now().UTC().Format(time.RFC3339)
	for _, rec := range []store.ShardRecord{
		{ID: "shard-A", State: string(StateLaunching), Tier: "small", BucketID: 1, CreatedAt: older,
			WindowStart: "2026-04-21T00:00:00Z", WindowEnd: "2026-04-22T00:00:00Z"},
		{ID: "shard-B", State: string(StateLaunching), Tier: "small", BucketID: 1, CreatedAt: newer,
			WindowStart: "2026-04-21T00:00:00Z", WindowEnd: "2026-04-22T00:00:00Z"},
	} {
		r := rec
		_ = fake.SaveShard(&r)
	}

	vm := &fakeVolumeManager{}
	asg := &fakeLifecycleASG{}
	reg := NewShardRegistry()

	h := NewLifecycleHandler(LifecycleHandlerConfig{
		Volumes:      vm,
		EC2Instances: &fakeEC2Instances{},
		ASG:          asg,
		Store:        &mockHeartbeatStore{},
		ShardOps:     fake,
		Registry:     reg,
		TierByASG:    map[string]string{"weft-shard-small": "small"},
		SizeByTier:   map[string]int32{"small": 100},
		DeviceName:   "/dev/sdf",
		DeadFor:      2 * time.Minute,
	})

	decision, err := h.HandleLaunchEvent(context.Background(), LifecycleEvent{
		ASGName:             "weft-shard-small",
		InstanceID:          "i-claims-A",
		LifecycleTransition: "autoscaling:EC2_INSTANCE_LAUNCHING",
	})
	if err != nil {
		t.Fatalf("HandleLaunchEvent: %v", err)
	}
	if decision != DecisionChristen {
		t.Errorf("decision: got %s want CHRISTEN", decision)
	}

	// shard-A (oldest) should have been stamped with i-claims-A.
	gotA, ok := fake.get("shard-A")
	if !ok {
		t.Fatal("shard-A missing after claim")
	}
	if gotA.InstanceID != "i-claims-A" {
		t.Errorf("shard-A.instance_id: got %q want i-claims-A", gotA.InstanceID)
	}

	// shard-B must remain unclaimed.
	gotB, _ := fake.get("shard-B")
	if gotB.InstanceID != "" {
		t.Errorf("shard-B.instance_id: got %q want empty (claim should have picked the older row)", gotB.InstanceID)
	}

	// The created volume must be tagged with the claimed row's id,
	// not a placeholder. createdByShard is keyed by the ShardID
	// passed to CreateForShard.
	if _, ok := vm.createdByShard["shard-A"]; !ok {
		t.Errorf("CreateForShard didn't use claimed row's id; createdByShard=%v", vm.createdByShard)
	}
}

// A heartbeat from an instance already stamped on a LAUNCHING row
// must promote that row to INGESTING, sending Configure with the
// row's existing shard id / heartbeat token / window (not freshly
// minted values). The row and its entry keep the identity that the
// reconciler chose at INSERT time.
func TestChristen_PromotesLaunchingRow_InsteadOfMinting(t *testing.T) {
	fake := newFakeRegistryStore()
	reg := NewShardRegistry()
	reg.SetStore(fake)

	db := openSMTestStore(t)
	seedDefaultBucket(t, db, []string{"weft"}, 2, 0)

	token := "a-previously-minted-token"
	windowStart := "2026-04-21T00:00:00Z"
	windowEnd := "2026-04-22T00:00:00Z"
	_ = fake.SaveShard(&store.ShardRecord{
		ID:             "shard-preminted",
		State:          string(StateLaunching),
		Tier:           "small",
		BucketID:       1,
		WindowStart:    windowStart,
		WindowEnd:      windowEnd,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
		HeartbeatToken: token,
		InstanceID:     "i-promoted",
	})

	configurer := &stubConfigurer{}
	prov := &Provisioner{
		registry: reg,
		config: ProvisionerConfig{
			PollInterval:   time.Second,
			WindowDuration: 7 * 24 * time.Hour,
			ShardOps:       fake,
		},
		configurer: configurer,
		Buckets:    NewBucketManager(db),
	}

	prov.ProcessHeartbeat(store.ShardHeartbeat{
		ShardID:    "",
		Addr:       "10.0.0.50:9000",
		InstanceID: "i-promoted",
		State:      "ADOPTED",
	})

	if len(configurer.calls) != 1 {
		t.Fatalf("want 1 Configure call, got %d", len(configurer.calls))
	}
	req := configurer.calls[0].Req
	if req.ShardId != "shard-preminted" {
		t.Errorf("Configure.ShardId: got %q want shard-preminted (minted a new id instead of promoting)", req.ShardId)
	}
	if req.HeartbeatToken != token {
		t.Errorf("Configure.HeartbeatToken: got %q want %q (new token minted instead of reusing row's)",
			req.HeartbeatToken, token)
	}
	if req.WindowStart != windowStart || req.WindowEnd != windowEnd {
		t.Errorf("Configure window: got [%s,%s] want [%s,%s]",
			req.WindowStart, req.WindowEnd, windowStart, windowEnd)
	}

	// The row was updated from LAUNCHING → INGESTING (in-place
	// UPSERT; same primary key, new state).
	gotRec, ok := fake.get("shard-preminted")
	if !ok {
		t.Fatal("shard-preminted disappeared after promotion")
	}
	if gotRec.State != string(StateIngesting) {
		t.Errorf("state: got %s want INGESTING", gotRec.State)
	}

	// Registry entry present and INGESTING.
	entry := reg.Get("shard-preminted")
	if entry == nil {
		t.Fatal("shard-preminted not in registry after promotion")
	}
	if entry.State != StateIngesting {
		t.Errorf("entry state: got %s want INGESTING", entry.State)
	}
}

// A LAUNCHING row older than staleLaunchingAge with an empty
// instance_id means the ASG never brought an instance up (capacity
// exhausted, launch template misconfigured). The reaper must DELETE
// the row so next tick's effectiveCapacity sees the gap and re-mints.
func TestReaper_DeletesStaleLaunchingWithoutInstance(t *testing.T) {
	fake := newFakeRegistryStore()
	reg := NewShardRegistry()
	reg.SetStore(fake)

	// Fresh LAUNCHING row — must not be reaped.
	fresh := time.Now().UTC().Format(time.RFC3339)
	_ = fake.SaveShard(&store.ShardRecord{
		ID:          "shard-fresh",
		State:       string(StateLaunching),
		Tier:        "small",
		BucketID:    1,
		WindowStart: "2026-04-21T00:00:00Z", WindowEnd: "2026-04-22T00:00:00Z",
		CreatedAt: fresh,
	})

	// Stale LAUNCHING row with empty instance_id — must be deleted.
	stale := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	_ = fake.SaveShard(&store.ShardRecord{
		ID:          "shard-stale-empty",
		State:       string(StateLaunching),
		Tier:        "small",
		BucketID:    1,
		WindowStart: "2026-04-21T00:00:00Z", WindowEnd: "2026-04-22T00:00:00Z",
		CreatedAt: stale,
	})

	prov := &Provisioner{
		registry: reg,
		launcher: &countingLauncher{},
		config:   ProvisionerConfig{ShardOps: fake},
	}

	prov.reapStaleLaunching()

	if _, ok := fake.get("shard-stale-empty"); ok {
		t.Error("stale LAUNCHING row was not deleted")
	}
	if _, ok := fake.get("shard-fresh"); !ok {
		t.Error("fresh LAUNCHING row was deleted (reaper too aggressive)")
	}
}

// A LAUNCHING row older than staleLaunchingAge with a populated
// instance_id means the lifecycle hook stamped us but the instance
// never heartbeated. The reaper must signal the ASG to terminate
// that specific instance (which decrements desired_capacity) and
// DELETE the row.
func TestReaper_AbandonsStuckInstance(t *testing.T) {
	fake := newFakeRegistryStore()
	reg := NewShardRegistry()
	reg.SetStore(fake)

	stale := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	_ = fake.SaveShard(&store.ShardRecord{
		ID:          "shard-stuck",
		State:       string(StateLaunching),
		Tier:        "small",
		BucketID:    1,
		InstanceID:  "i-never-heartbeated",
		WindowStart: "2026-04-21T00:00:00Z", WindowEnd: "2026-04-22T00:00:00Z",
		CreatedAt: stale,
	})

	launcher := &countingLauncher{}
	prov := &Provisioner{
		registry: reg,
		launcher: launcher,
		config:   ProvisionerConfig{ShardOps: fake},
	}

	prov.reapStaleLaunching()

	if _, ok := fake.get("shard-stuck"); ok {
		t.Error("stuck LAUNCHING row was not deleted")
	}
	if launcher.terminated != "i-never-heartbeated" {
		t.Errorf("TerminateInstance: got id=%q want i-never-heartbeated", launcher.terminated)
	}
}

// countingLauncher captures LaunchInstance+TerminateInstance calls
// so tests can assert the reconciler/reaper's interaction with the
// ASG (or stand-in) without a real cloud. UsesLaunchingRows=true
// places it on the LAUNCHING-row code path, matching ASGLauncher.
type countingLauncher struct {
	mu         sync.Mutex
	addr       string
	launches   int
	terminated string // last-seen id
}

func (c *countingLauncher) LaunchInstance(_ context.Context, _ string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.launches++
	return c.addr, nil
}

func (c *countingLauncher) TerminateInstance(_ context.Context, id, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.terminated = id
	return nil
}

func (c *countingLauncher) UsesLaunchingRows() bool { return true }

func (c *countingLauncher) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.launches
}
