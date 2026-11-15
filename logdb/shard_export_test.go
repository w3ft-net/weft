//go:build test

package logdb

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/shardstore"

	"github.com/w3ft-net/weft/shard"
)

// ── fakes ───────────────────────────────────────────────────────────

type fakeExportOps struct {
	mu sync.Mutex

	snapshotID string // returned by CreateSnapshot; "snap-1" if empty
	snapState  ExportSnapshotState
	instanceID string // returned by LaunchInstance; "i-export-1" if empty
	instState  string

	createErr, launchErr, cleanupErr error
	snapErr                          error // returned by SnapshotState when set

	createCalls  []SnapshotSpec
	launchCalls  []ExportSpec
	cleanupCalls []string
	rec          *eventLog // shared with the output deleter to assert ordering
}

// eventLog records the order of side effects across fakes.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (e *eventLog) add(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, s)
}

func (e *eventLog) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.events...)
}

// fakeOutputDeleter is the ObjectDeleter the failed-attempt cleanup uses.
type fakeOutputDeleter struct {
	mu    sync.Mutex
	paths []string
	err   error
	rec   *eventLog
}

func (f *fakeOutputDeleter) DeleteParquet(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rec != nil {
		f.rec.add("delete:" + path)
	}
	if f.err != nil {
		return f.err
	}
	f.paths = append(f.paths, path)
	return nil
}

func (f *fakeExportOps) CreateSnapshot(_ context.Context, s SnapshotSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls = append(f.createCalls, s)
	if f.createErr != nil {
		return "", f.createErr
	}
	if f.snapshotID == "" {
		return "snap-1", nil
	}
	return f.snapshotID, nil
}

func (f *fakeExportOps) SnapshotState(context.Context, string) (ExportSnapshotState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.snapErr != nil {
		return ExportSnapshotState{}, f.snapErr
	}
	return f.snapState, nil
}

func (f *fakeExportOps) LaunchInstance(_ context.Context, s ExportSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launchCalls = append(f.launchCalls, s)
	if f.launchErr != nil {
		return "", f.launchErr
	}
	if f.instanceID == "" {
		return "i-export-1", nil
	}
	return f.instanceID, nil
}

func (f *fakeExportOps) InstanceState(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.instState, nil
}

func (f *fakeExportOps) Cleanup(_ context.Context, shardID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanupCalls = append(f.cleanupCalls, shardID)
	if f.rec != nil {
		f.rec.add("cleanup")
	}
	return f.cleanupErr
}

type fakeManifestReader struct {
	mu        sync.Mutex
	manifests map[string]shard.ExportManifest
	err       error
	gets      int
	// appearOnGet, when > 0, hides the manifest until the Nth Get: it models
	// an export that writes its manifest between two coordinator polls.
	appearOnGet int
}

func (f *fakeManifestReader) Get(_ context.Context, key string) (shard.ExportManifest, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.err != nil {
		return shard.ExportManifest{}, false, f.err
	}
	if f.appearOnGet > 0 && f.gets < f.appearOnGet {
		return shard.ExportManifest{}, false, nil
	}
	m, ok := f.manifests[key]
	return m, ok, nil
}

// ── fixture ─────────────────────────────────────────────────────────

const testExportManifestKey = "s3://weft/warp/_manifests/s-test.json"

type exportFixture struct {
	sm       *Shard
	db       shardstore.Store
	ss       shardstore.Store
	ops      *fakeExportOps
	outputs  *fakeOutputDeleter
	events   *eventLog
	manifest *fakeManifestReader
	launcher *fakeLauncher
	volumes  *fakeVolumeManager
	live     *fakeArchivableShard
	told     *recordingRetention
	clock    time.Time // the fixture's fixed reference time
	now      time.Time // what the shard's clock reads; tests advance it
}

func newExportFixture(t *testing.T, customize ...func(*ShardDeps)) *exportFixture {
	t.Helper()
	events := &eventLog{}
	f := &exportFixture{
		events:   events,
		outputs:  &fakeOutputDeleter{rec: events},
		ops:      &fakeExportOps{snapState: ExportSnapshotState{State: "pending"}, instState: "pending", rec: events},
		manifest: &fakeManifestReader{manifests: map[string]shard.ExportManifest{}},
		launcher: &fakeLauncher{},
		volumes:  &fakeVolumeManager{},
		live:     &fakeArchivableShard{id: "s-test", archivePath: "s3://in-process-must-not-run"},
		told:     &recordingRetention{},
		clock:    time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC),
	}
	f.now = f.clock
	f.sm, f.db, _ = makeFixtureShardSM(t, StateArchiving, func(d *ShardDeps) {
		d.Clock = func() time.Time { return f.now }
		d.Launcher = f.launcher
		d.Volumes = f.volumes
		d.ParquetDir = "s3://weft"
		d.LambdaFunctionURL = "https://fn.example/" // non-empty → ColdShard skips the duckdb open
		d.Exports = f.ops
		d.ExportManifests = f.manifest
		d.ExportOutputs = f.outputs
		d.Retention = f.told
		for _, c := range customize {
			c(d)
		}
	})
	f.ss = f.db
	f.sm.Live = f.live
	f.sm.Services = []shardstore.ShardService{{NamespaceID: 1, Service: "warp"}}
	f.sm.EBSVolumeID, f.sm.InstanceID, f.sm.AZ, f.sm.RowCount = "vol-1", "i-shard", "us-east-2a", 1000
	if err := f.ss.SetShardLaunchResult("s-test", "i-shard", "us-east-2a", "vol-1", nil); err != nil {
		t.Fatalf("seed launch result: %v", err)
	}
	f.heartbeat(t, 3<<30) // 3 GiB: the smallest export size class
	return f
}

// heartbeat records a heartbeat whose disk footprint drives the instance sizing.
func (f *exportFixture) heartbeat(t *testing.T, diskBytes int64) {
	t.Helper()
	if err := f.ss.TouchHeartbeat("s-test", 0, f.clock.Add(-time.Minute), "", diskBytes); err != nil {
		t.Fatalf("seed heartbeat: %v", err)
	}
}

// occupyASlot gives another shard an export attempt in flight, so the cap sees
// it. The cap is generous for this setup claim; the test's own cap is what bites.
func (f *exportFixture) occupyASlot(t *testing.T, id string) {
	t.Helper()
	bucket := seedBucket(t, f.db, BucketStasis)
	if err := f.ss.SaveShard(&ShardRow{
		ID:          id,
		State:       StateArchiving,
		BucketID:    bucket.ID,
		Tier:        "small",
		WindowStart: f.clock,
		WindowEnd:   f.clock,
		CreatedAt:   f.clock,
	}); err != nil {
		t.Fatal(err)
	}
	if claim, err := f.ss.BeginExportAttempt(id, 0, "r7g.large", f.clock, 100); err != nil || claim != ExportClaimWon {
		t.Fatalf("occupy a slot: %v err=%v", claim, err)
	}
}

// seedAttempt puts the shard mid-export: row and entry both, as the writers do.
func (f *exportFixture) seedAttempt(t *testing.T, attempt int, started time.Time, snap, inst string) {
	t.Helper()
	if attempt > 0 {
		row, _, err := f.db.GetShard("s-test")
		if err != nil {
			t.Fatalf("seed attempt: %v", err)
		}
		row.Export.Attempt, row.Export.InstanceType, row.Export.StartedAt = attempt, "r7g.large", &started
		if err := f.db.SaveShard(row); err != nil {
			t.Fatalf("seed attempt: %v", err)
		}
	}
	if snap != "" {
		_ = f.ss.SetExportSnapshot("s-test", snap)
	}
	if inst != "" {
		_ = f.ss.SetExportInstance("s-test", inst)
	}
	f.sm.mu.Lock()
	f.sm.Export.Attempt, f.sm.Export.SnapshotID, f.sm.Export.Instance = attempt, snap, inst
	if attempt > 0 {
		f.sm.Export.InstanceType = "r7g.large"
		f.sm.Export.StartedAt = &started
	}
	f.sm.mu.Unlock()
}

func (f *exportFixture) evaluate(t *testing.T) {
	t.Helper()
	if err := f.sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
}

func (f *exportFixture) row(t *testing.T) *ShardRow {
	t.Helper()
	return getShardRow(t, f.ss)
}

func (f *exportFixture) state(t *testing.T) ShardState {
	t.Helper()
	st, _ := f.sm.smState(context.Background())
	return st
}

func (f *exportFixture) assertDataIntact(t *testing.T) {
	t.Helper()
	if len(f.volumes.deleted) != 0 {
		t.Fatalf("volume deleted before the export was recorded: %v", f.volumes.deleted)
	}
	if len(f.launcher.terminated) != 0 {
		t.Fatalf("shard instance terminated before the export was recorded: %v", f.launcher.terminated)
	}
	if f.state(t) != StateArchiving {
		t.Fatalf("state = %q, want ARCHIVING", f.state(t))
	}
}

func goodManifest(attempt int) shard.ExportManifest {
	t0 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	return shard.ExportManifest{
		ShardID: "s-test", Attempt: attempt, ParquetPath: "s3://weft/warp/s-test/**/*.parquet", Rows: 10,
		Partitions: []shard.PartitionStat{{PartitionValues: []string{"5"}, MinTs: t0, MaxTs: t0.Add(time.Hour), RowCount: 7}, {PartitionValues: []string{"6"}, MinTs: t0, MaxTs: t0, RowCount: 3}},
	}
}

// ── sizing ──────────────────────────────────────────────────────────

func TestExportInstanceType(t *testing.T) {
	d := &ShardDeps{}
	const gib = int64(1) << 30
	small, mid, big := defaultExportSizing[0].InstanceType, defaultExportSizing[1].InstanceType, defaultExportSizing[len(defaultExportSizing)-1].InstanceType
	cases := []struct {
		name    string
		bytes   int64
		attempt int
		want    string
	}{
		{"tiny", 1 * gib, 1, small},
		{"at the first boundary", defaultExportSizing[0].MaxDBBytes, 1, small},
		{"just over the first boundary", defaultExportSizing[0].MaxDBBytes + 1, 1, mid},
		{"huge", 500 * gib, 1, big},
		{"unknown size takes the largest: never guess small", 0, 1, big},
		{"a retry steps up one class", 1 * gib, 2, mid},
		{"a retry cannot step past the largest", 500 * gib, 3, big},
	}
	for _, c := range cases {
		got, err := d.exportInstanceType(c.bytes, c.attempt)
		if err != nil || got != c.want {
			t.Errorf("%s: exportInstanceType(%d, %d) = %q %v, want %q", c.name, c.bytes, c.attempt, got, err, c.want)
		}
	}

	custom := &ShardDeps{ExportSizing: []ExportSize{{MaxDBBytes: 10, InstanceType: "x.small"}, {MaxDBBytes: 1 << 62, InstanceType: "x.big"}}}
	if got, _ := custom.exportInstanceType(5, 1); got != "x.small" {
		t.Errorf("configured table ignored: got %q", got)
	}
}

// ── phases ──────────────────────────────────────────────────────────

func TestShardExport_FirstTickClaimsAttemptAndSnapshots(t *testing.T) {
	f := newExportFixture(t)
	f.evaluate(t)

	row := f.row(t)
	if row.Export.Attempt != 1 || row.Export.StartedAt == nil || row.Export.InstanceType != defaultExportSizing[0].InstanceType {
		t.Fatalf("attempt not claimed with a derived size: %+v", row)
	}
	if len(f.ops.createCalls) != 1 {
		t.Fatalf("CreateSnapshot calls = %d, want 1", len(f.ops.createCalls))
	}
	if got := f.ops.createCalls[0]; got.ShardID != "s-test" || got.Attempt != 1 || got.VolumeID != "vol-1" {
		t.Fatalf("snapshot spec = %+v", got)
	}
	if row.Export.SnapshotID != "snap-1" {
		t.Fatalf("snapshot not persisted: %q", row.Export.SnapshotID)
	}
	if f.sm.Export.Attempt != 1 || f.sm.Export.SnapshotID != "snap-1" {
		t.Fatalf("entry not mirrored (a later SaveShard would write stale facts back): attempt=%d snap=%q", f.sm.Export.Attempt, f.sm.Export.SnapshotID)
	}
	if f.live.archived {
		t.Fatal("the in-process Archive ran; the export must happen off the shard's instance")
	}
	if len(f.ops.launchCalls) != 0 {
		t.Fatal("launched an instance before the snapshot completed")
	}
	f.assertDataIntact(t)
}

func TestShardExport_SnapshotPendingWaits(t *testing.T) {
	f := newExportFixture(t)
	f.seedAttempt(t, 1, f.clock, "snap-1", "")
	f.evaluate(t)
	if len(f.ops.launchCalls) != 0 || len(f.ops.cleanupCalls) != 0 {
		t.Fatalf("pending snapshot: launches=%v cleanups=%v, want neither", f.ops.launchCalls, f.ops.cleanupCalls)
	}
	f.assertDataIntact(t)
}

func TestShardExport_SnapshotCompletedLaunchesTheExportInstance(t *testing.T) {
	f := newExportFixture(t)
	f.seedAttempt(t, 2, f.clock, "snap-9", "")
	f.ops.snapState = ExportSnapshotState{State: "completed"}
	f.evaluate(t)

	if len(f.ops.launchCalls) != 1 {
		t.Fatalf("LaunchInstance calls = %d, want 1", len(f.ops.launchCalls))
	}
	got := f.ops.launchCalls[0]
	if got.ShardID != "s-test" || got.Attempt != 2 || got.SnapshotID != "snap-9" || got.InstanceType != "r7g.large" ||
		got.AZ != "us-east-2a" || got.Prefix != "s3://weft/warp" {
		t.Fatalf("launch spec = %+v", got)
	}
	if f.row(t).Export.Instance != "i-export-1" || f.sm.Export.Instance != "i-export-1" {
		t.Fatal("export instance not persisted and mirrored")
	}
	f.assertDataIntact(t)
}

func TestShardExport_SnapshotErrorFailsTheAttempt(t *testing.T) {
	f := newExportFixture(t)
	f.seedAttempt(t, 1, f.clock, "snap-1", "")
	f.ops.snapState = ExportSnapshotState{State: "error"}
	f.evaluate(t)

	if len(f.ops.cleanupCalls) != 1 || f.ops.cleanupCalls[0] != "s-test" {
		t.Fatalf("cleanup calls = %v, want one for s-test", f.ops.cleanupCalls)
	}
	row := f.row(t)
	if row.Export.SnapshotID != "" || row.Export.StartedAt != nil {
		t.Fatalf("facts not cleared after a failed attempt: %+v", row)
	}
	if row.Export.Attempt != 1 {
		t.Fatalf("attempt counter = %d, want 1 (it must survive the clear)", row.Export.Attempt)
	}
	f.assertDataIntact(t)

	// The next tick starts attempt 2 and steps up one size class.
	f.ops.snapState = ExportSnapshotState{State: "pending"}
	f.evaluate(t)
	row = f.row(t)
	if row.Export.Attempt != 2 || row.Export.InstanceType != defaultExportSizing[1].InstanceType {
		t.Fatalf("retry: attempt=%d type=%q, want attempt 2 one size class up", row.Export.Attempt, row.Export.InstanceType)
	}
}

func TestShardExport_WaitsForTheManifestWhileTheInstanceRuns(t *testing.T) {
	f := newExportFixture(t)
	f.seedAttempt(t, 1, f.clock, "snap-1", "i-export-1")
	f.ops.snapState = ExportSnapshotState{State: "completed"}
	f.ops.instState = "running"
	f.evaluate(t)

	if len(f.ops.cleanupCalls) != 0 || len(f.ops.launchCalls) != 0 {
		t.Fatalf("running export disturbed: cleanups=%v launches=%v", f.ops.cleanupCalls, f.ops.launchCalls)
	}
	f.assertDataIntact(t)
}

func TestShardExport_ManifestRecordsArchiveThenRetiresTheShard(t *testing.T) {
	f := newExportFixture(t)
	f.seedAttempt(t, 1, f.clock, "snap-1", "i-export-1")
	f.ops.snapState = ExportSnapshotState{State: "completed"}
	f.ops.instState = "terminated" // the host powers itself off after writing the manifest
	f.manifest.manifests[testExportManifestKey] = goodManifest(1)

	f.evaluate(t)

	row := f.row(t)
	if row.ParquetPath != "s3://weft/warp/s-test/**/*.parquet" {
		t.Fatalf("parquet path = %q", row.ParquetPath)
	}
	if len(f.told.stats) != 2 || f.told.stats[0].RowCount != 7 {
		t.Fatalf("retention told of stats = %+v", f.told.stats)
	}
	if !f.live.closed {
		t.Fatal("live shard not closed after the ColdShard swap")
	}
	if _, ok := f.sm.Live.(*shard.ColdShard); !ok {
		t.Fatalf("Live = %T, want a ColdShard", f.sm.Live)
	}
	if len(f.ops.cleanupCalls) == 0 {
		t.Fatal("export resources not cleaned up")
	}
	if row.Export.SnapshotID != "" || row.Export.Instance != "" || row.Export.StartedAt != nil {
		t.Fatalf("export facts not cleared: %+v", row)
	}
	// The shard's own volume and instance are retired only now, and the shard
	// completes in the same pass.
	if len(f.volumes.deleted) != 1 || len(f.launcher.terminated) != 1 || f.launcher.terminated[0] != "i-shard" {
		t.Fatalf("shard resources: deleted=%v terminated=%v", f.volumes.deleted, f.launcher.terminated)
	}
	if f.state(t) != StateArchived {
		t.Fatalf("state = %q, want ARCHIVED", f.state(t))
	}
	if f.live.archived {
		t.Fatal("the in-process Archive ran")
	}
}

func TestShardExport_IgnoresAStaleManifest(t *testing.T) {
	f := newExportFixture(t)
	f.seedAttempt(t, 2, f.clock, "snap-2", "i-export-2")
	f.ops.snapState = ExportSnapshotState{State: "completed"}
	f.ops.instState = "running"
	f.manifest.manifests[testExportManifestKey] = goodManifest(1) // left by attempt 1

	f.evaluate(t)

	if f.row(t).ParquetPath != "" {
		t.Fatal("recorded attempt 1's manifest as attempt 2's result")
	}
	f.assertDataIntact(t)
}

func TestShardExport_InstanceEndedWithoutManifestFailsTheAttempt(t *testing.T) {
	f := newExportFixture(t)
	f.seedAttempt(t, 1, f.clock, "snap-1", "i-export-1")
	f.ops.snapState = ExportSnapshotState{State: "completed"}
	f.ops.instState = "terminated"

	f.evaluate(t)

	if len(f.ops.cleanupCalls) != 1 {
		t.Fatalf("cleanup calls = %v", f.ops.cleanupCalls)
	}
	row := f.row(t)
	if row.Export.Instance != "" || row.Export.SnapshotID != "" || row.Export.Attempt != 1 {
		t.Fatalf("after a failed attempt: %+v", row)
	}
	f.assertDataIntact(t)
}

// An export that writes its manifest and then terminates between the
// coordinator's manifest poll and its instance-state poll must not be failed.
func TestShardExport_ManifestAppearingAsTheInstanceEndsIsASuccess(t *testing.T) {
	f := newExportFixture(t)
	f.seedAttempt(t, 1, f.clock, "snap-1", "i-export-1")
	f.ops.snapState = ExportSnapshotState{State: "completed"}
	f.ops.instState = "terminated"
	f.manifest.manifests[testExportManifestKey] = goodManifest(1)
	f.manifest.appearOnGet = 2 // first poll misses; the re-check after seeing the dead instance finds it

	f.evaluate(t)

	if f.row(t).ParquetPath == "" {
		t.Fatal("a successful export was failed because its manifest landed a moment late")
	}
	if f.state(t) != StateArchived {
		t.Fatalf("state = %q, want ARCHIVED", f.state(t))
	}
}

func TestShardExport_UnknownInstanceStateWaitsRatherThanFails(t *testing.T) {
	// EC2 can briefly not know an instance it just launched; that must not
	// fail the attempt. A vanished instance is caught by the deadline.
	f := newExportFixture(t)
	f.seedAttempt(t, 1, f.clock, "snap-1", "i-export-1")
	f.ops.snapState = ExportSnapshotState{State: "completed"}
	f.ops.instState = ""

	f.evaluate(t)

	if len(f.ops.cleanupCalls) != 0 {
		t.Fatalf("failed an attempt over an instance EC2 did not know yet: %v", f.ops.cleanupCalls)
	}
	f.assertDataIntact(t)
}

func TestShardExport_AttemptDeadlineFailsAHungExport(t *testing.T) {
	f := newExportFixture(t)
	f.seedAttempt(t, 1, f.clock.Add(-exportAttemptTimeout-time.Minute), "snap-1", "i-export-1")
	f.ops.snapState = ExportSnapshotState{State: "completed"}
	f.ops.instState = "running" // alive but never finishing

	f.evaluate(t)

	if len(f.ops.cleanupCalls) != 1 {
		t.Fatalf("a hung export was not failed at the deadline: cleanups=%v", f.ops.cleanupCalls)
	}
	f.assertDataIntact(t)
}

func TestShardExport_AttemptCapStopsRetryingButKeepsTheData(t *testing.T) {
	f := newExportFixture(t)
	f.seedAttempt(t, maxExportAttempts, f.clock, "", "")
	_ = f.ss.ClearExportFacts("s-test") // the last attempt failed and was cleaned up
	f.sm.mu.Lock()
	f.sm.Export.StartedAt, f.sm.Export.InstanceType = nil, ""
	f.sm.mu.Unlock()

	f.evaluate(t)
	f.evaluate(t)

	if len(f.ops.createCalls) != 0 || len(f.ops.launchCalls) != 0 {
		t.Fatalf("started attempt %d past the cap: snapshots=%v launches=%v", maxExportAttempts+1, f.ops.createCalls, f.ops.launchCalls)
	}
	if got := f.row(t).Export.Attempt; got != maxExportAttempts {
		t.Fatalf("attempt counter moved to %d", got)
	}
	f.assertDataIntact(t) // stays ARCHIVING with the volume: it is never auto-GONE
}

// The cap holds the CLAIM, not the launch: a shard held by it has started
// nothing (no attempt number, no clock, no snapshot, no cost), and it proceeds
// the tick a slot frees.
func TestShardExport_ConcurrencyCapHoldsTheClaim(t *testing.T) {
	f := newExportFixture(t, func(d *ShardDeps) { d.MaxConcurrentExports = 1 })
	f.occupyASlot(t, "s-busy")

	f.evaluate(t)

	row := f.row(t)
	if row.Export.Attempt != 0 || row.Export.StartedAt != nil || row.Export.SnapshotID != "" {
		t.Fatalf("a shard held by the cap started something: %+v", row)
	}
	if len(f.ops.createCalls) != 0 || len(f.ops.launchCalls) != 0 || len(f.ops.cleanupCalls) != 0 {
		t.Fatalf("a held shard touched AWS: snapshots=%v launches=%v cleanups=%v", f.ops.createCalls, f.ops.launchCalls, f.ops.cleanupCalls)
	}
	f.assertDataIntact(t)

	// Capacity frees up: the next tick claims and snapshots.
	if err := f.ss.ClearExportFacts("s-busy"); err != nil {
		t.Fatal(err)
	}
	f.evaluate(t)
	if row := f.row(t); row.Export.Attempt != 1 || row.Export.SnapshotID != "snap-1" {
		t.Fatalf("did not proceed once capacity freed: %+v", row)
	}
}

// A shard with no volume has nothing to snapshot, so it must not take one of
// the scarce export slots: with the cap at 1, a dead ARCHIVING row (instance
// and volume long gone) that claimed the slot would block every healthy shard's
// export until its attempt deadline, three times over. It waits unclaimed
// instead (the stuck-archive alert is what surfaces it).
func TestShardExport_ShardWithoutAVolumeDoesNotTakeASlot(t *testing.T) {
	f := newExportFixture(t, func(d *ShardDeps) { d.MaxConcurrentExports = 1 })
	f.sm.mu.Lock()
	f.sm.EBSVolumeID = ""
	f.sm.mu.Unlock()

	for i := 0; i < 3; i++ {
		f.evaluate(t)
	}

	row := f.row(t)
	if row.Export.Attempt != 0 || row.Export.StartedAt != nil {
		t.Fatalf("a volume-less shard claimed an export attempt: %+v", row)
	}
	if len(f.ops.createCalls) != 0 || len(f.ops.launchCalls) != 0 {
		t.Fatalf("a volume-less shard touched AWS: snapshots=%v launches=%v", f.ops.createCalls, f.ops.launchCalls)
	}

	// The slot is still free for a healthy shard.
	bucket := seedBucket(t, f.db, BucketStasis)
	if err := f.ss.SaveShard(&ShardRow{
		ID:          "s-healthy",
		State:       StateArchiving,
		BucketID:    bucket.ID,
		Tier:        "small",
		WindowStart: f.clock,
		WindowEnd:   f.clock,
		CreatedAt:   f.clock,
	}); err != nil {
		t.Fatal(err)
	}
	if claim, err := f.ss.BeginExportAttempt("s-healthy", 0, "r7g.large", f.clock, 1); err != nil || claim != ExportClaimWon {
		t.Fatalf("the volume-less shard is holding the only slot: claim=%v err=%v", claim, err)
	}
	f.assertDataIntact(t)
}

// Waiting behind the cap must not burn the retry budget. The attempt clock and
// the attempt counter start at the claim, so a shard queued for longer than the
// attempt deadline (a whole cohort rotating together at a low cap) still gets
// its full first attempt, not a failed one it never ran.
func TestShardExport_QueueingBehindTheCapDoesNotConsumeAttempts(t *testing.T) {
	f := newExportFixture(t, func(d *ShardDeps) { d.MaxConcurrentExports = 1 })
	f.occupyASlot(t, "s-busy")

	// Held for far longer than an attempt may take, across many ticks.
	for i := 0; i < 5; i++ {
		f.evaluate(t)
		f.now = f.now.Add(exportAttemptTimeout)
	}
	if row := f.row(t); row.Export.Attempt != 0 {
		t.Fatalf("queueing consumed attempts: export_attempt = %d", row.Export.Attempt)
	}
	if len(f.ops.cleanupCalls) != 0 {
		t.Fatalf("queueing was treated as a failed attempt: %v", f.ops.cleanupCalls)
	}

	// The slot frees: the shard gets attempt 1 with a fresh clock.
	if err := f.ss.ClearExportFacts("s-busy"); err != nil {
		t.Fatal(err)
	}
	f.evaluate(t)
	row := f.row(t)
	if row.Export.Attempt != 1 || row.Export.StartedAt == nil || !row.Export.StartedAt.Equal(f.now) {
		t.Fatalf("first attempt after queueing: %+v (clock should start at %v)", row, f.now)
	}
	f.evaluate(t)
	if len(f.ops.cleanupCalls) != 0 {
		t.Fatalf("the first attempt was failed by time spent queueing: %v", f.ops.cleanupCalls)
	}
}

func TestShardExport_AWSErrorsRetryNextTickWithoutLosingTheData(t *testing.T) {
	f := newExportFixture(t)
	f.ops.createErr = errors.New("RequestLimitExceeded")
	f.evaluate(t)
	f.assertDataIntact(t)
	if f.row(t).Export.SnapshotID != "" {
		t.Fatal("recorded a snapshot that was never created")
	}

	f.ops.createErr = nil
	f.evaluate(t)
	if f.row(t).Export.SnapshotID != "snap-1" {
		t.Fatal("did not recover on the next tick")
	}
}

// The export needs only the volume and the snapshot, not the live handle: a
// coordinator that restarted mid-archive (Live == nil) must still drive it.
func TestShardExport_DoesNotNeedTheLiveHandle(t *testing.T) {
	f := newExportFixture(t)
	f.sm.Live = nil
	f.evaluate(t)
	if len(f.ops.createCalls) != 1 {
		t.Fatalf("snapshot not taken without a live handle: %v", f.ops.createCalls)
	}
	f.assertDataIntact(t)
}

// Completion requires the export resources to be gone. If cleanup fails after
// the manifest was recorded, the shard's volume must not be retired until a
// later tick cleans up.
func TestShardExport_CleanupFailureHoldsTheRetireUntilItSucceeds(t *testing.T) {
	f := newExportFixture(t)
	f.seedAttempt(t, 1, f.clock, "snap-1", "i-export-1")
	f.ops.snapState = ExportSnapshotState{State: "completed"}
	f.ops.instState = "terminated"
	f.manifest.manifests[testExportManifestKey] = goodManifest(1)
	f.ops.cleanupErr = errors.New("DeleteSnapshot throttled")

	f.evaluate(t)

	if f.row(t).ParquetPath == "" {
		t.Fatal("the export result was not recorded")
	}
	if len(f.volumes.deleted) != 0 || f.state(t) != StateArchiving {
		t.Fatalf("retired the shard with export resources outstanding: deleted=%v state=%q", f.volumes.deleted, f.state(t))
	}

	f.ops.cleanupErr = nil
	f.evaluate(t)
	if len(f.volumes.deleted) != 1 || f.state(t) != StateArchived {
		t.Fatalf("did not complete after cleanup succeeded: deleted=%v state=%q", f.volumes.deleted, f.state(t))
	}
	if row := f.row(t); row.Export.SnapshotID != "" || row.Export.Instance != "" {
		t.Fatalf("export facts still set: %+v", row)
	}
}

// Without export wiring (dev, docker, most tests) the in-process Archive path
// is unchanged.
func TestShardExport_NilExportsKeepsTheInProcessArchive(t *testing.T) {
	f := newExportFixture(t, func(d *ShardDeps) { d.Exports = nil; d.ExportManifests = nil })
	f.live.archivePath = "s3://weft/warp/s-test.parquet"
	f.evaluate(t)
	if !f.live.archived {
		t.Fatal("in-process Archive did not run with no export wiring")
	}
	if f.state(t) != StateArchived {
		t.Fatalf("state = %q, want ARCHIVED", f.state(t))
	}
}

// ── export prefix safety ────────────────────────────────────────────

// The export prefix travels through an EC2 tag and the host's shell and systemd
// environment file, so it is built from a sanitized label. The in-process
// archive's prefix uses the raw service name, which is customer-controlled and
// can hold spaces, quotes or newlines.
var exportPrefixAllowed = regexp.MustCompile(`^[A-Za-z0-9._:/-]+$`) // the same set deploy/export-role.sh enforces

func TestExportS3Prefix_IsAlwaysSafeForTheHost(t *testing.T) {
	sh := &Shard{deps: &ShardDeps{ParquetDir: "s3://weft"}}
	svc := func(names ...string) []shardstore.ShardService {
		var out []shardstore.ShardService
		for i, n := range names {
			out = append(out, shardstore.ShardService{NamespaceID: uint(i + 1), Service: n})
		}
		return out
	}
	cases := []struct {
		name     string
		services []shardstore.ShardService
		want     string
	}{
		{"plain name is unchanged", svc("warp"), "s3://weft/warp"},
		{"a space", svc("my service"), "s3://weft/my_service"},
		{"a slash does not nest", svc("a/b"), "s3://weft/a_b"},
		{"a newline", svc("x\ny"), "s3://weft/x_y"},
		{"shell metacharacters", svc("$(id);rm"), "s3://weft/__id__rm"},
		{"quotes", svc(`a"b'c`), "s3://weft/a_b_c"},
		{"non-ASCII", svc("héllo"), "s3://weft/h_llo"},
		{"dots only could escape the directory", svc(".."), "s3://weft/_"},
		{"a single dot", svc("."), "s3://weft/_"},
		{"an overlong name is truncated", svc(strings.Repeat("a", 200)), "s3://weft/" + strings.Repeat("a", 64)},
		{"several services collapse as before", svc("a", "b"), "s3://weft/2-apps"},
		{"none", nil, "s3://weft/shared"},
	}
	for _, c := range cases {
		got := sh.exportS3Prefix(c.services)
		if got != c.want {
			t.Errorf("%s: exportS3Prefix = %q, want %q", c.name, got, c.want)
		}
		if !exportPrefixAllowed.MatchString(got) {
			t.Errorf("%s: %q contains characters the export host rejects", c.name, got)
		}
	}
}

// End to end: a shard whose only service has a space in its name must still
// export. Launch, manifest poll and output cleanup all use the same safe prefix.
func TestShardExport_ServiceNameWithASpaceStillExports(t *testing.T) {
	f := newExportFixture(t)
	f.sm.Services = []shardstore.ShardService{{NamespaceID: 1, Service: "my service"}}
	f.seedAttempt(t, 1, f.clock, "snap-1", "")
	f.ops.snapState = ExportSnapshotState{State: "completed"}

	f.evaluate(t) // launches
	if len(f.ops.launchCalls) != 1 {
		t.Fatalf("launch calls = %d", len(f.ops.launchCalls))
	}
	if got := f.ops.launchCalls[0].Prefix; got != "s3://weft/my_service" || !exportPrefixAllowed.MatchString(got) {
		t.Fatalf("launch prefix = %q, want the sanitized s3://weft/my_service", got)
	}

	// The coordinator polls the manifest at the key derived from the same prefix.
	f.ops.instState = "terminated"
	f.manifest.manifests["s3://weft/my_service/_manifests/s-test.json"] = goodManifest(1)
	f.evaluate(t)
	if f.row(t).ParquetPath == "" {
		t.Fatal("the manifest at the sanitized key was never found")
	}
}

// ── failed-attempt output ───────────────────────────────────────────

var attempt2Outputs = []string{
	"s3://weft/warp/s-test.attempt-2/**/*.parquet",
	"s3://weft/warp/s-test.attempt-2.parquet",
}

// A failed attempt may have left partial output. It lives in the attempt's own
// location, so deleting it is safe; leaving it would only waste storage, but a
// retry never shares the directory, so correctness does not depend on it.
func TestShardExport_FailedAttemptDeletesItsOwnOutputAfterStoppingTheHost(t *testing.T) {
	f := newExportFixture(t)
	f.seedAttempt(t, 2, f.clock, "snap-2", "i-export-2")
	f.ops.snapState = ExportSnapshotState{State: "completed"}
	f.ops.instState = "terminated"

	f.evaluate(t)

	if got := f.outputs.paths; len(got) != 2 || got[0] != attempt2Outputs[0] || got[1] != attempt2Outputs[1] {
		t.Fatalf("deleted %v, want exactly attempt 2's output %v", got, attempt2Outputs)
	}
	// The host is stopped before its output is deleted, or a still-running host
	// could write after the delete and leave files nothing tracks.
	ev := f.events.snapshot()
	if len(ev) < 2 || ev[0] != "cleanup" || ev[1] != "delete:"+attempt2Outputs[0] {
		t.Fatalf("event order = %v, want cleanup first, then the deletes", ev)
	}
	row := f.row(t)
	if row.Export.SnapshotID != "" || row.Export.Instance != "" || row.Export.Attempt != 2 {
		t.Fatalf("after a failed attempt: %+v", row)
	}
}

// If the output cannot be deleted, the facts stay so the next tick retries, and
// the snapshot being gone must not wedge that retry in a "poll forever" loop.
func TestShardExport_OutputDeleteFailureRetriesNextTick(t *testing.T) {
	f := newExportFixture(t)
	f.seedAttempt(t, 2, f.clock, "snap-2", "i-export-2")
	f.ops.snapState = ExportSnapshotState{State: "completed"}
	f.ops.instState = "terminated"
	f.outputs.err = errors.New("AccessDenied")

	f.evaluate(t)
	if row := f.row(t); row.Export.SnapshotID == "" {
		t.Fatal("facts cleared although the output delete failed")
	}
	f.assertDataIntact(t)

	// Cleanup already removed the snapshot; the next tick sees it vanished.
	f.ops.snapErr = errSnapshotNotFound
	f.outputs.err = nil
	f.evaluate(t)

	if got := f.outputs.paths; len(got) != 2 {
		t.Fatalf("retry deleted %v, want attempt 2's two output paths", got)
	}
	if row := f.row(t); row.Export.SnapshotID != "" || row.Export.Instance != "" {
		t.Fatalf("facts not cleared after the retry: %+v", row)
	}
}

func TestShardExport_SnapshotVanishedFailsTheAttempt(t *testing.T) {
	f := newExportFixture(t)
	f.seedAttempt(t, 1, f.clock, "snap-1", "")
	f.ops.snapErr = errSnapshotNotFound

	f.evaluate(t)

	if len(f.ops.cleanupCalls) != 1 {
		t.Fatalf("a vanished snapshot did not fail the attempt: cleanups=%v", f.ops.cleanupCalls)
	}
	if row := f.row(t); row.Export.SnapshotID != "" {
		t.Fatalf("facts not cleared: %+v", row)
	}
}

// A transient describe error is not a vanished snapshot.
func TestShardExport_TransientSnapshotErrorDoesNotFailTheAttempt(t *testing.T) {
	f := newExportFixture(t)
	f.seedAttempt(t, 1, f.clock, "snap-1", "")
	f.ops.snapErr = errors.New("RequestLimitExceeded")

	f.evaluate(t)

	if len(f.ops.cleanupCalls) != 0 {
		t.Fatalf("failed the attempt over a throttled describe: %v", f.ops.cleanupCalls)
	}
	f.assertDataIntact(t)
}

// The success path must never delete: that output is the archive.
func TestShardExport_SuccessNeverDeletesOutput(t *testing.T) {
	f := newExportFixture(t)
	f.seedAttempt(t, 1, f.clock, "snap-1", "i-export-1")
	f.ops.snapState = ExportSnapshotState{State: "completed"}
	f.ops.instState = "terminated"
	f.manifest.manifests[testExportManifestKey] = goodManifest(1)

	f.evaluate(t)

	if f.state(t) != StateArchived {
		t.Fatalf("state = %q, want ARCHIVED", f.state(t))
	}
	if len(f.outputs.paths) != 0 {
		t.Fatalf("deleted %v on the success path", f.outputs.paths)
	}
}

// Belt and braces: once an archive is recorded, no failure path may touch the
// output, whatever called it.
func TestShardExport_NeverDeletesOutputOnceTheArchiveIsRecorded(t *testing.T) {
	f := newExportFixture(t)
	f.seedAttempt(t, 1, f.clock, "snap-1", "i-export-1")
	f.sm.mu.Lock()
	f.sm.ParquetPath = "s3://weft/warp/s-test.attempt-1/**/*.parquet"
	f.sm.mu.Unlock()

	if err := f.sm.failExportAttempt(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	if len(f.outputs.paths) != 0 {
		t.Fatalf("deleted %v although the archive is recorded", f.outputs.paths)
	}
}

func TestShardExport_NoOutputDeleterStillFailsTheAttempt(t *testing.T) {
	f := newExportFixture(t, func(d *ShardDeps) { d.ExportOutputs = nil })
	f.seedAttempt(t, 1, f.clock, "snap-1", "i-export-1")
	f.ops.snapState = ExportSnapshotState{State: "completed"}
	f.ops.instState = "terminated"

	f.evaluate(t)

	if len(f.ops.cleanupCalls) != 1 || f.row(t).Export.SnapshotID != "" {
		t.Fatalf("attempt not failed cleanly without a deleter: cleanups=%v row=%+v", f.ops.cleanupCalls, f.row(t))
	}
}

// An admin delete of a shard mid-export must not leave a snapshot or an
// instance behind.
func TestShardExport_AdminDeleteCleansUpExportResources(t *testing.T) {
	f := newExportFixture(t)
	f.seedAttempt(t, 1, f.clock, "snap-1", "i-export-1")

	if err := f.sm.Fire(context.Background(), TriggerAdminDelete); err != nil {
		t.Fatalf("AdminDelete: %v", err)
	}
	if len(f.ops.cleanupCalls) != 1 || f.ops.cleanupCalls[0] != "s-test" {
		t.Fatalf("cleanup calls = %v, want one for s-test", f.ops.cleanupCalls)
	}
}
