package logdb

import (
	"testing"
	"time"

	"github.com/graybeardcorp/weft/shard"
	"github.com/graybeardcorp/weft/store"
)

// seedGapTestShard writes a bucket + shard row via the given store handle
// and returns it.
func seedGapTestShard(t *testing.T, tx *store.Store, id string) *Shard {
	t.Helper()
	bucket := &store.Bucket{
		Label:                "leader-gap-test-" + id,
		TargetIngestingCount: 1,
		Tier:                 "small",
		CreatedAt:            time.Now().UTC().Format(time.RFC3339),
		WindowDuration:       "1h",
	}
	if err := tx.SaveBucket(bucket); err != nil {
		t.Fatalf("seed bucket: %v", err)
	}
	now := time.Now().UTC()
	sh := &Shard{
		ID:          id,
		WindowStart: now.Add(-time.Hour),
		WindowEnd:   now.Add(time.Hour),
		State:       StateIngesting,
		Tier:        "small",
		CreatedAt:   now,
		BucketID:    bucket.ID,
	}
	if err := (ShardStore{tx}).SaveShard(sh); err != nil {
		t.Fatalf("seed shard: %v", err)
	}
	return sh
}

// noReconstruct is the "coordinator restart / leadership handoff" LoadFromStore
// reconstruct stand-in: no live handle is attached, matching the production
// reconstruct for anything that isn't an ARCHIVED shard.
func noReconstruct(*Shard) (shard.Shard, error) { return nil, nil }

// TestRefreshFromStore_SyncsLaunchBookkeeping pins
// docs/plans/coordinator-horizontal-scaling.md #5 "Real gap #1": AZ,
// StripeIndex, and LaunchAttempts used to be permanently excluded from
// RefreshFromStore ("FSM-launch bookkeeping... deliberately NOT
// refreshed"). That was fine when only one coordinator ever touched a
// shard's FSM, but once #5 gates convergence to a single elected leader, a
// freshly-elected leader that never personally launched a shard would
// start from a stale/zero local copy — most dangerously AZ, which pins a
// SUSPENDED shard's recovery relaunch to the surviving EBS volume's AZ
// (EBS volumes are AZ-locked, so a wrong AZ fails to reattach).
//
// Simulates a leadership handoff: one store write path stands in for the
// coordinator that actually ran the launch (DB-then-memory, matching
// shardsm.go's runLaunch ordering); a second, independent registry —
// standing in for the newly-elected leader, which never touched this
// shard before — must pick the values up via RefreshFromStore.
func TestRefreshFromStore_SyncsLaunchBookkeeping(t *testing.T) {
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	tx, rollback := db.BeginTest()
	t.Cleanup(func() {
		rollback()
		db.Close()
	})

	seedGapTestShard(t, tx, "shard-launch-bookkeeping")

	stripe := 2
	if err := (ShardStore{tx}).SetShardLaunchResult("shard-launch-bookkeeping", "i-abc123", "us-east-2b", "vol-xyz", &stripe); err != nil {
		t.Fatalf("SetShardLaunchResult: %v", err)
	}
	if err := (ShardStore{tx}).IncrementShardLaunchAttempts("shard-launch-bookkeeping"); err != nil {
		t.Fatalf("IncrementShardLaunchAttempts: %v", err)
	}
	if err := (ShardStore{tx}).IncrementShardLaunchAttempts("shard-launch-bookkeeping"); err != nil {
		t.Fatalf("IncrementShardLaunchAttempts: %v", err)
	}

	// The "new leader": a registry hydrated fresh (as LoadFromStore would
	// after an election handoff), which never personally wrote any of the
	// above, then RefreshFromStore'd on its own tick.
	newLeader := NewShardRegistry()
	newLeader.SetStore(ShardStore{tx})
	if err := newLeader.LoadFromStore(noReconstruct); err != nil {
		t.Fatalf("LoadFromStore: %v", err)
	}

	entry := newLeader.Get("shard-launch-bookkeeping")
	if entry == nil {
		t.Fatal("shard missing after LoadFromStore")
	}
	// LoadFromStore already reads a fresh row, so exercise the case that
	// actually matters — a RefreshFromStore call on an entry the registry
	// already held before the DB values changed underneath it.
	entry.mu.Lock()
	entry.AZ = ""
	entry.StripeIndex = nil
	entry.LaunchAttempts = 0
	entry.mu.Unlock()

	newLeader.RefreshFromStore()

	entry.mu.RLock()
	az, stripeIdx, attempts := entry.AZ, entry.StripeIndex, entry.LaunchAttempts
	entry.mu.RUnlock()

	if az != "us-east-2b" {
		t.Errorf("AZ after RefreshFromStore: got %q, want us-east-2b — a leadership handoff would relaunch into the wrong AZ and fail to reattach the surviving EBS volume", az)
	}
	if stripeIdx == nil || *stripeIdx != 2 {
		t.Errorf("StripeIndex after RefreshFromStore: got %v, want 2", stripeIdx)
	}
	if attempts != 2 {
		t.Errorf("LaunchAttempts after RefreshFromStore: got %d, want 2", attempts)
	}
}

// TestRecordIngestFailure_PersistsAcrossCoordinators pins
// docs/plans/coordinator-horizontal-scaling.md #5 "Real gap #2":
// RecordIngestFailure/ResetIngestFailures used to be pure in-memory bumps
// with no persist call. Once evaluateStateMachines (and its
// evaluateIngesting circuit breaker) is gated to a single leader, the
// leader is very likely reading a counter some OTHER coordinator — the one
// actually routing this shard's ingest writes — is the one incrementing.
// Without persistence the DEGRADED circuit breaker goes silently inert.
//
// Simulates that split: one registry's RecordIngestFailure stands in for
// the coordinator routing ingest; a second, independent registry (the
// leader evaluating the circuit breaker) must see the count via
// RefreshFromStore.
func TestRecordIngestFailure_PersistsAcrossCoordinators(t *testing.T) {
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	tx, rollback := db.BeginTest()
	t.Cleanup(func() {
		rollback()
		db.Close()
	})

	seedGapTestShard(t, tx, "shard-ingest-failures")

	ingestRouter := NewShardRegistry()
	ingestRouter.SetStore(ShardStore{tx})
	if err := ingestRouter.LoadFromStore(noReconstruct); err != nil {
		t.Fatalf("LoadFromStore (ingest router): %v", err)
	}
	for i := 0; i < SuspendFailureThreshold; i++ {
		ingestRouter.RecordIngestFailure("shard-ingest-failures")
	}

	leader := NewShardRegistry()
	leader.SetStore(ShardStore{tx})
	if err := leader.LoadFromStore(noReconstruct); err != nil {
		t.Fatalf("LoadFromStore (leader): %v", err)
	}
	leader.RefreshFromStore()

	entry := leader.Get("shard-ingest-failures")
	if entry == nil {
		t.Fatal("shard missing from leader registry")
	}
	entry.mu.RLock()
	got := entry.IngestFailures
	entry.mu.RUnlock()
	if got != SuspendFailureThreshold {
		t.Fatalf("leader's IngestFailures after RefreshFromStore: got %d, want %d — the ingest circuit breaker would never trip on a coordinator that isn't routing this shard's writes", got, SuspendFailureThreshold)
	}

	// Reset must clear the persisted counter too, so a later leader
	// doesn't see a stale non-zero count and refuse to re-admit a shard
	// that's actually healthy again.
	ingestRouter.ResetIngestFailures("shard-ingest-failures")
	leader.RefreshFromStore()
	entry.mu.RLock()
	got = entry.IngestFailures
	entry.mu.RUnlock()
	if got != 0 {
		t.Fatalf("leader's IngestFailures after reset+RefreshFromStore: got %d, want 0", got)
	}
}

// TestResetIngestFailures_SkipsWriteWhenAlreadyZero pins the write-
// amplification guard: ResetIngestFailures runs on every successful
// ingest batch (the hot path — see shard.Ingest's rec.ResetIngestFailures
// call), so persisting an already-zero counter on every call would turn a
// safety mechanism's fix into a throughput problem. Only a transition from
// nonzero to zero should reach the store.
func TestResetIngestFailures_SkipsWriteWhenAlreadyZero(t *testing.T) {
	fake := newFakeRegistryStore()
	reg := NewShardRegistry()
	reg.SetStore(fake)
	reg.Add(&Shard{ID: "shard-reset-noop", State: StateIngesting, CreatedAt: time.Now()})

	baseline := fake.ingestFailureWrites
	reg.ResetIngestFailures("shard-reset-noop") // already 0 — must not write
	if fake.ingestFailureWrites != baseline {
		t.Fatalf("ResetIngestFailures wrote to the store when the counter was already 0 (writes %d -> %d)", baseline, fake.ingestFailureWrites)
	}

	reg.RecordIngestFailure("shard-reset-noop") // now nonzero
	reg.ResetIngestFailures("shard-reset-noop")
	rec, ok := fake.get("shard-reset-noop")
	if !ok {
		t.Fatal("shard missing from fake store")
	}
	if rec.IngestFailures != 0 {
		t.Fatalf("IngestFailures after a real reset: got %d, want 0", rec.IngestFailures)
	}
}
