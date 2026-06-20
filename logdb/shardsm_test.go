package logdb

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
	"github.com/graybeardcorp/weft/store"
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

// makeFixtureShardSM builds a shard in initialState backed by a real
// (transaction-scoped) store, with its FSM bound. Seeds a bucket +
// shard_records row, then returns the shard three ways for call-site
// convenience: the first and third returns are the SAME *Shard (the type
// is its own SM now — sm and entry collapsed), the middle is the store.
func makeFixtureShardSM(t *testing.T, initialState ShardState, customize ...func(*ShardDeps)) (*Shard, *store.Store, *Shard) {
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
	deps := ShardDeps{
		Store:          ShardStore{db},
		Clock:          clock,
		StaleThreshold: 2 * time.Minute,
		// Default to "booted a while ago" so a shard's own LastHeartbeatAt
		// (not the boot anchor) drives staleness in the common-case tests.
		// Restart-grace tests override this to simulate a fresh boot.
		BootedAt: clock().Add(-2 * time.Hour),
	}
	for _, c := range customize {
		c(&deps)
	}
	entry.ensureSM(newShardDeps(deps))
	return entry, db, entry
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
func waitForLaunch(t *testing.T, sm *Shard) {
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
	sm, db, entry := makeFixtureShardSM(t, StateLaunching, func(d *ShardDeps) { d.Launcher = fl })
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
	sm, _, entry := makeFixtureShardSM(t, StateLaunching, func(d *ShardDeps) { d.Launcher = fl })
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
	sm, _, entry := makeFixtureShardSM(t, StateLaunching, func(d *ShardDeps) {
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
	st, _ := sm.smState(context.Background())
	if st != StateGone {
		t.Fatalf("state: got %q want GONE", st)
	}
}

func TestShardSM_EvaluateLaunchFailureBumpsAttempts(t *testing.T) {
	fl := &fakeLauncher{err: fmt.Errorf("InsufficientInstanceCapacity")}
	sm, db, entry := makeFixtureShardSM(t, StateLaunching, func(d *ShardDeps) { d.Launcher = fl })
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
	st, _ := sm.smState(context.Background())
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
	st, _ := sm.smState(context.Background())
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
	st, _ := sm.smState(context.Background())
	if st != StateIngesting {
		t.Fatalf("a fresh shard should stay INGESTING; got %q", st)
	}
}

// TestShardSM_RestartGraceDoesNotSuspendOnPreBootHeartbeat pins the deploy
// regression: a shard loaded after a coordinator restart carries a persisted
// last_heartbeat_at from BEFORE the new process booted. Measured absolutely
// that timestamp is stale, but the shard must get a grace window to
// re-report rather than being suspended+relaunched on the first tick (which
// is exactly what hit a healthy 13.5h-INGESTING shard in prod). BootedAt ==
// clock here models "the coordinator just came up".
func TestShardSM_RestartGraceDoesNotSuspendOnPreBootHeartbeat(t *testing.T) {
	bootAt := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC) // == fixture clock: fresh boot
	sm, _, entry := makeFixtureShardSM(t, StateIngesting, func(d *ShardDeps) { d.BootedAt = bootAt })
	entry.EBSVolumeID = "vol-1"
	entry.LastHeartbeatAt = time.Date(2026, 5, 21, 11, 30, 0, 0, time.UTC) // 30m pre-boot — stale absolutely

	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if st, _ := sm.smState(context.Background()); st != StateIngesting {
		t.Fatalf("a shard loaded after a restart must get a grace window, not be suspended on a pre-boot heartbeat; got %q", st)
	}
}

// TestShardSM_StaleAfterRestartGraceSuspends is the other half: once the
// post-boot grace window elapses with no fresh heartbeat, a shard IS stale
// and suspends — a genuinely dead shard is still caught (what the blunt
// "zero on load" approach would have missed). BootedAt is 3m < clock, past
// the 2m threshold, and the shard never re-reported since boot.
func TestShardSM_StaleAfterRestartGraceSuspends(t *testing.T) {
	bootAt := time.Date(2026, 5, 21, 11, 57, 0, 0, time.UTC) // 3m before the fixture clock
	sm, _, entry := makeFixtureShardSM(t, StateIngesting, func(d *ShardDeps) { d.BootedAt = bootAt })
	entry.EBSVolumeID = "vol-1"                                            // volume intact → SUSPENDED, not GONE
	entry.LastHeartbeatAt = time.Date(2026, 5, 21, 11, 30, 0, 0, time.UTC) // pre-boot, never re-reported

	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if st, _ := sm.smState(context.Background()); st != StateSuspended {
		t.Fatalf("a shard that never re-reported within the post-boot grace must suspend; got %q", st)
	}
}

// TestShardSM_EvaluateIngestingDegradesOnFailures: an INGESTING shard
// whose consecutive ingest failures reach SuspendFailureThreshold trips
// the breaker → DEGRADED.
func TestShardSM_EvaluateIngestingDegradesOnFailures(t *testing.T) {
	sm, _, entry := makeFixtureShardSM(t, StateIngesting)
	entry.IngestFailures = SuspendFailureThreshold // breaker tripped
	// LastHeartbeatAt zero → fresh (not stale), so the degrade check runs.

	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if st, _ := sm.smState(context.Background()); st != StateDegraded {
		t.Fatalf("state: got %q want DEGRADED", st)
	}
}

func TestShardSM_EvaluateIngestingHealthyStaysIngesting(t *testing.T) {
	sm, _, entry := makeFixtureShardSM(t, StateIngesting)
	entry.IngestFailures = SuspendFailureThreshold - 1 // below the breaker

	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if st, _ := sm.smState(context.Background()); st != StateIngesting {
		t.Fatalf("a healthy shard should stay INGESTING; got %q", st)
	}
}

// TestShardSM_EvaluateIngestingDegradesOnReconnectFailures pins case #2 of
// the undialable-shard fix: a shard that's ALREADY INGESTING (e.g. loaded
// handle-less after a coordinator restart, or one that goes unreachable
// mid-life) but which the coordinator can no longer dial — the SG-gap /
// partition case — must not sit as a healthy-looking INGESTING zombie.
// Repeated reconnect failures on the heartbeat path trip the same circuit
// breaker the ingest path uses, dropping it to DEGRADED (out of write
// routing, visible) until it's reachable again.
func TestShardSM_EvaluateIngestingDegradesOnReconnectFailures(t *testing.T) {
	fc := &fakeConfigurer{err: fmt.Errorf("dial tcp 172.31.129.96:9000: i/o timeout")}
	sm, _, entry := makeFixtureShardSM(t, StateIngesting, func(d *ShardDeps) { d.Configurer = fc })
	entry.InstanceID = "i-abc"
	// Live==nil + a non-empty heartbeat Addr makes ObserveHeartbeat attempt a
	// reconnect every beat (the post-restart shape); the failing configurer
	// makes every attempt time out.

	hb := store.ShardHeartbeat{ShardID: "s-test", Addr: "172.31.129.96:9000", InstanceID: "i-abc"}
	for i := 0; i < SuspendFailureThreshold; i++ {
		entry.ObserveHeartbeat(context.Background(), hb)
	}
	if entry.Live != nil {
		t.Fatal("reconnect must not attach a handle when the dial fails")
	}

	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if st, _ := sm.smState(context.Background()); st != StateDegraded {
		t.Fatalf("an undialable INGESTING shard should degrade after %d reconnect failures; got %q", SuspendFailureThreshold, st)
	}
}

// TestShardSM_EvaluateDegradedRecoversAfterCooldown: once the cooldown
// elapses, a DEGRADED shard half-opens back to INGESTING with its failure
// count reset.
func TestShardSM_EvaluateDegradedRecoversAfterCooldown(t *testing.T) {
	sm, _, entry := makeFixtureShardSM(t, StateDegraded)
	entry.IngestFailures = SuspendFailureThreshold
	// clock() is 12:00; degraded well past the cooldown.
	entry.degradedAt = time.Date(2026, 5, 21, 11, 55, 0, 0, time.UTC) // 5m ago > 2m cooldown

	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if st, _ := sm.smState(context.Background()); st != StateIngesting {
		t.Fatalf("state: got %q want INGESTING (half-open)", st)
	}
	if entry.IngestFailures != 0 {
		t.Fatalf("failure count not reset on recovery: got %d", entry.IngestFailures)
	}
}

func TestShardSM_EvaluateDegradedStaysDuringCooldown(t *testing.T) {
	sm, _, entry := makeFixtureShardSM(t, StateDegraded)
	entry.IngestFailures = SuspendFailureThreshold
	entry.degradedAt = time.Date(2026, 5, 21, 11, 59, 30, 0, time.UTC) // 30s ago < 2m cooldown

	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if st, _ := sm.smState(context.Background()); st != StateDegraded {
		t.Fatalf("state: got %q want DEGRADED (still cooling down)", st)
	}
}

// TestShardSM_DegradedInstanceDeathGoesToSuspended: a DEGRADED shard whose
// instance also goes silent drops to the recovery branch, same as
// INGESTING (the inherited HeartbeatStale edge).
func TestShardSM_DegradedInstanceDeathGoesToSuspended(t *testing.T) {
	sm, _, entry := makeFixtureShardSM(t, StateDegraded)
	entry.EBSVolumeID = "vol-1"                                            // volume intact → SUSPENDED
	entry.LastHeartbeatAt = time.Date(2026, 5, 21, 11, 50, 0, 0, time.UTC) // 10m stale > 2m

	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if st, _ := sm.smState(context.Background()); st != StateSuspended {
		t.Fatalf("state: got %q want SUSPENDED", st)
	}
}

func TestShardSM_EvaluateSuspendedReapsThenRelaunches(t *testing.T) {
	fl := &fakeLauncher{}
	vm := &fakeVolumeManager{findResult: map[string]*VolumeInfo{"s-test": {VolumeID: "vol-1", AZ: "us-east-2a"}}}
	sm, db, entry := makeFixtureShardSM(t, StateSuspended, func(d *ShardDeps) {
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
	if st, _ := sm.smState(context.Background()); st != StateLaunching {
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
	sm, _, entry := makeFixtureShardSM(t, StateSuspended, func(d *ShardDeps) {
		d.Launcher = fl
		d.Volumes = vm
	})
	entry.InstanceID = "i-dead"
	entry.EBSVolumeID = "vol-gone"

	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if st, _ := sm.smState(context.Background()); st != StateGone {
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
	sm, _, entry := makeFixtureShardSM(t, StateSuspended, func(d *ShardDeps) {
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
	if st, _ := sm.smState(context.Background()); st != StateLaunching {
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

	got, err := sm.smState(context.Background())
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

// seedShardOperation inserts a row into shard_operations directly,
// bypassing the FSM. Used by tests that need a specific recovery
// history without driving the full state-machine cascade.
func seedShardOperation(t *testing.T, db *store.Store, shardID string, from, to ShardState) {
	t.Helper()
	if err := db.DB().Exec(
		"INSERT INTO shard_operations (shard_id, from_state, to_state) VALUES (?, ?, ?)",
		shardID, string(from), string(to),
	).Error; err != nil {
		t.Fatalf("seed shard_operations %s %s→%s: %v", shardID, from, to, err)
	}
}

// TestShardSM_LaunchingFromUpgradingPreservesReadOnly asserts the post-
// recovery state lookup: a READ_ONLY shard that was rolled (UPGRADING)
// must return to READ_ONLY on the post-launch heartbeat, not bounce
// through INGESTING. The transition log in shard_operations is the
// source of truth.
func TestShardSM_LaunchingFromUpgradingPreservesReadOnly(t *testing.T) {
	sm, db, entry := makeFixtureShardSM(t, StateLaunching)
	entry.InstanceID = "i-abc"
	// Synthesize the recovery history: at some point the shard was
	// READ_ONLY, then operator-rolled (UPGRADING), then evaluateRecovery
	// reaped and transitioned to LAUNCHING. The fixture only writes
	// (NULL, LAUNCHING) automatically, so we add the prior transitions.
	seedShardOperation(t, db, "s-test", StateReadOnly, StateUpgrading)
	seedShardOperation(t, db, "s-test", StateUpgrading, StateLaunching)

	hb := store.ShardHeartbeat{
		ShardID: "s-test", Addr: "10.0.0.1:9000", InstanceID: "i-abc",
	}
	if err := sm.Fire(context.Background(), TriggerHeartbeat, hb); err != nil {
		t.Fatalf("Fire heartbeat: %v", err)
	}
	got, _ := sm.smState(context.Background())
	if got != StateReadOnly {
		t.Fatalf("state: got %q want READ_ONLY (preserved across roll)", got)
	}
}

// TestShardSM_LaunchingFromSuspendedPreservesReadOnly mirrors the
// previous test for the HeartbeatStale → SUSPENDED organic-recovery
// path: same lookup, same result.
func TestShardSM_LaunchingFromSuspendedPreservesReadOnly(t *testing.T) {
	sm, db, entry := makeFixtureShardSM(t, StateLaunching)
	entry.InstanceID = "i-abc"
	seedShardOperation(t, db, "s-test", StateReadOnly, StateSuspended)
	seedShardOperation(t, db, "s-test", StateSuspended, StateLaunching)

	hb := store.ShardHeartbeat{
		ShardID: "s-test", Addr: "10.0.0.1:9000", InstanceID: "i-abc",
	}
	if err := sm.Fire(context.Background(), TriggerHeartbeat, hb); err != nil {
		t.Fatalf("Fire heartbeat: %v", err)
	}
	got, _ := sm.smState(context.Background())
	if got != StateReadOnly {
		t.Fatalf("state: got %q want READ_ONLY (preserved across SUSPENDED recovery)", got)
	}
}

// TestShardSM_LaunchingFromUpgradingFromIngestingGoesIngesting is the
// "common case" sanity check: when the prior state IS INGESTING
// (the normal write-routable rolls that operator triggers cover), the
// post-recovery target is still INGESTING. No regression on the
// dominant path.
func TestShardSM_LaunchingFromUpgradingFromIngestingGoesIngesting(t *testing.T) {
	sm, db, entry := makeFixtureShardSM(t, StateLaunching)
	entry.InstanceID = "i-abc"
	seedShardOperation(t, db, "s-test", StateIngesting, StateUpgrading)
	seedShardOperation(t, db, "s-test", StateUpgrading, StateLaunching)

	hb := store.ShardHeartbeat{
		ShardID: "s-test", Addr: "10.0.0.1:9000", InstanceID: "i-abc",
	}
	if err := sm.Fire(context.Background(), TriggerHeartbeat, hb); err != nil {
		t.Fatalf("Fire heartbeat: %v", err)
	}
	got, _ := sm.smState(context.Background())
	if got != StateIngesting {
		t.Fatalf("state: got %q want INGESTING", got)
	}
}

// TestShardSM_SuspendedRescueFromReadOnlyReturnsToReadOnly tests
// SUSPENDED's blip-rescue path: a READ_ONLY shard whose heartbeats
// stalled briefly (network blip → HeartbeatStale → SUSPENDED) should
// return to READ_ONLY on rescue, not be silently promoted to INGESTING.
func TestShardSM_SuspendedRescueFromReadOnlyReturnsToReadOnly(t *testing.T) {
	sm, db, entry := makeFixtureShardSM(t, StateSuspended)
	entry.EBSVolumeID = "vol-abc"
	entry.InstanceID = "i-alive"
	seedShardOperation(t, db, "s-test", StateReadOnly, StateSuspended)

	hb := store.ShardHeartbeat{
		ShardID: "s-test", Addr: "10.0.0.7:9000", InstanceID: "i-alive",
	}
	if err := sm.Fire(context.Background(), TriggerHeartbeat, hb); err != nil {
		t.Fatalf("Fire heartbeat: %v", err)
	}
	if entry.State != StateReadOnly {
		t.Fatalf("state: got %q want READ_ONLY (rescue preserves prior state)", entry.State)
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
	got, err := sm.smState(context.Background())
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
	got, err := sm.smState(context.Background())
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
	got, err := sm.smState(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if got != StateGone {
		t.Fatalf("state: got %q want GONE", got)
	}
}

// TestShardSM_RelaunchRequestedFromIngestingEntersUpgrading asserts the
// FSM table change: operator-driven RelaunchShard lands in UPGRADING
// (no heartbeat permit), not SUSPENDED (whose heartbeat-rescue permit
// would silently no-op the roll against a still-heartbeating process).
func TestShardSM_RelaunchRequestedFromIngestingEntersUpgrading(t *testing.T) {
	for _, src := range []ShardState{StateIngesting, StateReadOnly, StateDegraded} {
		t.Run(string(src), func(t *testing.T) {
			sm, _, _ := makeFixtureShardSM(t, src)
			if err := sm.Fire(context.Background(), TriggerRelaunchRequested); err != nil {
				t.Fatalf("Fire RelaunchRequested from %s: %v", src, err)
			}
			got, err := sm.smState(context.Background())
			if err != nil {
				t.Fatalf("State: %v", err)
			}
			if got != StateUpgrading {
				t.Fatalf("from %s: got %q want UPGRADING", src, got)
			}
		})
	}
}

// TestShardSM_UpgradingRejectsHeartbeatFromAliveInstance is the
// regression test for the Bug 1 silent-no-op: UPGRADING must reject
// heartbeats from the still-running shard process so the FSM commits
// to reap-and-relaunch instead of being lured back to INGESTING.
func TestShardSM_UpgradingRejectsHeartbeatFromAliveInstance(t *testing.T) {
	sm, _, entry := makeFixtureShardSM(t, StateUpgrading)
	entry.InstanceID = "i-alive" // shard process still up, still heartbeating

	hb := store.ShardHeartbeat{
		ShardID: "s-test", Addr: "10.0.0.7:9000", InstanceID: "i-alive",
	}
	err := sm.Fire(context.Background(), TriggerHeartbeat, hb)
	if err == nil {
		t.Fatalf("UPGRADING must reject TriggerHeartbeat: no error returned")
	}
	if entry.State != StateUpgrading {
		t.Fatalf("state: got %q want UPGRADING (rejected heartbeat)", entry.State)
	}
}

// TestShardSM_EvaluateUpgradingReapsThenRelaunches asserts UPGRADING
// shares evaluateRecovery with SUSPENDED — same reap-then-relaunch
// mechanics, just reached via a different trigger.
func TestShardSM_EvaluateUpgradingReapsThenRelaunches(t *testing.T) {
	fl := &fakeLauncher{}
	vm := &fakeVolumeManager{findResult: map[string]*VolumeInfo{"s-test": {VolumeID: "vol-1", AZ: "us-east-2a"}}}
	sm, _, entry := makeFixtureShardSM(t, StateUpgrading, func(d *ShardDeps) {
		d.Launcher = fl
		d.Volumes = vm
	})
	entry.InstanceID = "i-alive"
	entry.EBSVolumeID = "vol-1"
	entry.AZ = "us-east-2a"

	// Tick 1: reap the running instance, hand off to LAUNCHING.
	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate (reap): %v", err)
	}
	if len(fl.terminated) != 1 || fl.terminated[0] != "i-alive" {
		t.Fatalf("instance not reaped: terminated=%v", fl.terminated)
	}
	if entry.InstanceID != "" {
		t.Fatalf("InstanceID not cleared: %q", entry.InstanceID)
	}
	if st, _ := sm.smState(context.Background()); st != StateLaunching {
		t.Fatalf("after reap: got %q want LAUNCHING", st)
	}

	// Tick 2: LAUNCHING with the surviving volume → recovery relaunch
	// reattaches it.
	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate (relaunch): %v", err)
	}
	waitForLaunch(t, sm)
	if fl.launchCount() != 1 {
		t.Fatalf("launch calls: got %d want 1", fl.launchCount())
	}
	if fl.calls[0].ExistingVolumeID != "vol-1" {
		t.Fatalf("upgrade relaunch must reattach vol-1, got %q", fl.calls[0].ExistingVolumeID)
	}
}

// TestShardSM_SuspendedStillPermitsRescueHeartbeat is the
// "don't regress the feature" companion to the UPGRADING change:
// SUSPENDED's blip-rescue path stays intact, so a healthy shard whose
// heartbeats stalled briefly (network blip → HeartbeatStale →
// SUSPENDED) can resurrect without an instance churn.
func TestShardSM_SuspendedStillPermitsRescueHeartbeat(t *testing.T) {
	sm, _, entry := makeFixtureShardSM(t, StateSuspended)
	entry.EBSVolumeID = "vol-abc"
	entry.InstanceID = "i-alive"

	hb := store.ShardHeartbeat{
		ShardID: "s-test", Addr: "10.0.0.7:9000", InstanceID: "i-alive",
	}
	if err := sm.Fire(context.Background(), TriggerHeartbeat, hb); err != nil {
		t.Fatalf("Fire heartbeat: %v", err)
	}
	if entry.State != StateIngesting {
		t.Fatalf("state: got %q want INGESTING (blip rescue)", entry.State)
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
	id           string
	archivePath  string
	archiveStats []shard.CustomerStat
	archiveErr   error
	archived     bool
	closed       bool
}

func (s *fakeArchivableShard) ID() string                                      { return s.id }
func (s *fakeArchivableShard) Query(string, ...any) ([]*pb.ResultRow, error)   { return nil, nil }
func (s *fakeArchivableShard) QueryToSink(string, []any, shard.RowSink)        {}
func (s *fakeArchivableShard) Ingest([]*pb.LogRow, shard.IngestRecorder) error { return nil }
func (s *fakeArchivableShard) Close() error                                    { s.closed = true; return nil }
func (s *fakeArchivableShard) Archive(_ context.Context, _ string) (string, []shard.CustomerStat, error) {
	s.archived = true
	if s.archiveErr != nil {
		return "", nil, s.archiveErr
	}
	return s.archivePath, s.archiveStats, nil
}

// dialConfigure builds the Configure request from the entry's metadata and
// returns a live handle; the promote path then threads that handle through
// Fire so configureOnHeartbeat attaches it. (The dial moved out of the SM
// OnEntry so an undialable shard never reaches INGESTING — see
// promoteOnHeartbeat.)
func TestShardSM_DialConfigureBuildsRequestThenFireAttaches(t *testing.T) {
	fc := &fakeConfigurer{}
	sm, _, entry := makeFixtureShardSM(t, StateLaunching, func(d *ShardDeps) { d.Configurer = fc })
	entry.InstanceID = "i-abc"
	entry.Services = []store.ShardService{{NamespaceID: 1, Service: "warp"}}
	entry.WindowStart = time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	entry.WindowEnd = time.Date(2026, 5, 21, 13, 0, 0, 0, time.UTC)
	entry.HeartbeatToken = "tok-xyz"

	hb := store.ShardHeartbeat{ShardID: "s-test", Addr: "10.0.0.1:9000", InstanceID: "i-abc"}

	// Promote-gate step 1: dial + Configure, before any state change.
	dialed, err := entry.dialConfigure(context.Background(), hb)
	if err != nil {
		t.Fatalf("dialConfigure: %v", err)
	}
	if dialed == nil {
		t.Fatal("dialConfigure returned no handle on success")
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

	// Promote-gate step 2: fire with the dialed handle; OnEntry attaches it.
	if err := sm.Fire(context.Background(), TriggerHeartbeat, hb, dialed); err != nil {
		t.Fatalf("Fire heartbeat: %v", err)
	}
	if st, _ := sm.smState(context.Background()); st != StateIngesting {
		t.Fatalf("state: got %q want INGESTING", st)
	}
	if entry.Live != dialed {
		t.Fatal("configureOnHeartbeat did not attach the dialed handle")
	}
}

// TestShardSM_HeartbeatPersistsAddr pins the addr-clobber fix: a heartbeat
// that promotes a shard and sets its gRPC addr must persist that addr to
// shard_records.addr, not just set it in memory. Without persistence,
// RefreshFromStore re-reads the stale (empty) DB value and clobbers the
// live addr each tick, and a coordinator restart can't eager-dial the
// shard (ReconstructShard keys off shard_records.addr) — it stays invisible
// to routing until the next heartbeat re-dials.
func TestShardSM_HeartbeatPersistsAddr(t *testing.T) {
	sm, db, entry := makeFixtureShardSM(t, StateLaunching)
	entry.InstanceID = "i-abc" // so the heartbeat matches LAUNCHING

	hb := store.ShardHeartbeat{ShardID: "s-test", Addr: "10.0.0.1:9000", InstanceID: "i-abc"}
	if err := sm.Fire(context.Background(), TriggerHeartbeat, hb); err != nil {
		t.Fatalf("Fire heartbeat: %v", err)
	}
	if entry.Addr != "10.0.0.1:9000" {
		t.Fatalf("in-memory addr not set: got %q", entry.Addr)
	}
	rec, ok, err := (ShardStore{db}).GetShard("s-test")
	if err != nil || !ok {
		t.Fatalf("GetShard: ok=%v err=%v", ok, err)
	}
	if rec.Addr != "10.0.0.1:9000" {
		t.Fatalf("shard_records.addr not persisted: got %q want 10.0.0.1:9000", rec.Addr)
	}
}

// A dial failure surfaces from dialConfigure (the promote gate) so the
// caller can decline to fire — leaving the shard LAUNCHING with no handle
// rather than a half-promoted INGESTING zombie. The provisioner-level
// coverage is TestProcessHeartbeat_UndialableShardDoesNotPromote.
func TestShardSM_DialConfigureSurfacesDialFailure(t *testing.T) {
	fc := &fakeConfigurer{err: fmt.Errorf("connection refused")}
	sm, _, entry := makeFixtureShardSM(t, StateLaunching, func(d *ShardDeps) { d.Configurer = fc })
	entry.InstanceID = "i-abc"

	hb := store.ShardHeartbeat{ShardID: "s-test", Addr: "10.0.0.1:9000", InstanceID: "i-abc"}
	dialed, err := entry.dialConfigure(context.Background(), hb)
	if err == nil {
		t.Fatal("expected the dial failure to surface from dialConfigure")
	}
	if dialed != nil {
		t.Fatalf("no handle should be returned on dial failure; got %T", dialed)
	}
	// We never fired the promote trigger, so the shard stays LAUNCHING and
	// retries on the next heartbeat once reachable.
	if st, _ := sm.smState(context.Background()); st != StateLaunching {
		t.Fatalf("state: got %q want LAUNCHING (no promotion on dial failure)", st)
	}
}

func TestShardSM_EvaluateArchivingExportsRetiresTerminatesThenCompletes(t *testing.T) {
	fl := &fakeLauncher{}
	vm := &fakeVolumeManager{}
	statMin := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	statMax := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	live := &fakeArchivableShard{
		id:          "s-test",
		archivePath: "s3://weft/warp/s-test.parquet",
		archiveStats: []shard.CustomerStat{
			{Customer: "acme", MinTs: statMin, MaxTs: statMax, RowCount: 7},
			{Customer: "globex", MinTs: statMin, MaxTs: statMax, RowCount: 3},
		},
	}
	sm, db, entry := makeFixtureShardSM(t, StateArchiving, func(d *ShardDeps) {
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
	// The Archive RPC's per-customer stats land in shard_archive_stats for
	// the retention janitor to consult later.
	var stats []ShardArchiveStat
	if err := db.DB().Where("shard_id = ?", "s-test").Order("customer").Find(&stats).Error; err != nil {
		t.Fatalf("read archive stats: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("archive stats: got %d rows, want 2: %+v", len(stats), stats)
	}
	if stats[0].Customer != "acme" || stats[0].RowCount != 7 || !stats[0].MaxTs.Equal(statMax) {
		t.Fatalf("archive stat[0] = %+v, want acme/7/%v", stats[0], statMax)
	}
	if stats[1].Customer != "globex" || stats[1].RowCount != 3 {
		t.Fatalf("archive stat[1] = %+v, want globex/3", stats[1])
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
	if st, _ := sm.smState(context.Background()); st != StateArchived {
		t.Fatalf("state: got %q want ARCHIVED after a complete archiving pass", st)
	}
}

func TestShardSM_EvaluateArchivedTerminalNoop(t *testing.T) {
	fl := &fakeLauncher{}
	vm := &fakeVolumeManager{}
	sm, _, _ := makeFixtureShardSM(t, StateArchived, func(d *ShardDeps) {
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
	if st, _ := sm.smState(context.Background()); st != StateArchived {
		t.Fatalf("state: got %q want ARCHIVED unchanged", st)
	}
}

func TestShardSM_EvaluateArchivingRetriesOnExportFailure(t *testing.T) {
	fl := &fakeLauncher{}
	vm := &fakeVolumeManager{}
	live := &fakeArchivableShard{id: "s-test", archiveErr: fmt.Errorf("s3 unreachable")}
	sm, db, entry := makeFixtureShardSM(t, StateArchiving, func(d *ShardDeps) {
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
	if st, _ := sm.smState(context.Background()); st != StateArchiving {
		t.Fatalf("state: got %q want ARCHIVING (incomplete archive must not advance)", st)
	}
}

// TriggerRelaunchRequested is the admin-driven edge for in-place binary
// upgrades: an operator (or the kevcons deploy) asks for a shard to be
// relaunched onto the current AMI without archiving its data. It must be
// permitted from any "live with data" state (INGESTING, READ_ONLY,
// DEGRADED) and land on SUSPENDED, where the existing reap-and-relaunch
// recovery branch takes over (preserving the volume + identity).

// RelaunchRequested routing from INGESTING/READ_ONLY/DEGRADED is
// covered by TestShardSM_RelaunchRequestedFromIngestingEntersUpgrading
// above (the destination is UPGRADING, not SUSPENDED, after the Bug 1
// fix).

// States with no useful relaunch semantics must refuse the trigger so the
// caller (Provisioner.RelaunchShard) can map the rejection to a
// FailedPrecondition gRPC code. LAUNCHING is already coming up on the
// current AMI; ARCHIVING/ARCHIVED/GONE/SUSPENDED/UPGRADING have nothing
// to upgrade (UPGRADING is already mid-roll; SUSPENDED is mid-recovery).
func TestShardSM_RelaunchRequested_RejectedFromUnsupportedStates(t *testing.T) {
	for _, st := range []ShardState{StateLaunching, StateArchiving, StateArchived, StateGone, StateSuspended, StateUpgrading} {
		t.Run(string(st), func(t *testing.T) {
			sm, _, entry := makeFixtureShardSM(t, st)
			if err := sm.Fire(context.Background(), TriggerRelaunchRequested); err == nil {
				t.Fatalf("Fire from %s must be rejected; entry.State=%q", st, entry.State)
			}
			// stateless leaves the state unchanged when no permit fires.
			if entry.State != st {
				t.Fatalf("state must stay %s after rejected trigger; got %q", st, entry.State)
			}
		})
	}
}
