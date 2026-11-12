package logdb

import (
	"context"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/store"
)

// resetShardFSMLease clears any "shard-fsm-leader" row left behind by an
// earlier test — same rationale as coordinator's resetNATLease: tests in
// this file share one pooled DB per process (store.TestDSN's doc comment)
// and all exercise the same production lease name (a package constant),
// so each test must start from a clean slate.
func resetShardFSMLease(t *testing.T, db *store.Store) {
	t.Helper()
	if err := db.DB().Exec("DELETE FROM leases WHERE name = ?", shardFSMLeaseName).Error; err != nil {
		t.Fatalf("reset shard-fsm-leader lease: %v", err)
	}
}

// electorTestProvisioner builds a minimal Provisioner wired to db with the
// given holder id — enough to exercise leaderTick/IsLeader/markProgress
// without any real launcher or FSM control plane.
func electorTestProvisioner(db *store.Store, holder string, pollInterval time.Duration) *Provisioner {
	p := NewProvisioner(NewShardRegistry(), nil, ProvisionerConfig{
		Holder:       holder,
		PollInterval: pollInterval,
	})
	p.SetStore(db)
	// NewProvisioner defaults IsLeader() to true (the no-election-running
	// shape); these tests want to observe leaderTick actually deciding
	// leadership, so start from the not-yet-decided state.
	p.leading.Store(false)
	return p
}

// TestLeaderTick_AcquiresLeaseAndBecomesLeader confirms the core contract:
// a lone process wins the initially-unheld lease and IsLeader() flips true.
func TestLeaderTick_AcquiresLeaseAndBecomesLeader(t *testing.T) {
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	resetShardFSMLease(t, db)

	p := electorTestProvisioner(db, "coord-a", time.Minute)
	p.leaderTick()
	if !p.IsLeader() {
		t.Fatal("expected to hold the shard-fsm-leader lease after the first tick")
	}
}

// TestLeaderTick_DoesNotChallengeHealthyIncumbent: the lease's self-renewal
// bias means a challenger doesn't win while the incumbent keeps renewing
// and making progress.
func TestLeaderTick_DoesNotChallengeHealthyIncumbent(t *testing.T) {
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	resetShardFSMLease(t, db)

	a := electorTestProvisioner(db, "coord-a", time.Minute)
	a.leaderTick()
	if !a.IsLeader() {
		t.Fatal("incumbent should win the initially-unheld lease")
	}

	b := electorTestProvisioner(db, "coord-b", time.Minute)
	b.leaderTick()
	if b.IsLeader() {
		t.Fatal("challenger must not win the lease while the incumbent is still renewing")
	}

	// Incumbent keeps making progress and renewing; challenger keeps losing.
	a.markProgress()
	a.leaderTick()
	b.leaderTick()
	if !a.IsLeader() || b.IsLeader() {
		t.Fatalf("incumbent should keep the lease across ticks: a=%v b=%v", a.IsLeader(), b.IsLeader())
	}
}

// TestLeaderTick_ChallengerTakesOverAfterIncumbentExpiry: failover actually
// happens once the incumbent stops renewing.
func TestLeaderTick_ChallengerTakesOverAfterIncumbentExpiry(t *testing.T) {
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	resetShardFSMLease(t, db)

	// Seed an already-expired lease directly, simulating an incumbent that
	// died LeaseTTL+ ago rather than waiting out the real TTL here.
	if ok, err := db.AcquireLease(shardFSMLeaseName, "coord-dead", -time.Second); err != nil || !ok {
		t.Fatalf("setup: seed expired lease failed: ok=%v err=%v", ok, err)
	}

	b := electorTestProvisioner(db, "coord-b", time.Minute)
	b.leaderTick()
	if !b.IsLeader() {
		t.Fatal("challenger should take over an expired lease")
	}
}

// TestLeaderTick_ProgressStaleReleasesLease pins "Real gap #3": a process
// that can still reach Postgres (so a plain AcquireLease renewal would
// keep succeeding forever) but whose evaluateStateMachines has stopped
// making progress must give up the lease rather than block out every
// other coordinator indefinitely. Confirms both the local flag and the
// actual DB row are released — a challenger can win it right after.
func TestLeaderTick_ProgressStaleReleasesLease(t *testing.T) {
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	resetShardFSMLease(t, db)

	// A short PollInterval so progressStaleFactor*PollInterval is easy to
	// simulate crossing without a real sleep.
	p := electorTestProvisioner(db, "coord-stuck", 10*time.Millisecond)
	p.leaderTick()
	if !p.IsLeader() {
		t.Fatal("expected to acquire the lease on the first tick")
	}

	// Simulate a stuck evaluateStateMachines: back-date lastProgress past
	// progressStaleFactor*PollInterval without ever calling markProgress.
	p.lastProgress.Store(time.Now().Add(-time.Hour).UnixNano())

	p.leaderTick()
	if p.IsLeader() {
		t.Fatal("expected the deadman's switch to release the lease once progress went stale")
	}

	// The DB row itself must be gone (not just the local flag) — a
	// challenger should be able to win it immediately, not wait out a full
	// LeaseTTL for an unreleased-but-abandoned row.
	challenger := electorTestProvisioner(db, "coord-b", time.Minute)
	challenger.leaderTick()
	if !challenger.IsLeader() {
		t.Fatal("challenger should win immediately after the stuck leader released the lease")
	}
}

// TestCheck_EvaluateStateMachinesGatedBehindLeader confirms check()'s
// exclusive half (evaluateStateMachines — launches, terminates, persisted
// transitions) only runs while leading, while its non-exclusive half
// (RefreshFromStore, syncShardServices) stays unconditional. Drives check()
// directly with p.leading forced false, so it doesn't need a real second
// coordinator contending for the lease — see docs/plans/
// coordinator-horizontal-scaling.md #5's "what gets gated vs. what stays
// unconditional" breakdown.
func TestCheck_EvaluateStateMachinesGatedBehindLeader(t *testing.T) {
	db := openSMTestStore(t)
	seedDefaultBucket(t, db, 2, time.Hour)
	prov, launcher := newFSMTestProvisioner(t, db)
	ctx := context.Background()

	prov.leading.Store(false)
	prov.check(ctx)
	prov.evalWG.Wait() // shard Evaluates run in the background pool

	if got := launcher.launchCountSafe(); got != 0 {
		t.Fatalf("non-leader must not launch shards: got %d launches", got)
	}
	if got := countByState(prov, StateLaunching); got != 0 {
		t.Fatalf("non-leader must not mint LAUNCHING shards: got %d", got)
	}

	// Flip to leader and confirm the same provisioner now converges —
	// proves the gate is the only thing that was holding it back, not
	// some other broken precondition.
	prov.leading.Store(true)
	prov.check(ctx)
	prov.evalWG.Wait() // shard Evaluates run in the background pool
	drainLaunches(t, prov, launcher, 2)
	if got := launcher.launchCountSafe(); got != 2 {
		t.Fatalf("expected 2 launches once leading, got %d", got)
	}
}

// TestProcessHeartbeat_PromoteGatedBehindLeader confirms a non-leader that
// receives a promotable heartbeat (LAUNCHING/SUSPENDED, or the empty-
// ShardID christen bootstrap) drops the promotion silently rather than
// firing the FSM transition — evaluateStateMachines's exclusivity
// requirement extends to any FSM-transition-firing heartbeat handling, not
// just the tick. See docs/plans/coordinator-horizontal-scaling.md #5.
func TestProcessHeartbeat_PromoteGatedBehindLeader(t *testing.T) {
	db := openSMTestStore(t)
	seedDefaultBucket(t, db, 1, time.Hour)
	prov, launcher := newFSMTestProvisioner(t, db)
	ctx := context.Background()

	prov.check(ctx)    // leading (default true) — mint + launch the one shard
	prov.evalWG.Wait() // shard Evaluates run in the background pool
	drainLaunches(t, prov, launcher, 1)
	if got := countByState(prov, StateLaunching); got != 1 {
		t.Fatalf("expected 1 LAUNCHING shard set up, got %d", got)
	}

	prov.leading.Store(false)
	simulateHeartbeats(prov)
	if got := prov.registry.TotalIngesting(); got != 0 {
		t.Fatalf("non-leader must not promote on heartbeat: got %d INGESTING", got)
	}

	prov.leading.Store(true)
	simulateHeartbeats(prov)
	if got := prov.registry.TotalIngesting(); got != 1 {
		t.Fatalf("expected promotion once leading, got %d INGESTING", got)
	}
}
