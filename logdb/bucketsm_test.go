package logdb

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/shardstore"
)

// fakeShardController records the bucket SM's child-shard directives
// instead of touching a real ShardSM. MintShard hands back a synthetic
// id; FireShard just logs the (shardID, trigger) pair. Tests assert on
// the recorded slices to verify cascade behaviour without standing up
// the provisioner.
type fakeShardController struct {
	mu      sync.Mutex
	minted  []MintSpec
	fired   []firedTrigger
	mintErr error
	fireErr error
	mintSeq int
}

type firedTrigger struct {
	shardID string
	trigger Trigger
}

func (f *fakeShardController) MintShard(_ context.Context, spec MintSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mintErr != nil {
		return "", f.mintErr
	}
	f.minted = append(f.minted, spec)
	f.mintSeq++
	return fmt.Sprintf("minted-%d", f.mintSeq), nil
}

func (f *fakeShardController) FireShard(_ context.Context, shardID string, trigger Trigger, _ ...any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fired = append(f.fired, firedTrigger{shardID, trigger})
	return f.fireErr
}

func (f *fakeShardController) firedFor(trigger Trigger) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for _, ft := range f.fired {
		if ft.trigger == trigger {
			ids = append(ids, ft.shardID)
		}
	}
	return ids
}

// makeFixtureBucketSM builds a BucketSM in initialState backed by a
// real (transaction-scoped) store and a fake controller. Seeds the
// buckets row, then returns the SM, the store, the Bucket, and the fake
// controller. openSMTestStore + seedBucket live in shardsm_test.go.
func makeFixtureBucketSM(t *testing.T, initialState BucketState) (*BucketSM, shardstore.Store, *Bucket, *fakeShardController) {
	t.Helper()
	db := openSMTestStore(t)
	bucket := seedBucket(t, db, initialState)
	ab := &Bucket{ID: bucket.ID, Label: bucket.Label, Tier: "small", Target: 2, WindowDuration: time.Hour}
	clock := func() time.Time { return time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC) }
	ctrl := &fakeShardController{}
	sm := NewBucketSM(ab, db, ctrl, clock)
	return sm, db, ab, ctrl
}

// seedShardRow inserts a shard_records row for the bucket SM cascade
// tests. stripe is optional (nil for legacy/unstriped rows).
func seedShardRow(t *testing.T, db shardstore.Store, bucketID int64, id string, state ShardState, windowStart time.Time, stripe *int) {
	t.Helper()
	rec := &Shard{
		ShardRow: ShardRow{
			ID:          id,
			State:       state,
			BucketID:    bucketID,
			Tier:        "small",
			WindowStart: windowStart,
			WindowEnd:   windowStart.Add(time.Hour),
			CreatedAt:   windowStart,
			StripeIndex: stripe,
		},
	}
	if err := db.SaveShard(&rec.ShardRow); err != nil {
		t.Fatalf("seed shard row %s: %v", id, err)
	}
}

func TestBucketSM_StasisToWarmingOnRotationNeeded(t *testing.T) {
	sm, db, ab, _ := makeFixtureBucketSM(t, BucketStasis)

	if err := sm.Fire(context.Background(), TriggerRotationNeeded); err != nil {
		t.Fatalf("Fire RotationNeeded: %v", err)
	}
	got, err := sm.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if got != BucketWarming {
		t.Fatalf("state: got %q want WARMING", got)
	}
	// cascade_started_at must be set on WARMING entry
	b, _, _ := db.GetBucket(ab.ID)
	if b.CascadeStartedAt == nil {
		t.Fatalf("cascade_started_at: nil; expected set on WARMING")
	}
}

func TestBucketSM_FullCascade(t *testing.T) {
	sm, db, ab, _ := makeFixtureBucketSM(t, BucketStasis)
	ctx := context.Background()

	// Rotation-only cascade: STASIS → WARMING → ROTATING → STASIS. No
	// ARCHIVING phase — archival is a fire-and-forget retention sweep,
	// not a cascade step.
	if err := sm.Fire(ctx, TriggerRotationNeeded); err != nil {
		t.Fatalf("RotationNeeded: %v", err)
	}
	if err := sm.Fire(ctx, TriggerAllReplacementsReady); err != nil {
		t.Fatalf("AllReplacementsReady: %v", err)
	}
	if err := sm.Fire(ctx, TriggerOldShardsRotated); err != nil {
		t.Fatalf("OldShardsRotated: %v", err)
	}

	got, err := sm.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if got != BucketStasis {
		t.Fatalf("state: got %q want STASIS", got)
	}

	// cascade_started_at must be cleared on STASIS re-entry
	b, _, _ := db.GetBucket(ab.ID)
	if b.CascadeStartedAt != nil {
		t.Fatalf("cascade_started_at: %v; expected nil after cascade", b.CascadeStartedAt)
	}
}

func TestBucketSM_ScaleDownToStasis(t *testing.T) {
	sm, _, _, _ := makeFixtureBucketSM(t, BucketStasis)
	ctx := context.Background()

	if err := sm.Fire(ctx, TriggerScaleDownRequested); err != nil {
		t.Fatalf("ScaleDownRequested: %v", err)
	}
	if err := sm.Fire(ctx, TriggerSettled); err != nil {
		t.Fatalf("Settled: %v", err)
	}
	got, err := sm.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if got != BucketStasis {
		t.Fatalf("state: got %q want STASIS", got)
	}
}

func TestBucketSM_StasisRejectsCascadeAdvanceTriggers(t *testing.T) {
	// STASIS only accepts the cascade-start triggers; firing an
	// advance trigger from STASIS is a logic error and must error.
	for _, trigger := range []Trigger{
		TriggerAllReplacementsReady,
		TriggerOldShardsRotated,
		TriggerSettled,
	} {
		t.Run(string(trigger), func(t *testing.T) {
			sm, _, _, _ := makeFixtureBucketSM(t, BucketStasis)
			err := sm.Fire(context.Background(), trigger)
			if err == nil {
				t.Fatalf("expected %q to be rejected from STASIS", trigger)
			}
		})
	}
}

func TestBucketSM_HydratesStateFromStore(t *testing.T) {
	// Construct SM with a bucket row already in ROTATING.
	sm, _, _, _ := makeFixtureBucketSM(t, BucketRotating)
	got, err := sm.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if got != BucketRotating {
		t.Fatalf("state: got %q want ROTATING (hydrated from store)", got)
	}
	// And the appropriate next-step trigger fires.
	if err := sm.Fire(context.Background(), TriggerOldShardsRotated); err != nil {
		t.Fatalf("Fire OldShardsRotated after hydrate: %v", err)
	}
}

// ── Cascade body + Evaluate gate tests (Slice C step 1) ─────────

// Window anchors used by the cascade tests. curWin is the current window
// start for the fixture clock (1h windows truncate 12:00 → 12:00).
var (
	curWin  = time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	nextWin = curWin.Add(time.Hour)      // 13:00 — the rotation target
	oldWin  = curWin.Add(-3 * time.Hour) // 09:00 — past retention (>1h old end)
)

func ip(i int) *int { return &i }

func sortedSpecStripes(specs []MintSpec) []int {
	out := make([]int, len(specs))
	for i, s := range specs {
		out[i] = s.StripeIndex
	}
	sort.Ints(out)
	return out
}

// onEnterWarming on a full current window warms the NEXT window,
// inheriting the current cohort's stripe indices.
func TestBucketSM_WarmingMintsNextWindowOnRotation(t *testing.T) {
	sm, db, ab, ctrl := makeFixtureBucketSM(t, BucketStasis)
	seedShardRow(t, db, ab.ID, "s-cur-0", StateIngesting, curWin, ip(0))
	seedShardRow(t, db, ab.ID, "s-cur-1", StateIngesting, curWin, ip(1))

	if err := sm.Fire(context.Background(), TriggerRotationNeeded); err != nil {
		t.Fatalf("RotationNeeded: %v", err)
	}
	if len(ctrl.minted) != 2 {
		t.Fatalf("minted: got %d want 2 (%+v)", len(ctrl.minted), ctrl.minted)
	}
	for _, spec := range ctrl.minted {
		if !spec.WindowStart.Equal(nextWin) {
			t.Errorf("mint window: got %s want %s", spec.WindowStart, nextWin)
		}
	}
	if got := sortedSpecStripes(ctrl.minted); got[0] != 0 || got[1] != 1 {
		t.Errorf("inherited stripes: got %v want [0 1]", got)
	}
}

// onEnterWarming below Target warms the CURRENT window (fill / scale-up),
// minting the missing stripe indices.
func TestBucketSM_WarmingFillsCurrentWindowWhenUnderTarget(t *testing.T) {
	sm, _, _, ctrl := makeFixtureBucketSM(t, BucketStasis)

	if err := sm.Fire(context.Background(), TriggerScaleUpRequested); err != nil {
		t.Fatalf("ScaleUpRequested: %v", err)
	}
	if len(ctrl.minted) != 2 {
		t.Fatalf("minted: got %d want 2", len(ctrl.minted))
	}
	for _, spec := range ctrl.minted {
		if !spec.WindowStart.Equal(curWin) {
			t.Errorf("fill window: got %s want %s (current)", spec.WindowStart, curWin)
		}
	}
	if got := sortedSpecStripes(ctrl.minted); got[0] != 0 || got[1] != 1 {
		t.Errorf("fill stripes: got %v want [0 1]", got)
	}
}

// onEnterWarming skips stripe indices already covered by a live shard in
// the warming window — idempotent across a crash mid-mint.
func TestBucketSM_WarmingIsIdempotent(t *testing.T) {
	sm, db, ab, ctrl := makeFixtureBucketSM(t, BucketStasis)
	seedShardRow(t, db, ab.ID, "s-cur-0", StateIngesting, curWin, ip(0))
	seedShardRow(t, db, ab.ID, "s-cur-1", StateIngesting, curWin, ip(1))
	// Stripe 0 in the next window already minted (a prior, crashed pass).
	seedShardRow(t, db, ab.ID, "s-next-0", StateLaunching, nextWin, ip(0))

	if err := sm.Fire(context.Background(), TriggerRotationNeeded); err != nil {
		t.Fatalf("RotationNeeded: %v", err)
	}
	if len(ctrl.minted) != 1 {
		t.Fatalf("minted: got %d want 1 (stripe 0 already covered)", len(ctrl.minted))
	}
	if ctrl.minted[0].StripeIndex != 1 {
		t.Errorf("minted stripe: got %d want 1", ctrl.minted[0].StripeIndex)
	}
}

// Evaluate in WARMING advances to ROTATING once every incoming shard is
// INGESTING, and onEnterRotating fires RotateRequested on the outgoing
// cohort.
func TestBucketSM_WarmingGateAdvancesAndRotatesOutgoing(t *testing.T) {
	sm, db, ab, ctrl := makeFixtureBucketSM(t, BucketWarming)
	// Incoming (next window) all INGESTING — gate satisfied.
	seedShardRow(t, db, ab.ID, "s-next-0", StateIngesting, nextWin, ip(0))
	seedShardRow(t, db, ab.ID, "s-next-1", StateIngesting, nextWin, ip(1))
	// Outgoing (current window) still INGESTING — to be rotated.
	seedShardRow(t, db, ab.ID, "s-cur-0", StateIngesting, curWin, ip(0))
	seedShardRow(t, db, ab.ID, "s-cur-1", StateIngesting, curWin, ip(1))

	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got, _ := sm.State(context.Background()); got != BucketRotating {
		t.Fatalf("state: got %q want ROTATING", got)
	}
	rotated := ctrl.firedFor(TriggerRotateRequested)
	sort.Strings(rotated)
	if len(rotated) != 2 || rotated[0] != "s-cur-0" || rotated[1] != "s-cur-1" {
		t.Errorf("rotated outgoing: got %v want [s-cur-0 s-cur-1]", rotated)
	}
}

// Evaluate in WARMING holds while any incoming shard is still LAUNCHING.
func TestBucketSM_WarmingGateWaitsForLaunching(t *testing.T) {
	sm, db, ab, _ := makeFixtureBucketSM(t, BucketWarming)
	seedShardRow(t, db, ab.ID, "s-next-0", StateIngesting, nextWin, ip(0))
	seedShardRow(t, db, ab.ID, "s-next-1", StateLaunching, nextWin, ip(1))

	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got, _ := sm.State(context.Background()); got != BucketWarming {
		t.Fatalf("state: got %q want WARMING (incoming still launching)", got)
	}
}

// Regression (2026-06-06 ingest outage): a bucket stuck in WARMING whose
// entire minted cohort went GONE — e.g. every launch failed on a
// deregistered shard AMI — must re-mint on the next Evaluate. onEnterWarming
// only mints on state entry, so without a re-mint in evaluateWarming the
// bucket waits forever for a cohort that already died, and ingest stalls.
func TestBucketSM_WarmingRemintsWhenCohortAllGone(t *testing.T) {
	sm, db, ab, ctrl := makeFixtureBucketSM(t, BucketWarming)
	// No live shards: the WARMING mint died (GONE). An ARCHIVED shard from a
	// past window is present but must be ignored by the live-cohort check.
	seedShardRow(t, db, ab.ID, "s-old-0", StateArchived, oldWin, ip(0))

	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(ctrl.minted) != 2 {
		t.Fatalf("re-mint: got %d want 2 (WARMING must re-mint a dead cohort)", len(ctrl.minted))
	}
	for _, spec := range ctrl.minted {
		if !spec.WindowStart.Equal(curWin) {
			t.Errorf("re-mint window: got %s want %s (current)", spec.WindowStart, curWin)
		}
	}
	if got := sortedSpecStripes(ctrl.minted); got[0] != 0 || got[1] != 1 {
		t.Errorf("re-mint stripes: got %v want [0 1]", got)
	}
	if got, _ := sm.State(context.Background()); got != BucketWarming {
		t.Fatalf("state: got %q want WARMING (cohort not yet INGESTING)", got)
	}
}

// A WARMING cohort that's partially dead (one stripe INGESTING, the other
// GONE with nothing in flight) must re-mint only the missing stripe — it
// can never reach the AllReplacementsReady gate otherwise.
func TestBucketSM_WarmingRemintsMissingStripe(t *testing.T) {
	sm, db, ab, ctrl := makeFixtureBucketSM(t, BucketWarming)
	// Stripe 0 came up; stripe 1's launch died (GONE → absent from cohort).
	seedShardRow(t, db, ab.ID, "s-cur-0", StateIngesting, curWin, ip(0))

	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(ctrl.minted) != 1 {
		t.Fatalf("re-mint: got %d want 1 (only the dead stripe)", len(ctrl.minted))
	}
	if ctrl.minted[0].StripeIndex != 1 {
		t.Errorf("re-mint stripe: got %d want 1", ctrl.minted[0].StripeIndex)
	}
}

// A WARMING cohort with a launch still in flight must NOT re-mint — the
// re-mint guard only fires once nothing is launching or recovering.
func TestBucketSM_WarmingDoesNotRemintWhileLaunching(t *testing.T) {
	sm, db, ab, ctrl := makeFixtureBucketSM(t, BucketWarming)
	seedShardRow(t, db, ab.ID, "s-cur-0", StateIngesting, curWin, ip(0))
	seedShardRow(t, db, ab.ID, "s-cur-1", StateLaunching, curWin, ip(1))

	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(ctrl.minted) != 0 {
		t.Fatalf("re-mint: got %d want 0 (stripe 1 still launching)", len(ctrl.minted))
	}
}

// Evaluate in ROTATING returns to STASIS once the outgoing cohort has all
// left INGESTING.
func TestBucketSM_RotatingGateReturnsToStasisWhenDrained(t *testing.T) {
	sm, db, ab, _ := makeFixtureBucketSM(t, BucketRotating)
	seedShardRow(t, db, ab.ID, "s-next-0", StateIngesting, nextWin, ip(0))
	seedShardRow(t, db, ab.ID, "s-next-1", StateIngesting, nextWin, ip(1))
	// Outgoing already flipped to READ_ONLY.
	seedShardRow(t, db, ab.ID, "s-cur-0", StateReadOnly, curWin, ip(0))

	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got, _ := sm.State(context.Background()); got != BucketStasis {
		t.Fatalf("state: got %q want STASIS", got)
	}
}

// Evaluate in ROTATING holds while an outgoing shard is still INGESTING.
func TestBucketSM_RotatingWaitsForOutgoing(t *testing.T) {
	sm, db, ab, _ := makeFixtureBucketSM(t, BucketRotating)
	seedShardRow(t, db, ab.ID, "s-next-0", StateIngesting, nextWin, ip(0))
	seedShardRow(t, db, ab.ID, "s-cur-0", StateIngesting, curWin, ip(0)) // not yet rotated

	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got, _ := sm.State(context.Background()); got != BucketRotating {
		t.Fatalf("state: got %q want ROTATING (outgoing still INGESTING)", got)
	}
}

// Evaluate in STASIS over-target fires ScaleDownRequested and the entry
// rotates the excess.
func TestBucketSM_StasisFiresScaleDown(t *testing.T) {
	sm, db, ab, ctrl := makeFixtureBucketSM(t, BucketStasis) // Target 2
	seedShardRow(t, db, ab.ID, "s-0", StateIngesting, curWin, ip(0))
	seedShardRow(t, db, ab.ID, "s-1", StateIngesting, curWin, ip(1))
	seedShardRow(t, db, ab.ID, "s-2", StateIngesting, curWin, ip(2))

	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got, _ := sm.State(context.Background()); got != BucketScalingDown {
		t.Fatalf("state: got %q want SCALING_DOWN", got)
	}
	if rotated := ctrl.firedFor(TriggerRotateRequested); len(rotated) != 1 {
		t.Errorf("scale-down rotated: got %v want exactly 1 (excess)", rotated)
	}
}

// Evaluate in STASIS under-target fires ScaleUpRequested.
func TestBucketSM_StasisFiresScaleUp(t *testing.T) {
	sm, db, ab, _ := makeFixtureBucketSM(t, BucketStasis) // Target 2
	seedShardRow(t, db, ab.ID, "s-0", StateIngesting, curWin, ip(0))

	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got, _ := sm.State(context.Background()); got != BucketWarming {
		t.Fatalf("state: got %q want WARMING (scale-up)", got)
	}
}

// TestBucketSM_StasisDoesNotScaleUpWhileRecovering is the Bug 2
// regression: a SUSPENDED (HeartbeatStale path) or UPGRADING (operator-
// roll path) shard occupies a stripe slot via evaluateRecovery's
// reap-then-relaunch. STASIS must wait for that recovery to land in
// INGESTING (or escalate to GONE) before minting a replacement, or the
// resulting duplicate-stripe shard parks an EC2+EBS for a full
// retention window after scale-down rotates it back out.
func TestBucketSM_StasisDoesNotScaleUpWhileRecovering(t *testing.T) {
	for _, recState := range []ShardState{StateSuspended, StateUpgrading} {
		t.Run(string(recState), func(t *testing.T) {
			sm, db, ab, ctrl := makeFixtureBucketSM(t, BucketStasis) // Target 2
			seedShardRow(t, db, ab.ID, "s-ingest", StateIngesting, curWin, ip(0))
			seedShardRow(t, db, ab.ID, "s-recovery", recState, curWin, ip(1))

			if err := sm.Evaluate(context.Background()); err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if got, _ := sm.State(context.Background()); got != BucketStasis {
				t.Fatalf("state: got %q want STASIS (recovery in flight)", got)
			}
			if len(ctrl.fired) != 0 {
				t.Fatalf("triggers fired: got %v want []", ctrl.fired)
			}
			if len(ctrl.minted) != 0 {
				t.Fatalf("mints: got %d want 0 (no replacement while recovering)", len(ctrl.minted))
			}
		})
	}
}

// TestBucketSM_WarmingDoesNotMintDuplicateStripeForRecoveringShard
// asserts the Bug 2 fix at the stripe-set layer: a SUSPENDED/UPGRADING
// shard's stripe must count as covered by onEnterWarming, otherwise the
// fill branch mints a duplicate at the same stripe in the same window.
// We force WARMING entry via TriggerScaleUpRequested directly (bypassing
// the evaluateStasis gate that the previous test covers) so we can
// observe what onEnterWarming does given the cohort.
func TestBucketSM_WarmingDoesNotMintDuplicateStripeForRecoveringShard(t *testing.T) {
	for _, recState := range []ShardState{StateSuspended, StateUpgrading} {
		t.Run(string(recState), func(t *testing.T) {
			sm, db, ab, ctrl := makeFixtureBucketSM(t, BucketStasis) // Target 2
			seedShardRow(t, db, ab.ID, "s-ingest", StateIngesting, curWin, ip(0))
			seedShardRow(t, db, ab.ID, "s-recovery", recState, curWin, ip(1))

			if err := sm.Fire(context.Background(), TriggerScaleUpRequested); err != nil {
				t.Fatalf("ScaleUpRequested: %v", err)
			}
			if len(ctrl.minted) != 0 {
				t.Fatalf("minted: got %d want 0 (stripe 1 already covered by %s shard)", len(ctrl.minted), recState)
			}
		})
	}
}

// TestBucketSM_WarmingGateWaitsForRecoveryToResolve asserts the gate
// change in evaluateWarming: the WARMING→ROTATING transition waits for
// every in-flight recovery in the warming window to either resolve to
// INGESTING or escalate to GONE before firing TriggerAllReplacementsReady.
// The original gate compared `ingesting == total`, which broke as soon
// as SUSPENDED/UPGRADING joined the cohort (total counts them but
// ingesting doesn't, so the gate stuck forever).
func TestBucketSM_WarmingGateWaitsForRecoveryToResolve(t *testing.T) {
	for _, recState := range []ShardState{StateSuspended, StateUpgrading} {
		t.Run(string(recState), func(t *testing.T) {
			sm, db, ab, _ := makeFixtureBucketSM(t, BucketWarming) // Target 2
			// Warming cohort: 2 INGESTING (meeting Target) + 1 recovering
			// (the duplicate-stripe from a pre-fix-era mint, mid-rebind).
			seedShardRow(t, db, ab.ID, "s-warm-0", StateIngesting, nextWin, ip(0))
			seedShardRow(t, db, ab.ID, "s-warm-1", StateIngesting, nextWin, ip(1))
			seedShardRow(t, db, ab.ID, "s-recovering", recState, nextWin, ip(0))

			if err := sm.Evaluate(context.Background()); err != nil {
				t.Fatalf("Evaluate (recovering): %v", err)
			}
			if got, _ := sm.State(context.Background()); got != BucketWarming {
				t.Fatalf("state: got %q want WARMING (recovery in flight)", got)
			}

			// Resolve the recovery: flip the recovering shard to INGESTING.
			// Now the gate should fire on the next Evaluate.
			rec, _, _ := db.GetShard("s-recovering")
			rec.State = StateIngesting
			if err := db.SaveShard(rec); err != nil {
				t.Fatalf("flip recovery to INGESTING: %v", err)
			}

			if err := sm.Evaluate(context.Background()); err != nil {
				t.Fatalf("Evaluate (resolved): %v", err)
			}
			if got, _ := sm.State(context.Background()); got != BucketRotating {
				t.Fatalf("state after resolve: got %q want ROTATING", got)
			}
		})
	}
}

// Evaluate in SCALING_DOWN settles back to STASIS once INGESTING is at
// Target.
func TestBucketSM_ScalingDownGateSettles(t *testing.T) {
	sm, db, ab, _ := makeFixtureBucketSM(t, BucketScalingDown) // Target 2
	seedShardRow(t, db, ab.ID, "s-0", StateIngesting, curWin, ip(0))
	seedShardRow(t, db, ab.ID, "s-1", StateIngesting, curWin, ip(1))

	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got, _ := sm.State(context.Background()); got != BucketStasis {
		t.Fatalf("state: got %q want STASIS (settled)", got)
	}
}

// The retention sweep fires ArchiveRequested (fire-and-forget) on a
// READ_ONLY shard past retention, in any bucket state, without changing
// the bucket's own state.
func TestBucketSM_RetentionSweepFiresArchive(t *testing.T) {
	sm, db, ab, ctrl := makeFixtureBucketSM(t, BucketStasis) // Target 2, WindowDuration 1h
	// Two healthy INGESTING shards so STASIS itself stays put.
	seedShardRow(t, db, ab.ID, "s-cur-0", StateIngesting, curWin, ip(0))
	seedShardRow(t, db, ab.ID, "s-cur-1", StateIngesting, curWin, ip(1))
	// A READ_ONLY shard whose window ended >1h ago → past retention.
	seedShardRow(t, db, ab.ID, "s-old", StateReadOnly, oldWin, ip(0))

	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got, _ := sm.State(context.Background()); got != BucketStasis {
		t.Fatalf("state: got %q want STASIS (sweep never changes bucket state)", got)
	}
	archived := ctrl.firedFor(TriggerArchiveRequested)
	if len(archived) != 1 || archived[0] != "s-old" {
		t.Errorf("archived: got %v want [s-old]", archived)
	}
}
