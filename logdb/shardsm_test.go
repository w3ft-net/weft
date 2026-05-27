package logdb

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/store"
)

// openSMTestStore opens the test postgres (WEFT_TEST_DSN, default the
// docker-compose dev DB) and returns a transaction-scoped store that
// rolls back at test end. Skips when postgres is unavailable, matching
// the other DB-backed logdb tests. Now that the FSMs hold a concrete
// *store.Store (no SMStore interface to fake), state reads/writes go
// through the real schema.
func openSMTestStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tx, rollback := db.BeginTest()
	t.Cleanup(rollback)
	return tx
}

// seedBucket inserts a bucket row (unique label) and returns it.
// shard_records carries a NOT NULL FK on bucket_id, so every shard
// fixture needs a real bucket to point at; the bucket FSM fixture
// uses it directly for its state column.
func seedBucket(t *testing.T, db *store.Store, state BucketState) *store.Bucket {
	t.Helper()
	b := &store.Bucket{
		Label:                fmt.Sprintf("sm-test-%d", time.Now().UnixNano()),
		Services:             []string{},
		Tier:                 "small",
		TargetIngestingCount: 2,
		CreatedAt:            "2026-05-21T12:00:00Z",
		WindowDuration:       "1h0m0s",
		State:                string(state),
	}
	if err := db.SaveBucket(b); err != nil {
		t.Fatalf("seed bucket: %v", err)
	}
	return b
}

// makeFixtureShardSM builds a ShardSM in initialState backed by a real
// (transaction-scoped) store. Seeds a bucket + shard_records row, then
// returns the SM, the store, and the in-memory entry the SM mutates.
func makeFixtureShardSM(t *testing.T, initialState ShardState, customize ...func(*ShardSMDeps)) (*ShardSM, *store.Store, *Shard) {
	t.Helper()
	db := openSMTestStore(t)
	bucket := seedBucket(t, db, BucketStasis)
	if err := (ShardStore{db}).SaveShard(&Shard{
		ID:          "s-test",
		State:       initialState,
		BucketID:    bucket.ID,
		Tier:        "small",
		WindowStart: time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC),
		WindowEnd:   time.Date(2026, 5, 21, 13, 0, 0, 0, time.UTC),
		CreatedAt:   time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seed shard: %v", err)
	}
	entry := &Shard{
		ID:       "s-test",
		BucketID: bucket.ID,
		Tier:     "small",
		State:    initialState,
	}
	clock := func() time.Time { return time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC) }
	deps := ShardSMDeps{
		Store:          ShardStore{db},
		Clock:          clock,
		StaleThreshold: 2 * time.Minute,
	}
	for _, c := range customize {
		c(&deps)
	}
	sm := NewShardSM(entry, deps)
	return sm, db, entry
}

// fakeLauncher is a ShardLaunchAPI that records Launch/Terminate calls
// and returns a canned result (or error). Empty result fields default
// to plausible ids so callers can assert on persistence.
type fakeLauncher struct {
	mu         sync.Mutex
	result     LaunchResult
	err        error
	calls      []LaunchSpec
	terminated []string
}

func (f *fakeLauncher) Launch(_ context.Context, spec LaunchSpec) (LaunchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, spec)
	if f.err != nil {
		return LaunchResult{}, f.err
	}
	res := f.result
	if res.InstanceID == "" {
		res.InstanceID = "i-fake"
	}
	if res.AZ == "" {
		res.AZ = spec.AZ
	}
	if res.VolumeID == "" {
		if spec.ExistingVolumeID != "" {
			res.VolumeID = spec.ExistingVolumeID
		} else {
			res.VolumeID = "vol-fake"
		}
	}
	return res, nil
}

func (f *fakeLauncher) Terminate(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminated = append(f.terminated, id)
	return nil
}

func (f *fakeLauncher) launchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// waitForLaunch blocks until the SM's async launch goroutine finishes
// (launchInFlight cleared), or fails the test after a short deadline.
func waitForLaunch(t *testing.T, sm *ShardSM) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !sm.launchInFlight.Load() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("launch goroutine did not finish within deadline")
}

func TestShardSM_EvaluateLaunchingKicksFreshLaunch(t *testing.T) {
	fl := &fakeLauncher{}
	sm, db, entry := makeFixtureShardSM(t, StateLaunching, func(d *ShardSMDeps) { d.Launcher = fl })
	entry.AZ = "us-east-2a"
	si := 0
	entry.StripeIndex = &si

	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	waitForLaunch(t, sm)

	if fl.launchCount() != 1 {
		t.Fatalf("launch calls: got %d want 1", fl.launchCount())
	}
	if fl.calls[0].ExistingVolumeID != "" {
		t.Fatalf("fresh launch should carry no existing volume, got %q", fl.calls[0].ExistingVolumeID)
	}
	if entry.InstanceID != "i-fake" {
		t.Fatalf("entry.InstanceID: got %q", entry.InstanceID)
	}
	// launch result persisted narrowly
	rec, _, _ := ShardStore{db}.GetShard("s-test")
	if rec.InstanceID != "i-fake" || rec.AZ != "us-east-2a" || rec.EBSVolumeID != "vol-fake" {
		t.Fatalf("persisted launch result wrong: instance=%q az=%q vol=%q", rec.InstanceID, rec.AZ, rec.EBSVolumeID)
	}
}

func TestShardSM_EvaluateLaunchingNoopWhenInstanceSet(t *testing.T) {
	fl := &fakeLauncher{}
	sm, _, entry := makeFixtureShardSM(t, StateLaunching, func(d *ShardSMDeps) { d.Launcher = fl })
	entry.InstanceID = "i-existing"

	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if fl.launchCount() != 0 {
		t.Fatalf("should not launch when an instance is already set; got %d calls", fl.launchCount())
	}
}

func TestShardSM_EvaluateLaunchingBudgetExhaustedGoesGone(t *testing.T) {
	fl := &fakeLauncher{}
	sm, _, entry := makeFixtureShardSM(t, StateLaunching, func(d *ShardSMDeps) {
		d.Launcher = fl
		d.LaunchRetryBudget = 3
	})
	entry.LaunchAttempts = 3 // budget spent

	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if fl.launchCount() != 0 {
		t.Fatalf("exhausted budget should not launch; got %d calls", fl.launchCount())
	}
	st, _ := sm.State(context.Background())
	if st != StateGone {
		t.Fatalf("state: got %q want GONE", st)
	}
}

func TestShardSM_EvaluateLaunchFailureBumpsAttempts(t *testing.T) {
	fl := &fakeLauncher{err: fmt.Errorf("InsufficientInstanceCapacity")}
	sm, db, entry := makeFixtureShardSM(t, StateLaunching, func(d *ShardSMDeps) { d.Launcher = fl })
	entry.AZ = "us-east-2a"

	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	waitForLaunch(t, sm)

	if entry.LaunchAttempts != 1 {
		t.Fatalf("entry.LaunchAttempts: got %d want 1", entry.LaunchAttempts)
	}
	rec, _, _ := ShardStore{db}.GetShard("s-test")
	if rec.LaunchAttempts != 1 {
		t.Fatalf("persisted launch_attempts: got %d want 1", rec.LaunchAttempts)
	}
	st, _ := sm.State(context.Background())
	if st != StateLaunching {
		t.Fatalf("a retryable failure should stay LAUNCHING; got %q", st)
	}
}

func TestShardSM_EvaluateActiveFiresStaleToSuspended(t *testing.T) {
	sm, _, entry := makeFixtureShardSM(t, StateIngesting)
	entry.EBSVolumeID = "vol-1"                                            // volume intact → SUSPENDED, not GONE
	entry.LastHeartbeatAt = time.Date(2026, 5, 21, 11, 50, 0, 0, time.UTC) // 10m < clock(12:00), threshold 2m

	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	st, _ := sm.State(context.Background())
	if st != StateSuspended {
		t.Fatalf("state: got %q want SUSPENDED", st)
	}
}

func TestShardSM_EvaluateActiveNoStaleWhenFresh(t *testing.T) {
	sm, _, entry := makeFixtureShardSM(t, StateIngesting)
	entry.EBSVolumeID = "vol-1"
	entry.LastHeartbeatAt = time.Date(2026, 5, 21, 11, 59, 30, 0, time.UTC) // 30s < 2m threshold

	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	st, _ := sm.State(context.Background())
	if st != StateIngesting {
		t.Fatalf("a fresh shard should stay INGESTING; got %q", st)
	}
}

func TestShardSM_EvaluateSuspendedReapsThenRelaunches(t *testing.T) {
	fl := &fakeLauncher{}
	vm := &fakeVolumeManager{findResult: map[string]*VolumeInfo{"s-test": {VolumeID: "vol-1", AZ: "us-east-2a"}}}
	sm, db, entry := makeFixtureShardSM(t, StateSuspended, func(d *ShardSMDeps) {
		d.Launcher = fl
		d.Volumes = vm
	})
	entry.InstanceID = "i-dead"
	entry.EBSVolumeID = "vol-1"
	entry.AZ = "us-east-2a"

	// Tick 1: reap the dead instance, hand off to LAUNCHING.
	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate (reap): %v", err)
	}
	if len(fl.terminated) != 1 || fl.terminated[0] != "i-dead" {
		t.Fatalf("dead instance not reaped: terminated=%v", fl.terminated)
	}
	if entry.InstanceID != "" {
		t.Fatalf("InstanceID not cleared in memory: %q", entry.InstanceID)
	}
	rec, _, _ := ShardStore{db}.GetShard("s-test")
	if rec.InstanceID != "" {
		t.Fatalf("InstanceID not cleared in store: %q", rec.InstanceID)
	}
	if rec.AZ != "us-east-2a" || rec.EBSVolumeID != "vol-1" {
		t.Fatalf("reap clobbered az/volume: az=%q vol=%q", rec.AZ, rec.EBSVolumeID)
	}
	if st, _ := sm.State(context.Background()); st != StateLaunching {
		t.Fatalf("after reap: got %q want LAUNCHING", st)
	}

	// Tick 2: LAUNCHING with the surviving volume → recovery relaunch.
	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate (relaunch): %v", err)
	}
	waitForLaunch(t, sm)
	if fl.launchCount() != 1 {
		t.Fatalf("launch calls: got %d want 1", fl.launchCount())
	}
	if fl.calls[0].ExistingVolumeID != "vol-1" {
		t.Fatalf("recovery launch must reattach vol-1, got %q", fl.calls[0].ExistingVolumeID)
	}
	if entry.InstanceID != "i-fake" {
		t.Fatalf("replacement instance not stamped: %q", entry.InstanceID)
	}
}

func TestShardSM_EvaluateSuspendedStrandedWhenVolumeGone(t *testing.T) {
	fl := &fakeLauncher{}
	vm := &fakeVolumeManager{} // FindByShard returns nil → volume gone
	sm, _, entry := makeFixtureShardSM(t, StateSuspended, func(d *ShardSMDeps) {
		d.Launcher = fl
		d.Volumes = vm
	})
	entry.InstanceID = "i-dead"
	entry.EBSVolumeID = "vol-gone"

	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if st, _ := sm.State(context.Background()); st != StateGone {
		t.Fatalf("stranded shard: got %q want GONE", st)
	}
	if len(fl.terminated) != 0 {
		t.Fatalf("stranded check must short-circuit before reaping; terminated=%v", fl.terminated)
	}
}

func TestShardSM_EvaluateSuspendedRestartAfterReapRelaunches(t *testing.T) {
	// Crash recovery: we reaped the dead instance and cleared InstanceID,
	// then crashed before firing Relaunch. Re-Evaluate must not try to
	// reap nothing — it goes straight to LAUNCHING.
	fl := &fakeLauncher{}
	vm := &fakeVolumeManager{findResult: map[string]*VolumeInfo{"s-test": {VolumeID: "vol-1"}}}
	sm, _, entry := makeFixtureShardSM(t, StateSuspended, func(d *ShardSMDeps) {
		d.Launcher = fl
		d.Volumes = vm
	})
	entry.InstanceID = "" // already reaped
	entry.EBSVolumeID = "vol-1"

	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(fl.terminated) != 0 {
		t.Fatalf("nothing to reap; terminated=%v", fl.terminated)
	}
	if st, _ := sm.State(context.Background()); st != StateLaunching {
		t.Fatalf("got %q want LAUNCHING", st)
	}
}

func TestShardSM_LaunchingToIngestingOnMatchingHeartbeat(t *testing.T) {
	sm, _, entry := makeFixtureShardSM(t, StateLaunching)
	entry.InstanceID = "i-abc" // simulated post-launch tag; the guard reads it off the entry

	hb := store.ShardHeartbeat{
		ShardID: "s-test", Addr: "10.0.0.1:9000", InstanceID: "i-abc", RowCount: 7,
	}
	if err := sm.Fire(context.Background(), TriggerHeartbeat, hb); err != nil {
		t.Fatalf("Fire heartbeat: %v", err)
	}

	got, err := sm.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if got != StateIngesting {
		t.Fatalf("state: got %q want INGESTING", got)
	}
	if entry.Addr != "10.0.0.1:9000" {
		t.Fatalf("entry.Addr: got %q", entry.Addr)
	}
	if entry.RowCount != 7 {
		t.Fatalf("entry.RowCount: got %d", entry.RowCount)
	}
}

func TestShardSM_LaunchingRejectsStrayInstanceHeartbeat(t *testing.T) {
	sm, _, entry := makeFixtureShardSM(t, StateLaunching)
	entry.InstanceID = "i-abc"

	hb := store.ShardHeartbeat{
		ShardID: "s-test", Addr: "10.0.0.1:9000", InstanceID: "i-xyz", // wrong instance
	}
	err := sm.Fire(context.Background(), TriggerHeartbeat, hb)
	// stateless returns an error when no permit fires.
	if err == nil {
		t.Fatalf("expected guard rejection; entry stayed at %q", entry.State)
	}
	if entry.State != StateLaunching {
		t.Fatalf("state: got %q want LAUNCHING", entry.State)
	}
}

func TestShardSM_LaunchingToGoneOnLaunchFailed(t *testing.T) {
	sm, db, _ := makeFixtureShardSM(t, StateLaunching)
	if err := sm.Fire(context.Background(), TriggerLaunchFailed); err != nil {
		t.Fatalf("Fire LaunchFailed: %v", err)
	}
	// GONE.OnEntry deletes the row → state read returns GONE
	got, err := sm.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if got != StateGone {
		t.Fatalf("state: got %q want GONE", got)
	}
	if _, ok, err := (ShardStore{db}).GetShard("s-test"); err != nil || ok {
		t.Fatalf("expected shard row deleted on GONE; ok=%v err=%v", ok, err)
	}
}

func TestShardSM_IngestingToReadOnlyOnRotate(t *testing.T) {
	sm, _, entry := makeFixtureShardSM(t, StateIngesting)
	if err := sm.Fire(context.Background(), TriggerRotateRequested); err != nil {
		t.Fatalf("Fire Rotate: %v", err)
	}
	if entry.State != StateReadOnly {
		t.Fatalf("state: got %q want READ_ONLY", entry.State)
	}
}

func TestShardSM_ReadOnlyToArchivingOnArchiveRequested(t *testing.T) {
	// The bucket's retention sweep fires ArchiveRequested; the shard
	// moves to ARCHIVING (not straight to ARCHIVED) — the export work
	// then runs in evaluateArchiving and completes via ArchiveComplete.
	sm, _, entry := makeFixtureShardSM(t, StateReadOnly)
	if err := sm.Fire(context.Background(), TriggerArchiveRequested); err != nil {
		t.Fatalf("Fire ArchiveRequested: %v", err)
	}
	if entry.State != StateArchiving {
		t.Fatalf("state: got %q want ARCHIVING", entry.State)
	}
}

func TestShardSM_IngestingToSuspendedOnStaleWithVolume(t *testing.T) {
	sm, _, entry := makeFixtureShardSM(t, StateIngesting)
	entry.EBSVolumeID = "vol-abc"
	entry.InstanceID = "i-old"
	entry.Addr = "10.0.0.1:9000"
	entry.LastHeartbeatAt = time.Date(2026, 5, 21, 11, 50, 0, 0, time.UTC) // 10min stale

	if err := sm.Fire(context.Background(), TriggerHeartbeatStale); err != nil {
		t.Fatalf("Fire stale: %v", err)
	}
	if entry.State != StateSuspended {
		t.Fatalf("state: got %q want SUSPENDED", entry.State)
	}
	// SUSPENDED.OnEntry tears down the live connection (Addr/Shard) but
	// KEEPS InstanceID — that's the dead instance evaluateSuspended must
	// reap before relaunching. (Changed in B1.2; previously it cleared
	// InstanceID, which lost the corpse's id.)
	if entry.Addr != "" {
		t.Fatalf("SUSPENDED.OnEntry should have cleared Addr; got %q", entry.Addr)
	}
	if entry.InstanceID != "i-old" {
		t.Fatalf("SUSPENDED.OnEntry must keep InstanceID for the reap; got %q", entry.InstanceID)
	}
}

func TestShardSM_IngestingToGoneOnStaleWithoutVolume(t *testing.T) {
	sm, _, entry := makeFixtureShardSM(t, StateIngesting)
	entry.EBSVolumeID = ""
	entry.LastHeartbeatAt = time.Date(2026, 5, 21, 11, 50, 0, 0, time.UTC)

	if err := sm.Fire(context.Background(), TriggerHeartbeatStale); err != nil {
		t.Fatalf("Fire stale: %v", err)
	}
	got, err := sm.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if got != StateGone {
		t.Fatalf("state: got %q want GONE", got)
	}
}

func TestShardSM_StaleGuardRejectsWhenFresh(t *testing.T) {
	sm, _, entry := makeFixtureShardSM(t, StateIngesting)
	entry.EBSVolumeID = "vol-abc"
	// LastHeartbeatAt 30s ago (well under 2min threshold)
	entry.LastHeartbeatAt = time.Date(2026, 5, 21, 11, 59, 30, 0, time.UTC)

	err := sm.Fire(context.Background(), TriggerHeartbeatStale)
	if err == nil {
		t.Fatalf("expected guard rejection on fresh heartbeat")
	}
	if entry.State != StateIngesting {
		t.Fatalf("state: got %q want unchanged INGESTING", entry.State)
	}
}

func TestShardSM_SuspendedToIngestingOnRebindHeartbeat(t *testing.T) {
	sm, _, entry := makeFixtureShardSM(t, StateSuspended)
	entry.EBSVolumeID = "vol-abc"
	entry.InstanceID = "i-new" // replacement just attached

	hb := store.ShardHeartbeat{
		ShardID: "s-test", Addr: "10.0.0.7:9000", InstanceID: "i-new",
	}
	if err := sm.Fire(context.Background(), TriggerHeartbeat, hb); err != nil {
		t.Fatalf("Fire heartbeat: %v", err)
	}
	if entry.State != StateIngesting {
		t.Fatalf("state: got %q want INGESTING", entry.State)
	}
	if entry.Addr != "10.0.0.7:9000" {
		t.Fatalf("entry.Addr: got %q", entry.Addr)
	}
}

func TestShardSM_SuspendedToGoneOnStranded(t *testing.T) {
	sm, _, _ := makeFixtureShardSM(t, StateSuspended)
	if err := sm.Fire(context.Background(), TriggerStranded); err != nil {
		t.Fatalf("Fire Stranded: %v", err)
	}
	got, err := sm.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if got != StateGone {
		t.Fatalf("state: got %q want GONE", got)
	}
}

func TestShardSM_ArchivedToGoneOnAdminDelete(t *testing.T) {
	sm, db, _ := makeFixtureShardSM(t, StateArchived)
	if err := sm.Fire(context.Background(), TriggerAdminDelete); err != nil {
		t.Fatalf("Fire AdminDelete: %v", err)
	}
	if _, ok, err := (ShardStore{db}).GetShard("s-test"); err != nil || ok {
		t.Fatalf("expected shard row deleted on AdminDelete; ok=%v err=%v", ok, err)
	}
}

func TestShardSM_ArchivedRejectsRotateAndArchive(t *testing.T) {
	// ARCHIVED is terminal-for-data; only AdminDelete leaves it.
	for _, trigger := range []Trigger{TriggerRotateRequested, TriggerArchiveRequested, TriggerHeartbeatStale, TriggerLaunchFailed} {
		t.Run(string(trigger), func(t *testing.T) {
			sm, _, entry := makeFixtureShardSM(t, StateArchived)
			err := sm.Fire(context.Background(), trigger)
			if err == nil {
				t.Fatalf("expected %q to be rejected from ARCHIVED", trigger)
			}
			if entry.State != StateArchived {
				t.Fatalf("state: got %q want ARCHIVED unchanged", entry.State)
			}
		})
	}
}

// ── B1.3: configure-on-heartbeat dial + archive cleanup ─────────

// fakeConfigurer records the Dial it's asked to make and hands back a
// canned live handle (or error). The returned shard is a failingShard
// (a no-op shard.Shard) when none is set — the SM only needs a non-nil
// pointer to attach.
type fakeConfigurer struct {
	mu       sync.Mutex
	dialAddr string
	dialReq  *pb.ConfigureRequest
	err      error
	returned shard.Shard
}

func (f *fakeConfigurer) Dial(_ context.Context, addr string, req *pb.ConfigureRequest) (shard.Shard, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dialAddr = addr
	f.dialReq = req
	if f.err != nil {
		return nil, f.err
	}
	if f.returned == nil {
		f.returned = &failingShard{id: req.ShardId}
	}
	return f.returned, nil
}

// fakeArchivableShard is a shard.Shard whose Archive returns a canned
// parquet path (or error) and records that it was archived + closed.
type fakeArchivableShard struct {
	id          string
	archivePath string
	archiveErr  error
	archived    bool
	closed      bool
}

func (s *fakeArchivableShard) ID() string                                      { return s.id }
func (s *fakeArchivableShard) Query(string, ...any) ([]*pb.ResultRow, error)   { return nil, nil }
func (s *fakeArchivableShard) QueryToSink(string, []any, shard.RowSink)        {}
func (s *fakeArchivableShard) Ingest([]*pb.LogRow, shard.IngestRecorder) error { return nil }
func (s *fakeArchivableShard) Close() error                                    { s.closed = true; return nil }
func (s *fakeArchivableShard) Archive(_ context.Context, _ string) (string, error) {
	s.archived = true
	if s.archiveErr != nil {
		return "", s.archiveErr
	}
	return s.archivePath, nil
}

func TestShardSM_ConfigureOnHeartbeatDialsAndAttaches(t *testing.T) {
	fc := &fakeConfigurer{}
	sm, _, entry := makeFixtureShardSM(t, StateLaunching, func(d *ShardSMDeps) { d.Configurer = fc })
	entry.InstanceID = "i-abc"
	entry.Services = []store.ShardService{{NamespaceID: 1, Service: "warp"}}
	entry.WindowStart = time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	entry.WindowEnd = time.Date(2026, 5, 21, 13, 0, 0, 0, time.UTC)
	entry.HeartbeatToken = "tok-xyz"

	hb := store.ShardHeartbeat{ShardID: "s-test", Addr: "10.0.0.1:9000", InstanceID: "i-abc"}
	if err := sm.Fire(context.Background(), TriggerHeartbeat, hb); err != nil {
		t.Fatalf("Fire heartbeat: %v", err)
	}
	if st, _ := sm.State(context.Background()); st != StateIngesting {
		t.Fatalf("state: got %q want INGESTING", st)
	}
	if fc.dialAddr != "10.0.0.1:9000" {
		t.Fatalf("dial addr: got %q", fc.dialAddr)
	}
	if fc.dialReq.ShardId != "s-test" || fc.dialReq.HeartbeatToken != "tok-xyz" {
		t.Fatalf("dial req mismatched: %+v", fc.dialReq)
	}
	if len(fc.dialReq.Services) != 1 || fc.dialReq.Services[0] != "warp" {
		t.Fatalf("dial req services: %v", fc.dialReq.Services)
	}
	if fc.dialReq.WindowStart != "2026-05-21T12:00:00Z" || fc.dialReq.WindowEnd != "2026-05-21T13:00:00Z" {
		t.Fatalf("dial req window: start=%q end=%q", fc.dialReq.WindowStart, fc.dialReq.WindowEnd)
	}
	if entry.Live == nil {
		t.Fatal("entry.Live not attached after a successful configure")
	}
}

func TestShardSM_ConfigureOnHeartbeatDialFailureSurfacesError(t *testing.T) {
	fc := &fakeConfigurer{err: fmt.Errorf("connection refused")}
	sm, _, entry := makeFixtureShardSM(t, StateLaunching, func(d *ShardSMDeps) { d.Configurer = fc })
	entry.InstanceID = "i-abc"

	hb := store.ShardHeartbeat{ShardID: "s-test", Addr: "10.0.0.1:9000", InstanceID: "i-abc"}
	err := sm.Fire(context.Background(), TriggerHeartbeat, hb)
	if err == nil {
		t.Fatal("expected the dial failure to surface from Fire")
	}
	if entry.Live != nil {
		t.Fatalf("entry.Live must stay nil on dial failure; got %T", entry.Live)
	}
	// stateless writes the destination before running OnEntry, so the row
	// is already INGESTING despite the failed dial — a later heartbeat
	// reconnects it (slice C).
	if st, _ := sm.State(context.Background()); st != StateIngesting {
		t.Fatalf("state: got %q want INGESTING (write lands before OnEntry)", st)
	}
}

func TestShardSM_EvaluateArchivingExportsRetiresTerminatesThenCompletes(t *testing.T) {
	fl := &fakeLauncher{}
	vm := &fakeVolumeManager{}
	live := &fakeArchivableShard{id: "s-test", archivePath: "s3://weft/warp/s-test.parquet"}
	sm, db, entry := makeFixtureShardSM(t, StateArchiving, func(d *ShardSMDeps) {
		d.Launcher = fl
		d.Volumes = vm
		d.ParquetDir = "s3://weft"
		d.LambdaFunctionURL = "https://fn.example/" // non-empty → ColdShard skips the duckdb open
	})
	entry.Live = live
	entry.Services = []store.ShardService{{NamespaceID: 1, Service: "warp"}}
	entry.EBSVolumeID = "vol-1"
	entry.InstanceID = "i-1"
	// Seed the persisted row too so the cleared-column assertions prove the
	// narrow writes ran (the fixture leaves these blank).
	if err := (ShardStore{db}).SetShardLaunchResult("s-test", "i-1", "us-east-2a", "vol-1", nil); err != nil {
		t.Fatalf("seed launch result: %v", err)
	}

	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !live.archived {
		t.Fatal("live shard was not exported")
	}
	if !live.closed {
		t.Fatal("live shard not closed after the ColdShard swap")
	}
	rec, _, _ := ShardStore{db}.GetShard("s-test")
	if rec.ParquetPath != "s3://weft/warp/s-test.parquet" {
		t.Fatalf("parquet path not persisted: %q", rec.ParquetPath)
	}
	if rec.EBSVolumeID != "" {
		t.Fatalf("volume not retired/cleared: %q", rec.EBSVolumeID)
	}
	if rec.InstanceID != "" {
		t.Fatalf("instance not cleared: %q", rec.InstanceID)
	}
	if len(fl.terminated) != 1 || fl.terminated[0] != "i-1" {
		t.Fatalf("instance not terminated: %v", fl.terminated)
	}
	if _, ok := entry.Live.(*shard.ColdShard); !ok {
		t.Fatalf("entry.Live not swapped to ColdShard: %T", entry.Live)
	}
	// All three steps landed in one tick → the shard fires ArchiveComplete
	// itself and reaches the clean terminal.
	if st, _ := sm.State(context.Background()); st != StateArchived {
		t.Fatalf("state: got %q want ARCHIVED after a complete archiving pass", st)
	}
}

func TestShardSM_EvaluateArchivedTerminalNoop(t *testing.T) {
	fl := &fakeLauncher{}
	vm := &fakeVolumeManager{}
	sm, _, _ := makeFixtureShardSM(t, StateArchived, func(d *ShardSMDeps) {
		d.Launcher = fl
		d.Volumes = vm
	})

	// ARCHIVED is terminal — Evaluate must do nothing (no re-terminate,
	// no re-export) however the shard got rehydrated.
	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(fl.terminated) != 0 {
		t.Fatalf("a terminal ARCHIVED shard should do nothing; terminated=%v", fl.terminated)
	}
	if st, _ := sm.State(context.Background()); st != StateArchived {
		t.Fatalf("state: got %q want ARCHIVED unchanged", st)
	}
}

func TestShardSM_EvaluateArchivingRetriesOnExportFailure(t *testing.T) {
	fl := &fakeLauncher{}
	vm := &fakeVolumeManager{}
	live := &fakeArchivableShard{id: "s-test", archiveErr: fmt.Errorf("s3 unreachable")}
	sm, db, entry := makeFixtureShardSM(t, StateArchiving, func(d *ShardSMDeps) {
		d.Launcher = fl
		d.Volumes = vm
		d.ParquetDir = "s3://weft"
	})
	entry.Live = live
	entry.EBSVolumeID = "vol-1"
	entry.InstanceID = "i-1"
	if err := (ShardStore{db}).SetShardLaunchResult("s-test", "i-1", "us-east-2a", "vol-1", nil); err != nil {
		t.Fatalf("seed launch result: %v", err)
	}

	// A failed export is swallowed (logged) so the next tick retries; it
	// must not advance to volume/instance cleanup with the data unexported,
	// nor fire ArchiveComplete.
	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate should swallow the export failure for retry, got: %v", err)
	}
	rec, _, _ := ShardStore{db}.GetShard("s-test")
	if rec.ParquetPath != "" {
		t.Fatalf("parquet path should be empty after a failed export: %q", rec.ParquetPath)
	}
	if rec.EBSVolumeID != "vol-1" {
		t.Fatalf("volume retired before the export landed: %q", rec.EBSVolumeID)
	}
	if len(fl.terminated) != 0 {
		t.Fatalf("instance terminated before the export landed: %v", fl.terminated)
	}
	if st, _ := sm.State(context.Background()); st != StateArchiving {
		t.Fatalf("state: got %q want ARCHIVING (incomplete archive must not advance)", st)
	}
}
