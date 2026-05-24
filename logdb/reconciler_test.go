package logdb

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// reconcile() is a pure function: given a snapshot (bucket config +
// registry entries + in-flight LAUNCHING count), it returns the list
// of ops required to close the drift. Tests feed a snapshot and
// assert on the shape of the op list, no live Provisioner needed.

// Op 1 — app-drift: an INGESTING shard whose Apps is missing a
// bucket app yields one AddAppToShardOp per missing app.
func TestReconcile_AppDrift_EmitsOneOpPerMissingApp(t *testing.T) {
	bucket := &AppBucket{ID: 1, Label: "default", Apps: []string{"weft", "vimgym", "callbox"}, Tier: "small"}
	entries := []*ShardEntry{
		{ID: "shard-a", BucketID: 1, State: StateIngesting, Apps: []string{"weft"}},
	}
	snap := ReconcilerSnapshot{Bucket: bucket, Entries: entries, Now: time.Now()}

	ops := reconcile(snap)
	if len(ops) != 2 {
		t.Fatalf("want 2 ops (vimgym, callbox missing), got %d: %v", len(ops), describeOps(ops))
	}

	wanted := map[string]bool{"vimgym": true, "callbox": true}
	for _, op := range ops {
		add, ok := op.(AddAppToShardOp)
		if !ok {
			t.Errorf("unexpected op type %T: %s", op, op.Describe())
			continue
		}
		if add.ShardID != "shard-a" {
			t.Errorf("op shard: got %q want shard-a", add.ShardID)
		}
		if !wanted[add.App] {
			t.Errorf("unexpected app in op: %q", add.App)
		}
		delete(wanted, add.App)
	}
	if len(wanted) != 0 {
		t.Errorf("did not emit ops for apps: %v", wanted)
	}
}

// READ_ONLY and ARCHIVED shards never get app-drift ops — their
// apps list is frozen (they didn't accept writes for the new app).
func TestReconcile_AppDrift_SkipsNonIngestingStates(t *testing.T) {
	bucket := &AppBucket{ID: 1, Apps: []string{"weft", "vimgym"}, Tier: "small"}
	entries := []*ShardEntry{
		{ID: "shard-ro", BucketID: 1, State: StateReadOnly, Apps: []string{"weft"}},
		{ID: "shard-arch", BucketID: 1, State: StateArchived, Apps: []string{"weft"}},
	}
	snap := ReconcilerSnapshot{Bucket: bucket, Entries: entries, Now: time.Now()}

	if ops := reconcile(snap); len(ops) != 0 {
		t.Errorf("want 0 ops (READ_ONLY/ARCHIVED frozen), got %d: %v", len(ops), describeOps(ops))
	}
}

// An already-current INGESTING shard produces zero ops — reconcile
// must be idempotent across ticks.
func TestReconcile_AppDrift_NoOpWhenCurrent(t *testing.T) {
	bucket := &AppBucket{ID: 1, Apps: []string{"weft", "vimgym"}, Tier: "small"}
	entries := []*ShardEntry{
		{ID: "shard-a", BucketID: 1, State: StateIngesting, Apps: []string{"weft", "vimgym"}},
	}
	snap := ReconcilerSnapshot{Bucket: bucket, Entries: entries, Now: time.Now()}

	if ops := reconcile(snap); len(ops) != 0 {
		t.Errorf("want 0 ops, got %d: %v", len(ops), describeOps(ops))
	}
}

// AddAppToShardOp.Apply must be idempotent at the apply level too —
// re-running an apply that already ran is a no-op, and a shard that
// flipped to READ_ONLY since the snapshot is left alone.

func TestAddAppToShardOp_Apply_Idempotent(t *testing.T) {
	reg := NewShardRegistry()
	reg.Add(&ShardEntry{ID: "shard-a", State: StateIngesting, Apps: []string{"weft", "vimgym"}})
	prov := &Provisioner{registry: reg}

	if err := (AddAppToShardOp{ShardID: "shard-a", App: "vimgym"}).Apply(context.Background(), prov); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := reg.Get("shard-a").Apps
	if len(got) != 2 {
		t.Errorf("Apps changed on idempotent re-apply: got %v, want [weft vimgym]", got)
	}
}

// AddAppToShard's mutation must be lock-protected so a concurrent
// IngestingShard or MatchingEntries read doesn't see a torn Apps
// slice header. Run with -race (which the standard test invocation
// enables in CI); the assertion is that the test doesn't panic or
// trip the race detector.
func TestAddAppToShard_RaceSafeAgainstReaders(t *testing.T) {
	reg := NewShardRegistry()
	for i := 0; i < 4; i++ {
		reg.Add(&ShardEntry{
			ID:    fmt.Sprintf("shard-%d", i),
			State: StateIngesting,
			Shard: &failingShard{id: fmt.Sprintf("shard-%d", i)},
			Apps:  []string{"weft"},
		})
	}

	done := make(chan struct{})
	var wg sync.WaitGroup

	// Readers: repeatedly scan via IngestingShard — same path the
	// ingest drain uses to pick a target shard — under registry
	// RLock.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				_ = reg.IngestingShard("weft")
				_ = reg.MatchingEntries(TimeRange{}, servingFor(reg, "weft"), "bolt")
			}
		}()
	}

	// Writers: pound on AddAppToShard with a variety of apps across
	// all shards. Under the old direct-mutation code this tripped
	// go test -race; under the locked version it doesn't.
	apps := []string{"vimgym", "callbox", "grantcentral", "xylem", "weft"}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				shardID := fmt.Sprintf("shard-%d", j%4)
				app := apps[j%len(apps)]
				reg.AddAppToShard(shardID, app)
			}
		}(i)
	}

	time.Sleep(50 * time.Millisecond)
	close(done)
	wg.Wait()
}

func TestAddAppToShardOp_Apply_SkipsReadOnly(t *testing.T) {
	reg := NewShardRegistry()
	reg.Add(&ShardEntry{ID: "shard-ro", State: StateReadOnly, Apps: []string{"weft"}})
	prov := &Provisioner{registry: reg}

	if err := (AddAppToShardOp{ShardID: "shard-ro", App: "vimgym"}).Apply(context.Background(), prov); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := reg.Get("shard-ro").Apps
	if len(got) != 1 || got[0] != "weft" {
		t.Errorf("READ_ONLY shard was mutated: got %v", got)
	}
}

// End-to-end: OnboardApp persists the app to the bucket and calls
// reconcileBucket synchronously, so INGESTING shards in the bucket
// pick up the new app before OnboardApp returns.
func TestOnboardApp_ReconcilerPropagatesImmediately(t *testing.T) {
	reg := NewShardRegistry()

	reg.Add(&ShardEntry{ID: "ingest-1", BucketID: defaultBucketID, State: StateIngesting, Apps: []string{"weft"}, Shard: &failingShard{id: "ingest-1"}})
	reg.Add(&ShardEntry{ID: "readonly-1", BucketID: defaultBucketID, State: StateReadOnly, Apps: []string{"weft"}, Shard: &failingShard{id: "readonly-1"}})

	db := openSMTestStore(t)
	seedDefaultBucket(t, db, []string{"weft"}, 0, 0)
	bm := NewBucketManager(db)
	prov := &Provisioner{registry: reg, Buckets: bm}

	if err := prov.OnboardApp("xylem"); err != nil {
		t.Fatalf("OnboardApp: %v", err)
	}

	// INGESTING shard picked up xylem.
	if got := reg.Get("ingest-1").Apps; !containsApp(got, "xylem") {
		t.Errorf("INGESTING shard missing xylem after OnboardApp: got %v", got)
	}
	// READ_ONLY stayed frozen.
	if got := reg.Get("readonly-1").Apps; containsApp(got, "xylem") {
		t.Errorf("READ_ONLY shard was mutated: got %v", got)
	}
	// Bucket has the app too.
	def, _ := bm.Default()
	if !containsApp(def.Apps, "xylem") {
		t.Errorf("bucket missing xylem: got %v", def.Apps)
	}
}

// Op 2 — under-capacity: emit one LaunchShardOp per shard missing
// from the bucket's target. LAUNCHING counts just as much as
// INGESTING because the in-flight launch is an intent already
// committed to the DB.

func TestReconcile_UnderCapacity_EmitsLaunchOps(t *testing.T) {
	bucket := &AppBucket{ID: 1, Label: "default", Apps: []string{"weft"}, Target: 3, Tier: "small"}
	snap := ReconcilerSnapshot{Bucket: bucket, Entries: nil, LaunchingCount: 0, IngestingCount: 0, Now: time.Now()}

	ops := reconcile(snap)

	launchOps := 0
	for _, op := range ops {
		if _, ok := op.(LaunchShardOp); ok {
			launchOps++
		}
	}
	if launchOps != 3 {
		t.Errorf("want 3 LaunchShardOps (bucket target=3, capacity=0), got %d: %v", launchOps, describeOps(ops))
	}
}

// LAUNCHING + INGESTING together must meet the target. If LAUNCHING
// already covers the gap, no new launch ops should fire — this is
// the ratchet guard that §2 exists to prevent.
func TestReconcile_UnderCapacity_LaunchingCountsTowardTarget(t *testing.T) {
	bucket := &AppBucket{ID: 1, Apps: nil, Target: 2, Tier: "small"}
	entries := []*ShardEntry{
		{ID: "shard-live", BucketID: 1, State: StateIngesting},
	}
	snap := ReconcilerSnapshot{
		Bucket: bucket, Entries: entries,
		LaunchingCount: 1, IngestingCount: 1,
		Now: time.Now(),
	}

	for _, op := range reconcile(snap) {
		if _, ok := op.(LaunchShardOp); ok {
			t.Errorf("unexpected LaunchShardOp when 1 LAUNCHING + 1 INGESTING meets target 2: %s", op.Describe())
		}
	}
}

// At or above target, no launch ops. (The "above" case comes up
// naturally if two concurrent ticks both emit launches and both
// apply.) Op 2 derives IngestingCount from snap.Entries — the
// snapshot must carry a registry entry per INGESTING shard, which
// matches what snapshotForBucket produces in production.
func TestReconcile_UnderCapacity_AboveTargetNoOp(t *testing.T) {
	bucket := &AppBucket{ID: 1, Target: 2, Tier: "small"}
	entries := []*ShardEntry{
		{ID: "shard-a", BucketID: 1, State: StateIngesting},
		{ID: "shard-b", BucketID: 1, State: StateIngesting},
		{ID: "shard-c", BucketID: 1, State: StateIngesting},
	}
	snap := ReconcilerSnapshot{Bucket: bucket, Entries: entries, LaunchingCount: 0, Now: time.Now()}

	for _, op := range reconcile(snap) {
		if _, ok := op.(LaunchShardOp); ok {
			t.Errorf("unexpected LaunchShardOp when capacity exceeds target: %s", op.Describe())
		}
	}
}

// Op 3 — window-closed: an INGESTING shard past its WindowEnd
// gets a RotateToReadOnlyOp. Zero windows (legacy/uninitialised)
// and non-INGESTING shards are skipped.

// A past-window shard rotates once a replacement is already
// INGESTING (capacity-gated — see the Op 3 invariant tests below).
func TestReconcile_WindowClosed_EmitsRotateOp(t *testing.T) {
	now := time.Now().UTC()
	bucket := &AppBucket{ID: 1, Target: 1, Tier: "small", WindowDuration: time.Hour}
	entries := []*ShardEntry{
		{ID: "shard-past", BucketID: 1, State: StateIngesting,
			WindowStart: now.Add(-2 * time.Hour), WindowEnd: now.Add(-1 * time.Minute)},
		{ID: "shard-repl", BucketID: 1, State: StateIngesting,
			WindowStart: now.Add(-1 * time.Minute), WindowEnd: now.Add(59 * time.Minute)},
	}
	snap := ReconcilerSnapshot{Bucket: bucket, Entries: entries, IngestingCount: 2, Now: now}

	rotates := 0
	for _, op := range reconcile(snap) {
		if r, ok := op.(RotateToReadOnlyOp); ok {
			rotates++
			if r.ShardID != "shard-past" {
				t.Errorf("op shard: got %q want shard-past", r.ShardID)
			}
		}
	}
	if rotates != 1 {
		t.Errorf("want 1 RotateToReadOnlyOp, got %d: %v", rotates, describeOps(reconcile(snap)))
	}
}

func TestReconcile_WindowClosed_SkipsZeroWindow(t *testing.T) {
	now := time.Now().UTC()
	bucket := &AppBucket{ID: 1, Target: 1, Tier: "small"}
	entries := []*ShardEntry{
		{ID: "shard-zero", BucketID: 1, State: StateIngesting}, // zero WindowEnd
	}
	snap := ReconcilerSnapshot{Bucket: bucket, Entries: entries, IngestingCount: 1, Now: now}

	for _, op := range reconcile(snap) {
		if _, ok := op.(RotateToReadOnlyOp); ok {
			t.Errorf("unexpected RotateToReadOnlyOp for zero-window shard: %s", op.Describe())
		}
	}
}

func TestReconcile_WindowClosed_SkipsNonIngesting(t *testing.T) {
	now := time.Now().UTC()
	bucket := &AppBucket{ID: 1, Target: 1, Tier: "small"}
	entries := []*ShardEntry{
		{ID: "shard-ro", BucketID: 1, State: StateReadOnly,
			WindowStart: now.Add(-3 * time.Hour), WindowEnd: now.Add(-2 * time.Hour)},
	}
	snap := ReconcilerSnapshot{Bucket: bucket, Entries: entries, Now: now}

	for _, op := range reconcile(snap) {
		if _, ok := op.(RotateToReadOnlyOp); ok {
			t.Errorf("unexpected RotateToReadOnlyOp for already-READ_ONLY shard: %s", op.Describe())
		}
	}
}

func TestRotateToReadOnlyOp_Apply_Idempotent(t *testing.T) {
	reg := NewShardRegistry()
	reg.Add(&ShardEntry{ID: "shard-a", State: StateReadOnly, WindowEnd: time.Now().Add(-time.Minute)})
	prov := &Provisioner{registry: reg}

	if err := (RotateToReadOnlyOp{ShardID: "shard-a"}).Apply(context.Background(), prov); err != nil {
		t.Fatalf("Apply on already-READ_ONLY: %v", err)
	}
	if got := reg.Get("shard-a").State; got != StateReadOnly {
		t.Errorf("state: got %s want still READ_ONLY", got)
	}
}

// LaunchShardOp.Apply delegates to provisionForBucket. For sync
// launchers this means one launchAndConfigure + registry.Add per
// Apply call.
func TestLaunchShardOp_Apply_SyncLauncher_LaunchesOneShard(t *testing.T) {
	launcher := NewLocalLauncher()
	t.Cleanup(func() { launcher.Close() })

	reg := NewShardRegistry()
	bucket := &AppBucket{ID: 1, Label: "default", Apps: []string{"weft"}, Target: 1, Tier: "small"}

	prov := NewProvisioner(reg, launcher, ProvisionerConfig{})

	if err := (LaunchShardOp{Bucket: bucket}).Apply(context.Background(), prov); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := reg.Count(); got != 1 {
		t.Errorf("want 1 shard after Apply, got %d", got)
	}
}

// Op 5 — retention-archive: a READ_ONLY shard whose WindowEnd is
// older than bucket.WindowDuration gets an ArchiveShardOp.

func TestReconcile_Retention_EmitsArchiveOp(t *testing.T) {
	now := time.Now().UTC()
	bucket := &AppBucket{ID: 1, Target: 0, WindowDuration: time.Hour, Tier: "small"}
	entries := []*ShardEntry{
		{ID: "shard-ripe", BucketID: 1, State: StateReadOnly,
			WindowStart: now.Add(-3 * time.Hour), WindowEnd: now.Add(-2 * time.Hour)}, // 2h past, retention 1h
	}
	snap := ReconcilerSnapshot{Bucket: bucket, Entries: entries, Now: now}

	archives := 0
	for _, op := range reconcile(snap) {
		if a, ok := op.(ArchiveShardOp); ok {
			archives++
			if a.ShardID != "shard-ripe" {
				t.Errorf("op shard: got %q want shard-ripe", a.ShardID)
			}
		}
	}
	if archives != 1 {
		t.Errorf("want 1 ArchiveShardOp, got %d: %v", archives, describeOps(reconcile(snap)))
	}
}

func TestReconcile_Retention_WithinRetentionNoOp(t *testing.T) {
	now := time.Now().UTC()
	bucket := &AppBucket{ID: 1, WindowDuration: 2 * time.Hour, Tier: "small"}
	entries := []*ShardEntry{
		{ID: "shard-warm", BucketID: 1, State: StateReadOnly,
			WindowStart: now.Add(-90 * time.Minute), WindowEnd: now.Add(-30 * time.Minute)},
	}
	snap := ReconcilerSnapshot{Bucket: bucket, Entries: entries, Now: now}

	for _, op := range reconcile(snap) {
		if _, ok := op.(ArchiveShardOp); ok {
			t.Errorf("unexpected ArchiveShardOp for within-retention shard: %s", op.Describe())
		}
	}
}

// WindowDuration=0 means "archive disabled for this bucket" —
// no ArchiveShardOp should fire regardless of age.
func TestReconcile_Retention_ZeroRetentionDisables(t *testing.T) {
	now := time.Now().UTC()
	bucket := &AppBucket{ID: 1, WindowDuration: 0, Tier: "small"}
	entries := []*ShardEntry{
		{ID: "shard-ancient", BucketID: 1, State: StateReadOnly,
			WindowStart: now.Add(-10 * 24 * time.Hour), WindowEnd: now.Add(-9 * 24 * time.Hour)},
	}
	snap := ReconcilerSnapshot{Bucket: bucket, Entries: entries, Now: now}

	for _, op := range reconcile(snap) {
		if _, ok := op.(ArchiveShardOp); ok {
			t.Errorf("unexpected ArchiveShardOp when bucket retention=0: %s", op.Describe())
		}
	}
}

// Zero-window (legacy adoption) shards are skipped — we can't tell
// how stale they are.
func TestReconcile_Retention_SkipsZeroWindow(t *testing.T) {
	now := time.Now().UTC()
	bucket := &AppBucket{ID: 1, WindowDuration: time.Hour, Tier: "small"}
	entries := []*ShardEntry{
		{ID: "shard-nowindow", BucketID: 1, State: StateReadOnly}, // zero WindowEnd
	}
	snap := ReconcilerSnapshot{Bucket: bucket, Entries: entries, Now: now}

	for _, op := range reconcile(snap) {
		if _, ok := op.(ArchiveShardOp); ok {
			t.Errorf("unexpected ArchiveShardOp for zero-window shard: %s", op.Describe())
		}
	}
}

// Apply is a no-op when ParquetDir is unconfigured — coordinator
// isn't wired for export. Re-check on entry state handles the
// "already archived" race.
func TestArchiveShardOp_Apply_NoParquetDirNoOp(t *testing.T) {
	reg := NewShardRegistry()
	reg.Add(&ShardEntry{ID: "shard-x", State: StateReadOnly})
	prov := &Provisioner{registry: reg, config: ProvisionerConfig{}}

	if err := (ArchiveShardOp{ShardID: "shard-x"}).Apply(context.Background(), prov); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := reg.Get("shard-x").State; got != StateReadOnly {
		t.Errorf("state: got %s want still READ_ONLY (no ParquetDir)", got)
	}
}

// Op 6 — terminate-on-archive. After archive() has exported the
// shard's data and retired its EBS volume, the EC2 instance is
// still running; this op reclaims it via
// TerminateInstanceInAutoScalingGroup so the ASG's desired_capacity
// decrements atomically.

func TestReconcile_TerminateArchived_EmitsOpForArchivedWithInstanceID(t *testing.T) {
	bucket := &AppBucket{ID: 1, Target: 0, Tier: "small"}
	entries := []*ShardEntry{
		{ID: "shard-done", BucketID: 1, State: StateArchived, InstanceID: "i-dead-shard"},
	}
	snap := ReconcilerSnapshot{Bucket: bucket, Entries: entries, Now: time.Now()}

	terminates := 0
	for _, op := range reconcile(snap) {
		term, ok := op.(TerminateArchivedInstanceOp)
		if !ok {
			continue
		}
		terminates++
		if term.ShardID != "shard-done" {
			t.Errorf("op shard: got %q want shard-done", term.ShardID)
		}
	}
	if terminates != 1 {
		t.Errorf("want 1 TerminateArchivedInstanceOp, got %d: %v",
			terminates, describeOps(reconcile(snap)))
	}
}

// An ARCHIVED entry with no InstanceID has already been terminated
// (or was never on an instance we manage). No op should fire.
func TestReconcile_TerminateArchived_SkipsArchivedWithoutInstanceID(t *testing.T) {
	bucket := &AppBucket{ID: 1, Target: 0, Tier: "small"}
	entries := []*ShardEntry{
		{ID: "shard-clean", BucketID: 1, State: StateArchived, InstanceID: ""},
	}
	snap := ReconcilerSnapshot{Bucket: bucket, Entries: entries, Now: time.Now()}

	for _, op := range reconcile(snap) {
		if _, ok := op.(TerminateArchivedInstanceOp); ok {
			t.Errorf("unexpected op for already-nulled instance: %s", op.Describe())
		}
	}
}

// Non-ARCHIVED entries stay alone — terminate is only for shards
// whose data has already been exported.
func TestReconcile_TerminateArchived_SkipsLiveStates(t *testing.T) {
	bucket := &AppBucket{ID: 1, Target: 0, Tier: "small"}
	entries := []*ShardEntry{
		{ID: "shard-live", BucketID: 1, State: StateIngesting, InstanceID: "i-1"},
		{ID: "shard-ro", BucketID: 1, State: StateReadOnly, InstanceID: "i-2"},
	}
	snap := ReconcilerSnapshot{Bucket: bucket, Entries: entries, Now: time.Now()}

	for _, op := range reconcile(snap) {
		if _, ok := op.(TerminateArchivedInstanceOp); ok {
			t.Errorf("unexpected terminate op for live state: %s", op.Describe())
		}
	}
}

// Apply calls the launcher's TerminateInstance with the right id,
// then nulls entry.InstanceID + entry.Addr so the next tick doesn't
// re-emit.
func TestTerminateArchivedInstanceOp_Apply_CallsLauncherAndNullsFields(t *testing.T) {
	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID:         "shard-done",
		State:      StateArchived,
		InstanceID: "i-dead-shard",
		Addr:       "10.0.0.5:9000",
	})

	launcher := &countingLauncher{}
	prov := &Provisioner{registry: reg, launcher: launcher}

	if err := (TerminateArchivedInstanceOp{ShardID: "shard-done"}).Apply(context.Background(), prov); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if launcher.terminated != "i-dead-shard" {
		t.Errorf("TerminateInstance id: got %q want i-dead-shard", launcher.terminated)
	}
	entry := reg.Get("shard-done")
	if entry.InstanceID != "" {
		t.Errorf("InstanceID: got %q want empty (should be nulled post-terminate)", entry.InstanceID)
	}
	if entry.Addr != "" {
		t.Errorf("Addr: got %q want empty (should be nulled post-terminate)", entry.Addr)
	}
}

// If the entry has flipped away from ARCHIVED between snapshot and
// Apply, the op must not fire — the shard is no longer a candidate
// for reclamation.
func TestTerminateArchivedInstanceOp_Apply_StateChangedSinceSnapshot(t *testing.T) {
	reg := NewShardRegistry()
	// Snapshot would have had this entry as ARCHIVED; by Apply time
	// it's been rebound into INGESTING somehow.
	reg.Add(&ShardEntry{
		ID:         "shard-revived",
		State:      StateIngesting,
		InstanceID: "i-alive",
	})

	launcher := &countingLauncher{}
	prov := &Provisioner{registry: reg, launcher: launcher}

	if err := (TerminateArchivedInstanceOp{ShardID: "shard-revived"}).Apply(context.Background(), prov); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if launcher.terminated != "" {
		t.Errorf("TerminateInstance must NOT fire on non-ARCHIVED state; got terminated=%q", launcher.terminated)
	}
	if got := reg.Get("shard-revived").InstanceID; got != "i-alive" {
		t.Errorf("InstanceID should be untouched: got %q", got)
	}
}

// Apply for an already-nulled entry is a no-op — useful under
// retry or if two reconciler ticks race.
func TestTerminateArchivedInstanceOp_Apply_EmptyInstanceIDNoOp(t *testing.T) {
	reg := NewShardRegistry()
	reg.Add(&ShardEntry{ID: "shard-already-done", State: StateArchived, InstanceID: ""})

	launcher := &countingLauncher{}
	prov := &Provisioner{registry: reg, launcher: launcher}

	if err := (TerminateArchivedInstanceOp{ShardID: "shard-already-done"}).Apply(context.Background(), prov); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if launcher.terminated != "" {
		t.Errorf("TerminateInstance must NOT fire when InstanceID already empty; got %q", launcher.terminated)
	}
}

// Op 4 — pre-warm next window. The reconciler emits one
// PrewarmNextWindowOp per bucket.Target when any INGESTING shard's
// WindowEnd is within PrewarmLead of now AND no next-window
// LAUNCHING rows exist yet for this bucket. Async launchers only.

// withPrewarmLead temporarily overrides the package-scoped
// PrewarmLead, restoring on cleanup so tests don't leak global
// state.
func withPrewarmLead(t *testing.T, d time.Duration) {
	t.Helper()
	prev := PrewarmLead
	PrewarmLead = d
	t.Cleanup(func() { PrewarmLead = prev })
}

func TestShouldPrewarmNextWindow_TrueWhenWindowEndWithinLead(t *testing.T) {
	withPrewarmLead(t, 10*time.Minute)
	now := time.Now().UTC()
	bucket := &AppBucket{ID: 1, Tier: "small", WindowDuration: time.Hour}
	entries := []*ShardEntry{
		{ID: "s", State: StateIngesting, WindowEnd: now.Add(5 * time.Minute)}, // 5m away, within 10m lead
	}
	if !bucket.ShouldPrewarmNextWindow(now, entries) {
		t.Error("want ShouldPrewarmNextWindow=true (WindowEnd within PrewarmLead)")
	}
}

func TestShouldPrewarmNextWindow_FalseWhenWindowEndFar(t *testing.T) {
	withPrewarmLead(t, 10*time.Minute)
	now := time.Now().UTC()
	bucket := &AppBucket{ID: 1, Tier: "small", WindowDuration: time.Hour}
	entries := []*ShardEntry{
		{ID: "s", State: StateIngesting, WindowEnd: now.Add(time.Hour)},
	}
	if bucket.ShouldPrewarmNextWindow(now, entries) {
		t.Error("want false (WindowEnd way outside PrewarmLead)")
	}
}

func TestShouldPrewarmNextWindow_SkipsNonIngestingAndZeroWindow(t *testing.T) {
	withPrewarmLead(t, time.Hour)
	now := time.Now().UTC()
	bucket := &AppBucket{ID: 1, Tier: "small", WindowDuration: time.Hour}
	entries := []*ShardEntry{
		{ID: "ro", State: StateReadOnly, WindowEnd: now},                       // non-INGESTING
		{ID: "zero", State: StateIngesting, WindowEnd: time.Time{}},            // zero window
		{ID: "future", State: StateIngesting, WindowEnd: now.Add(24 * time.Hour)}, // outside lead
	}
	if bucket.ShouldPrewarmNextWindow(now, entries) {
		t.Error("want false: no INGESTING+valid-window entry within lead")
	}
}

// reconcile() emits PrewarmNextWindowOp when the bucket method
// returns true, respecting async-only gating and not re-emitting
// if next-window LAUNCHING rows already exist.
func TestReconcile_Prewarm_EmitsWhenBucketSaysYes(t *testing.T) {
	withPrewarmLead(t, 10*time.Minute)
	now := time.Now().UTC()
	bucket := &AppBucket{ID: 1, Target: 2, Tier: "small", WindowDuration: time.Hour}
	entries := []*ShardEntry{
		{ID: "ing", BucketID: 1, State: StateIngesting, WindowEnd: now.Add(5 * time.Minute)},
	}
	snap := ReconcilerSnapshot{
		Bucket: bucket, Entries: entries,
		IngestingCount: 1, FutureLaunchingCount: 0,
		IsAsyncLauncher: true,
		Now:             now,
	}

	prewarms := 0
	var seenStarts []time.Time
	for _, op := range reconcile(snap) {
		p, ok := op.(PrewarmNextWindowOp)
		if !ok {
			continue
		}
		prewarms++
		if p.Bucket == nil || p.Bucket.ID != 1 {
			t.Errorf("Bucket.ID: got %v want 1", p.Bucket)
		}
		seenStarts = append(seenStarts, p.WindowStart)
	}
	if prewarms != bucket.Target {
		t.Errorf("want %d PrewarmNextWindowOps (bucket.Target), got %d: %v",
			bucket.Target, prewarms, describeOps(reconcile(snap)))
	}
	// All ops should target the same next-window start.
	for i, s := range seenStarts {
		if i > 0 && !s.Equal(seenStarts[0]) {
			t.Errorf("op %d WindowStart=%v differs from first %v", i, s, seenStarts[0])
		}
	}
}

func TestReconcile_Prewarm_SkipsWhenFutureLaunchingExists(t *testing.T) {
	withPrewarmLead(t, 10*time.Minute)
	now := time.Now().UTC()
	bucket := &AppBucket{ID: 1, Target: 2, Tier: "small", WindowDuration: time.Hour}
	entries := []*ShardEntry{
		{ID: "ing", BucketID: 1, State: StateIngesting, WindowEnd: now.Add(5 * time.Minute)},
	}
	snap := ReconcilerSnapshot{
		Bucket: bucket, Entries: entries,
		IngestingCount: 1, FutureLaunchingCount: 2, // next-window rows already in flight
		IsAsyncLauncher: true,
		Now:             now,
	}

	for _, op := range reconcile(snap) {
		if _, ok := op.(PrewarmNextWindowOp); ok {
			t.Errorf("unexpected PrewarmNextWindowOp when FutureLaunchingCount > 0: %s", op.Describe())
		}
	}
}

func TestReconcile_Prewarm_SkipsForSyncLauncher(t *testing.T) {
	withPrewarmLead(t, 10*time.Minute)
	now := time.Now().UTC()
	bucket := &AppBucket{ID: 1, Target: 2, Tier: "small", WindowDuration: time.Hour}
	entries := []*ShardEntry{
		{ID: "ing", BucketID: 1, State: StateIngesting, WindowEnd: now.Add(5 * time.Minute)},
	}
	snap := ReconcilerSnapshot{
		Bucket: bucket, Entries: entries,
		IngestingCount:  1,
		IsAsyncLauncher: false, // sync launcher — no LAUNCHING row dance
		Now:             now,
	}

	for _, op := range reconcile(snap) {
		if _, ok := op.(PrewarmNextWindowOp); ok {
			t.Errorf("unexpected PrewarmNextWindowOp for sync launcher: %s", op.Describe())
		}
	}
}

// Apply inserts a LAUNCHING row via mintLaunchingRowForWindow with
// the op's window bounds.
func TestPrewarmNextWindowOp_Apply_InsertsLaunchingRowForNextWindow(t *testing.T) {
	fake := newFakeRegistryStore()
	reg := NewShardRegistry()
	reg.SetStore(fake)

	bucket := &AppBucket{ID: 1, Label: "default", Tier: "small", WindowDuration: time.Hour}

	prov := &Provisioner{
		registry: reg,
		config:   ProvisionerConfig{ShardOps: fake},
	}

	start := time.Date(2026, 4, 21, 14, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	op := PrewarmNextWindowOp{Bucket: bucket, WindowStart: start, WindowEnd: end}
	if err := op.Apply(context.Background(), prov); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Exactly one new LAUNCHING row, in the right bucket and window.
	rows := 0
	for _, r := range fake.records {
		if r.State != string(StateLaunching) {
			continue
		}
		rows++
		if r.BucketID != bucket.ID {
			t.Errorf("inserted row bucket=%d want %d", r.BucketID, bucket.ID)
		}
		if r.WindowStart != start.Format(time.RFC3339) {
			t.Errorf("inserted row WindowStart=%q want %q", r.WindowStart, start.Format(time.RFC3339))
		}
		if r.WindowEnd != end.Format(time.RFC3339) {
			t.Errorf("inserted row WindowEnd=%q want %q", r.WindowEnd, end.Format(time.RFC3339))
		}
		if r.InstanceID != "" {
			t.Errorf("inserted row InstanceID=%q, want empty (filled by lifecycle hook)", r.InstanceID)
		}
	}
	if rows != 1 {
		t.Errorf("want 1 new LAUNCHING row, got %d", rows)
	}
}

// Op 3 — stale window span: an INGESTING shard whose window span
// exceeds the bucket's current WindowDuration must rotate even if
// its WindowEnd is in the future. This catches legacy shards
// created under a wider window config.
func TestReconcile_WindowClosed_RotatesStaleWindowSpan(t *testing.T) {
	now := time.Now().UTC()
	bucket := &AppBucket{ID: 1, Target: 1, Tier: "small", WindowDuration: time.Hour}
	entries := []*ShardEntry{
		{ID: "shard-7d", BucketID: 1, State: StateIngesting,
			WindowStart: now.Add(-24 * time.Hour),
			WindowEnd:   now.Add(6 * 24 * time.Hour)}, // 7-day span, bucket wants 1h
		// Replacement already INGESTING so the stale shard may rotate
		// without dropping the bucket below Target.
		{ID: "shard-repl", BucketID: 1, State: StateIngesting,
			WindowStart: now.Add(-1 * time.Minute), WindowEnd: now.Add(59 * time.Minute)},
	}
	snap := ReconcilerSnapshot{Bucket: bucket, Entries: entries, IngestingCount: 2, Now: now}

	rotates := 0
	for _, op := range reconcile(snap) {
		if _, ok := op.(RotateToReadOnlyOp); ok {
			rotates++
		}
	}
	if rotates != 1 {
		t.Errorf("want 1 RotateToReadOnlyOp for stale-span shard, got %d: %v",
			rotates, describeOps(reconcile(snap)))
	}
}

// A shard whose window span matches the bucket's WindowDuration and
// whose WindowEnd is still in the future must NOT rotate.
func TestReconcile_WindowClosed_CurrentSpanNoRotation(t *testing.T) {
	now := time.Now().UTC()
	bucket := &AppBucket{ID: 1, Target: 1, Tier: "small", WindowDuration: time.Hour}
	entries := []*ShardEntry{
		{ID: "shard-ok", BucketID: 1, State: StateIngesting,
			WindowStart: now.Add(-30 * time.Minute),
			WindowEnd:   now.Add(30 * time.Minute)}, // 1h span, matching bucket
	}
	snap := ReconcilerSnapshot{Bucket: bucket, Entries: entries, IngestingCount: 1, Now: now}

	for _, op := range reconcile(snap) {
		if _, ok := op.(RotateToReadOnlyOp); ok {
			t.Errorf("unexpected rotation for current-span shard: %s", op.Describe())
		}
	}
}

// ── Op 3 capacity-gate invariant ─────────────────────────────────
//
// Safety invariant: a shard must never leave INGESTING (window-closed
// OR stale-span) if doing so drops the bucket below Target INGESTING
// shards. Rotation is blocked until a replacement has launched AND
// reached INGESTING — otherwise a window roll opens a write-gap with
// no shard accepting ingest for the bucket.

// The sole INGESTING shard, past its window, with no replacement yet,
// must NOT rotate — that would leave the bucket with zero ingest
// capacity.
func TestReconcile_WindowClosed_DoesNotRotateBelowTarget(t *testing.T) {
	now := time.Now().UTC()
	bucket := &AppBucket{ID: 1, Target: 1, Tier: "small"}
	entries := []*ShardEntry{
		{ID: "shard-only", BucketID: 1, State: StateIngesting,
			WindowStart: now.Add(-2 * time.Hour), WindowEnd: now.Add(-1 * time.Minute)},
	}
	snap := ReconcilerSnapshot{Bucket: bucket, Entries: entries, IngestingCount: 1, Now: now}

	for _, op := range reconcile(snap) {
		if _, ok := op.(RotateToReadOnlyOp); ok {
			t.Errorf("rotated the only INGESTING shard with no replacement — write-gap: %s", op.Describe())
		}
	}
}

// Once a replacement is INGESTING (Target=1, two INGESTING: one
// past-window "old", one healthy "new"), the old one rotates and the
// new one stays — exactly one rotate, and it must be the old shard.
func TestReconcile_WindowClosed_RotatesOldOnceReplacementIngesting(t *testing.T) {
	now := time.Now().UTC()
	bucket := &AppBucket{ID: 1, Target: 1, Tier: "small", WindowDuration: time.Hour}
	entries := []*ShardEntry{
		{ID: "shard-old", BucketID: 1, State: StateIngesting,
			WindowStart: now.Add(-90 * time.Minute), WindowEnd: now.Add(-30 * time.Minute)},
		{ID: "shard-new", BucketID: 1, State: StateIngesting,
			WindowStart: now.Add(-1 * time.Minute), WindowEnd: now.Add(59 * time.Minute)},
	}
	snap := ReconcilerSnapshot{Bucket: bucket, Entries: entries, IngestingCount: 2, Now: now}

	var rotated []string
	for _, op := range reconcile(snap) {
		if r, ok := op.(RotateToReadOnlyOp); ok {
			rotated = append(rotated, r.ShardID)
		}
	}
	if len(rotated) != 1 || rotated[0] != "shard-old" {
		t.Errorf("want exactly [shard-old] rotated, got %v: %v", rotated, describeOps(reconcile(snap)))
	}
}

// Stale-span rotation is gated the same way: two stale-span INGESTING
// shards, Target=2, no replacements → zero rotations (rotating any
// drops below Target).
func TestReconcile_WindowClosed_StaleSpanGatedByTarget(t *testing.T) {
	now := time.Now().UTC()
	bucket := &AppBucket{ID: 1, Target: 2, Tier: "small", WindowDuration: time.Hour}
	entries := []*ShardEntry{
		{ID: "stale-a", BucketID: 1, State: StateIngesting,
			WindowStart: now.Add(-24 * time.Hour), WindowEnd: now.Add(6 * 24 * time.Hour)},
		{ID: "stale-b", BucketID: 1, State: StateIngesting,
			WindowStart: now.Add(-24 * time.Hour), WindowEnd: now.Add(6 * 24 * time.Hour)},
	}
	snap := ReconcilerSnapshot{Bucket: bucket, Entries: entries, IngestingCount: 2, Now: now}

	for _, op := range reconcile(snap) {
		if _, ok := op.(RotateToReadOnlyOp); ok {
			t.Errorf("rotated a stale shard below Target=2 — write-gap: %s", op.Describe())
		}
	}
}

// When more INGESTING shards are eligible than the budget allows,
// rotate only down to Target and rotate the oldest-WindowEnd first
// (keep the freshest hot). 3 past-window INGESTING, Target=1 → rotate
// exactly 2 (the two oldest), leaving the newest INGESTING.
func TestReconcile_WindowClosed_RotatesOldestFirstDownToTarget(t *testing.T) {
	now := time.Now().UTC()
	bucket := &AppBucket{ID: 1, Target: 1, Tier: "small"}
	entries := []*ShardEntry{
		{ID: "p-newest", BucketID: 1, State: StateIngesting,
			WindowStart: now.Add(-70 * time.Minute), WindowEnd: now.Add(-1 * time.Minute)},
		{ID: "p-oldest", BucketID: 1, State: StateIngesting,
			WindowStart: now.Add(-3 * time.Hour), WindowEnd: now.Add(-3 * time.Minute)},
		{ID: "p-middle", BucketID: 1, State: StateIngesting,
			WindowStart: now.Add(-2 * time.Hour), WindowEnd: now.Add(-2 * time.Minute)},
	}
	snap := ReconcilerSnapshot{Bucket: bucket, Entries: entries, IngestingCount: 3, Now: now}

	rotated := map[string]bool{}
	for _, op := range reconcile(snap) {
		if r, ok := op.(RotateToReadOnlyOp); ok {
			rotated[r.ShardID] = true
		}
	}
	if len(rotated) != 2 || !rotated["p-oldest"] || !rotated["p-middle"] {
		t.Errorf("want exactly {p-oldest,p-middle} rotated (oldest-first, leave 1 for Target), got %v: %v",
			rotated, describeOps(reconcile(snap)))
	}
}

func describeOps(ops []ReconcileOp) []string {
	out := make([]string, 0, len(ops))
	for _, op := range ops {
		out = append(out, op.Describe())
	}
	return out
}
