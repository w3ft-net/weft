package logdb

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

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
func makeFixtureShardSM(t *testing.T, initialState ShardState, customize ...func(*ShardSMDeps)) (*ShardSM, *store.Store, *ShardEntry) {
	t.Helper()
	db := openSMTestStore(t)
	bucket := seedBucket(t, db, BucketStasis)
	if err := db.SaveShard(&store.ShardRecord{
		ID:          "s-test",
		State:       string(initialState),
		BucketID:    bucket.ID,
		Tier:        "small",
		WindowStart: "2026-05-21T12:00:00Z",
		WindowEnd:   "2026-05-21T13:00:00Z",
		CreatedAt:   "2026-05-21T12:00:00Z",
	}); err != nil {
		t.Fatalf("seed shard: %v", err)
	}
	entry := &ShardEntry{
		ID:       "s-test",
		BucketID: bucket.ID,
		Tier:     "small",
		State:    initialState,
	}
	clock := func() time.Time { return time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC) }
	deps := ShardSMDeps{
		Store:          db,
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
	rec, _, _ := db.GetShard("s-test")
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
	rec, _, _ := db.GetShard("s-test")
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
	entry.EBSVolumeID = "vol-1" // volume intact → SUSPENDED, not GONE
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
	if _, ok, err := db.GetShard("s-test"); err != nil || ok {
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

func TestShardSM_ReadOnlyToArchived(t *testing.T) {
	sm, _, entry := makeFixtureShardSM(t, StateReadOnly)
	if err := sm.Fire(context.Background(), TriggerArchiveRequested); err != nil {
		t.Fatalf("Fire Archive: %v", err)
	}
	if entry.State != StateArchived {
		t.Fatalf("state: got %q want ARCHIVED", entry.State)
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
	if entry.InstanceID != "" || entry.Addr != "" {
		t.Fatalf("SUSPENDED.OnEntry should have cleared InstanceID/Addr; got %q/%q",
			entry.InstanceID, entry.Addr)
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
	if _, ok, err := db.GetShard("s-test"); err != nil || ok {
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
