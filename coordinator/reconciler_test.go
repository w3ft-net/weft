package coordinator

import (
	"context"
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

	stub := &stubBuckets{bucket: &AppBucket{ID: defaultBucketID, Label: "default", Apps: []string{"weft"}, Tier: "small"}}
	prov := &Provisioner{registry: reg, Buckets: stub}

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
	def, _ := stub.Default()
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
// apply.)
func TestReconcile_UnderCapacity_AboveTargetNoOp(t *testing.T) {
	bucket := &AppBucket{ID: 1, Target: 2, Tier: "small"}
	snap := ReconcilerSnapshot{Bucket: bucket, LaunchingCount: 0, IngestingCount: 3, Now: time.Now()}

	for _, op := range reconcile(snap) {
		if _, ok := op.(LaunchShardOp); ok {
			t.Errorf("unexpected LaunchShardOp when capacity exceeds target: %s", op.Describe())
		}
	}
}

// Op 3 — window-closed: an INGESTING shard past its WindowEnd
// gets a RotateToReadOnlyOp. Zero windows (legacy/uninitialised)
// and non-INGESTING shards are skipped.

func TestReconcile_WindowClosed_EmitsRotateOp(t *testing.T) {
	now := time.Now().UTC()
	bucket := &AppBucket{ID: 1, Target: 1, Tier: "small"}
	entries := []*ShardEntry{
		{ID: "shard-past", BucketID: 1, State: StateIngesting,
			WindowStart: now.Add(-2 * time.Hour), WindowEnd: now.Add(-1 * time.Minute)},
	}
	snap := ReconcilerSnapshot{Bucket: bucket, Entries: entries, IngestingCount: 1, Now: now}

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
	stub := &stubBuckets{bucket: bucket}

	prov := NewProvisioner(reg, launcher, ProvisionerConfig{})
	prov.Buckets = stub

	if err := (LaunchShardOp{Bucket: bucket}).Apply(context.Background(), prov); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := reg.Count(); got != 1 {
		t.Errorf("want 1 shard after Apply, got %d", got)
	}
}

// Op 5 — retention-archive: a READ_ONLY shard whose WindowEnd is
// older than bucket.RetentionPeriod gets an ArchiveShardOp.

func TestReconcile_Retention_EmitsArchiveOp(t *testing.T) {
	now := time.Now().UTC()
	bucket := &AppBucket{ID: 1, Target: 0, RetentionPeriod: time.Hour, Tier: "small"}
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
	bucket := &AppBucket{ID: 1, RetentionPeriod: 2 * time.Hour, Tier: "small"}
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

// RetentionPeriod=0 means "archive disabled for this bucket" —
// no ArchiveShardOp should fire regardless of age.
func TestReconcile_Retention_ZeroRetentionDisables(t *testing.T) {
	now := time.Now().UTC()
	bucket := &AppBucket{ID: 1, RetentionPeriod: 0, Tier: "small"}
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
	bucket := &AppBucket{ID: 1, RetentionPeriod: time.Hour, Tier: "small"}
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

func describeOps(ops []ReconcileOp) []string {
	out := make([]string, 0, len(ops))
	for _, op := range ops {
		out = append(out, op.Describe())
	}
	return out
}
