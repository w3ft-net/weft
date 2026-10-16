package logdb

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/qmuntal/stateless"
	"gorm.io/gorm"

	"github.com/graybeardcorp/weft/internal/weftlog"
	"github.com/graybeardcorp/weft/store"
)

// BucketSM is the per-bucket finite state machine. Owns the
// cascade (STASIS→WARMING→ROTATING→ARCHIVING→STASIS) and the
// scale-down branch (STASIS→SCALING_DOWN→STASIS). Shard FSMs only
// advance through INGESTING→READ_ONLY→ARCHIVED when this SM tells
// them to (via TriggerRotateRequested / TriggerArchiveRequested) —
// see docs/plans/asg-as-api-stateless.md.
//
// The bucket SM is driven entirely by Evaluate() on the tick. No
// external Fire() call sites — every transition originates from
// Evaluate checking gates against observed shard state.
//
// Concurrency: stateless's FiringQueued mode serializes triggers
// per SM. Cross-bucket Evaluates can run in parallel.
type BucketSM struct {
	*Bucket // identity + config (ID, Label, Target, Tier, window methods); promoted

	sm         *stateless.StateMachine
	db         *gorm.DB         // persists the FSM's state column + cascade-set queries
	controller ShardController  // mints/drives child shard SMs; nil on dev/test paths that don't
	clock      func() time.Time // test seam; stamps cascade_started_at and drives window math

	mu sync.Mutex // serialises action-internal mutations
}

// ShardController is the bucket SM's handle into the provisioner's
// child-shard machinery. The bucket SM never touches a shard's SM
// directly: WARMING / scale-up mint fresh LAUNCHING shards via
// MintShard; ROTATING, scale-down, and the retention sweep drive
// existing shards via FireShard. The provisioner implements it (Slice
// C); bucket SM unit tests fake it.
type ShardController interface {
	// MintShard creates a new LAUNCHING shard for the bucket in the given
	// window at the given stripe index (the provisioner resolves the AZ
	// from the stripe index via the striper), registers its registry
	// entry (binding the shard's own SM), and returns the new shard id.
	MintShard(ctx context.Context, spec MintSpec) (string, error)
	// FireShard dispatches a trigger to an existing shard's SM, resolving
	// it by id. A missing SM is an error the caller logs (and otherwise
	// tolerates — a shard that vanished mid-cascade is self-healed by the
	// next tick's gate re-check).
	FireShard(ctx context.Context, shardID string, trigger Trigger, args ...any) error
}

// MintSpec is the bucket SM's request to bring up one new shard. AZ is
// deliberately absent — the bucket holds no AZ knowledge beyond the
// stripe index; the provisioner maps StripeIndex → AZ via the striper.
type MintSpec struct {
	BucketID    int64
	Tier        string
	WindowStart time.Time
	WindowEnd   time.Time
	StripeIndex int
}

// PrewarmLead is how long before an INGESTING shard's WindowEnd the
// bucket SM arms a rotation (evaluateStasis fires RotationNeeded so the
// next-window cohort is INGESTING before the current window closes,
// avoiding a write-gap). Package-scoped so main.go can override it at
// boot for per-env tuning; the default must comfortably exceed instance
// launch + configure time.
var PrewarmLead = 10 * time.Minute

// ErrUnknownBucket is the bucket-FSM counterpart to
// ErrUnknownShard. Returned by the state accessor when the
// underlying buckets row is gone.
var ErrUnknownBucket = errors.New("bucket record not found")

// NewBucketSM constructs a fresh SM bound to bucket. Like the shard
// SM, the initial state is read lazily from the store via the
// ExternalStorage accessor on the first transition.
func NewBucketSM(bucket *Bucket, db *gorm.DB, controller ShardController, clock func() time.Time) *BucketSM {
	if clock == nil {
		clock = time.Now
	}
	b := &BucketSM{
		Bucket:     bucket,
		db:         db,
		controller: controller,
		clock:      clock,
	}
	b.sm = stateless.NewStateMachineWithExternalStorage(
		b.readState, b.writeState, stateless.FiringQueued,
	)
	b.configure()
	return b
}

// State reads the current state via the external-storage accessor.
func (b *BucketSM) State(ctx context.Context) (BucketState, error) {
	st, err := b.sm.State(ctx)
	if err != nil {
		return "", err
	}
	if st == nil {
		return "", ErrUnknownBucket
	}
	return st.(BucketState), nil
}

// Fire dispatches a trigger. Almost always called from within
// Evaluate; exposed mostly for tests that need to drive specific
// transitions.
func (b *BucketSM) Fire(ctx context.Context, trigger Trigger, args ...any) error {
	return b.sm.FireCtx(ctx, trigger, args...)
}

func (b *BucketSM) readState(ctx context.Context) (stateless.State, error) {
	var rec store.Bucket
	err := b.db.Select("state").Where("id = ?", b.ID).First(&rec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrUnknownBucket
	}
	if err != nil {
		return nil, fmt.Errorf("bucketsm read %d: %w", b.ID, err)
	}
	if rec.State == "" {
		// Defensive: should never happen post-migration-056, but if a
		// row predates the migration somehow, treat as STASIS so the
		// FSM can converge on the next tick.
		return BucketStasis, nil
	}
	return BucketState(rec.State), nil
}

func (b *BucketSM) writeState(ctx context.Context, st stateless.State) error {
	newState := st.(BucketState)
	// Narrow write: only the state column. Lifecycle-owned columns
	// (apps, target, tier) are written through SaveBucket, so this
	// can't race a concurrent config writer over them.
	if err := b.db.Model(&store.Bucket{}).Where("id = ?", b.ID).
		Update("state", string(newState)).Error; err != nil {
		return fmt.Errorf("bucketsm write %d=%s: %w", b.ID, newState, err)
	}
	return nil
}

func (b *BucketSM) configure() {
	b.sm.Configure(BucketStasis).
		Permit(TriggerRotationNeeded, BucketWarming).
		Permit(TriggerScaleUpRequested, BucketWarming).
		Permit(TriggerScaleDownRequested, BucketScalingDown)

	b.sm.Configure(BucketWarming).
		OnEntry(b.onEnterWarming).
		Permit(TriggerAllReplacementsReady, BucketRotating)

	// ROTATING returns straight to STASIS — there's no ARCHIVING phase.
	// Archival of the now-expired READ_ONLY cohort is fired as a
	// fire-and-forget retention sweep from Evaluate, not gated here.
	b.sm.Configure(BucketRotating).
		OnEntry(b.onEnterRotating).
		Permit(TriggerOldShardsRotated, BucketStasis)

	b.sm.Configure(BucketScalingDown).
		OnEntry(b.onEnterScalingDown).
		Permit(TriggerSettled, BucketStasis)

	// STASIS.OnEntry clears cascade_started_at. Wired separately so
	// the initial state-read (before any transition) doesn't
	// double-fire it.
	b.sm.Configure(BucketStasis).OnEntry(b.onEnterStasis)
}

// ── OnEntry actions ─────────────────────────────────────────────

// onEnterWarming stamps cascade_started_at and mints the warming
// cohort. Two shapes, distinguished by whether the current window
// already holds Target INGESTING shards:
//
//   - Rotation (current window is full): warm the NEXT window, minting
//     one incoming shard per current-cohort stripe index so the next
//     window mirrors the current AZ layout.
//   - Fill / scale-up (current window under Target): warm the CURRENT
//     window, minting the stripe indices 0..Target-1 not yet covered.
//
// Idempotent: a re-entry (or a tick that re-derives the same plan)
// skips any stripe index already covered by a live shard in the warming
// window, so a crash between mints doesn't double-launch.
func (b *BucketSM) onEnterWarming(ctx context.Context, args ...any) error {
	now := b.clock().UTC()
	if err := b.db.Model(&store.Bucket{}).Where("id = ?", b.ID).
		Update("cascade_started_at", &now).Error; err != nil {
		return fmt.Errorf("bucketsm %d: set cascade_started_at: %w", b.ID, err)
	}
	return b.mintWarmingCohort(ctx, now)
}

// mintWarmingCohort brings the warming window up to Target by minting the
// stripe indices not already covered by a live shard. It is the WARMING
// cascade's capacity primitive: run once on state entry (onEnterWarming)
// and again from evaluateWarming whenever the cohort is deficient with no
// launch in flight — so a cohort that died wholesale (e.g. every launch
// failed on a deregistered AMI and went GONE) is re-minted rather than
// deadlocking the bucket. Idempotent: covered stripes are skipped, so
// repeated calls converge to exactly Target shards.
func (b *BucketSM) mintWarmingCohort(ctx context.Context, now time.Time) error {
	if b.controller == nil || b.Target <= 0 {
		weftlog.Infof(ctx, "bucketsm %d (%s): WARMING — no controller or target<=0, nothing to mint", b.ID, b.Label)
		return nil
	}

	curStart, curEnd := b.CurrentWindow(now)
	nextStart, nextEnd := b.NextWindow(now)
	live, err := b.shardsInStates(liveCohort...)
	if err != nil {
		return fmt.Errorf("bucketsm %d: warming cohort query: %w", b.ID, err)
	}

	var warmStart, warmEnd time.Time
	var stripes []int
	if b.countServingIn(live, curStart) >= b.Target {
		// Rotation — warm the next window, inheriting the current cohort's
		// stripe indices.
		warmStart, warmEnd = nextStart, nextEnd
		covered := stripeSet(live, warmStart)
		fallback := 0
		for _, r := range live {
			if !ingestingLike(r.State) || !windowStartEq(r, curStart) {
				continue
			}
			si, ok := stripeOf(r)
			if !ok {
				si = fallback
			}
			fallback++
			if !covered[si] {
				covered[si] = true
				stripes = append(stripes, si)
			}
		}
	} else {
		// Fill / scale-up — warm the current window up to Target.
		warmStart, warmEnd = curStart, curEnd
		covered := stripeSet(live, warmStart)
		for i := 0; i < b.Target; i++ {
			if !covered[i] {
				stripes = append(stripes, i)
			}
		}
	}

	for _, si := range stripes {
		id, err := b.controller.MintShard(ctx, MintSpec{
			BucketID:    b.ID,
			Tier:        b.Tier,
			WindowStart: warmStart,
			WindowEnd:   warmEnd,
			StripeIndex: si,
		})
		if err != nil {
			weftlog.Infof(ctx, "bucketsm %d (%s): mint stripe %d: %v", b.ID, b.Label, si, err)
			continue
		}
		weftlog.Infof(ctx, "bucketsm %d (%s): WARMING minted %s (stripe=%d window %s–%s)",
			b.ID, b.Label, id, si, warmStart.Format(time.RFC3339), warmEnd.Format(time.RFC3339))
	}
	return nil
}

// onEnterRotating fires RotateRequested on every outgoing shard — the
// INGESTING shards in windows older than the newest (warming) window.
// The incoming cohort is INGESTING by now (the WARMING→ROTATING gate
// guaranteed it), so 2·Target shards are hot when this drains the old
// ones; the bucket never gaps the write path.
func (b *BucketSM) onEnterRotating(ctx context.Context, args ...any) error {
	if b.controller == nil {
		weftlog.Infof(ctx, "bucketsm %d (%s): enter ROTATING — no controller, nothing to rotate", b.ID, b.Label)
		return nil
	}
	ing, err := b.shardsInStates(servingCohort...)
	if err != nil {
		return fmt.Errorf("bucketsm %d: rotating cohort query: %w", b.ID, err)
	}
	warmStart, ok := latestWindowStart(ing)
	if !ok {
		return nil
	}
	for _, r := range ing {
		if windowStartBefore(r, warmStart) {
			if err := b.controller.FireShard(ctx, r.ID, TriggerRotateRequested); err != nil {
				weftlog.Infof(ctx, "bucketsm %d (%s): rotate %s: %v", b.ID, b.Label, r.ID, err)
			}
		}
	}
	return nil
}

// onEnterScalingDown fires RotateRequested on the N youngest INGESTING
// shards (N = current − target). They flow through the normal
// READ_ONLY → ARCHIVING → ARCHIVED path on subsequent ticks; the
// retention sweep handles their eventual archival. Youngest-first so
// the oldest shards (closest to their own rotation anyway) stay hot.
func (b *BucketSM) onEnterScalingDown(ctx context.Context, args ...any) error {
	if b.controller == nil {
		weftlog.Infof(ctx, "bucketsm %d (%s): enter SCALING_DOWN — no controller, nothing to drain", b.ID, b.Label)
		return nil
	}
	ing, err := b.shardsInStates(servingCohort...)
	if err != nil {
		return fmt.Errorf("bucketsm %d: scaling-down cohort query: %w", b.ID, err)
	}
	excess := len(ing) - b.Target
	if excess <= 0 {
		return nil
	}
	// Youngest first: latest window_start, then latest created_at.
	sort.SliceStable(ing, func(i, j int) bool {
		if !ing[i].WindowStart.Equal(ing[j].WindowStart) {
			return ing[i].WindowStart.After(ing[j].WindowStart)
		}
		return ing[i].CreatedAt.After(ing[j].CreatedAt)
	})
	for _, r := range ing[:excess] {
		if err := b.controller.FireShard(ctx, r.ID, TriggerRotateRequested); err != nil {
			weftlog.Infof(ctx, "bucketsm %d (%s): scale-down rotate %s: %v", b.ID, b.Label, r.ID, err)
		}
	}
	return nil
}

func (b *BucketSM) onEnterStasis(ctx context.Context, args ...any) error {
	// Clear cascade_started_at (NULL = no cascade in flight). A typed
	// nil *time.Time binds as SQL NULL, keeping the column nullable
	// rather than zero-valued.
	if err := b.db.Model(&store.Bucket{}).Where("id = ?", b.ID).
		Update("cascade_started_at", (*time.Time)(nil)).Error; err != nil {
		return fmt.Errorf("bucketsm %d: clear cascade_started_at: %w", b.ID, err)
	}
	return nil
}

// ── Evaluate — the convergence driver ───────────────────────────
//
// Evaluate runs once per reconciler tick per bucket. It reads the
// persisted state, observes the bucket's shard cohorts (via inline
// gorm queries — the cascade context is 3NF, derived from shard_records
// rather than stored as a list), and fires whichever advancing trigger
// the gate permits. Like the shard SM, all convergence lives here, so a
// restart that drops straight into Evaluate re-checks the gate and
// either advances or waits — there is no boot-vs-running mode.
//
// The retention sweep runs first in every state: it fires
// ArchiveRequested (fire-and-forget) on READ_ONLY shards past retention,
// so a slow S3 export can never block the rotation cascade.
func (b *BucketSM) Evaluate(ctx context.Context) error {
	st, err := b.State(ctx)
	if err != nil {
		return err
	}
	now := b.clock().UTC()

	if err := b.sweepRetention(ctx, now); err != nil {
		weftlog.Infof(ctx, "bucketsm %d (%s): retention sweep: %v", b.ID, b.Label, err)
	}

	switch st {
	case BucketStasis:
		return b.evaluateStasis(ctx, now)
	case BucketWarming:
		return b.evaluateWarming(ctx)
	case BucketRotating:
		return b.evaluateRotating(ctx)
	case BucketScalingDown:
		return b.evaluateScalingDown(ctx)
	default:
		return nil
	}
}

// evaluateStasis decides whether to start a cascade. Priority:
// scale-down (target dropped) > rotation (window approaching) >
// scale-up (under target). Only one trigger fires per tick; the next
// tick re-evaluates from the new state.
func (b *BucketSM) evaluateStasis(ctx context.Context, now time.Time) error {
	if b.Target <= 0 {
		return nil // no ingest capacity required — nothing to manage
	}
	live, err := b.shardsInStates(liveCohort...)
	if err != nil {
		return fmt.Errorf("bucketsm %d: stasis cohort query: %w", b.ID, err)
	}
	curStart, _ := b.CurrentWindow(now)
	nextStart, _ := b.NextWindow(now)

	ingesting := 0
	launchingCur := 0
	recoveringCur := 0
	rotationDue := false
	hasNextWindow := false
	for _, r := range live {
		if windowStartEq(r, nextStart) {
			hasNextWindow = true
		}
		switch r.State {
		case StateIngesting, StateDegraded:
			ingesting++
			if end, ok := windowEnd(r); ok && !now.Add(PrewarmLead).Before(end) {
				rotationDue = true // within PrewarmLead of (or past) WindowEnd
			}
		case StateLaunching:
			if windowStartEq(r, curStart) {
				launchingCur++
			}
		case StateSuspended, StateUpgrading:
			// In-flight recovery (reap-then-relaunch via evaluateRecovery).
			// Holds a stripe slot; the bucket must wait for it to either
			// land back in INGESTING or escalate to GONE before scaling up,
			// otherwise we mint a duplicate-stripe replacement that becomes
			// orphaned READ_ONLY when the original recovers.
			if windowStartEq(r, curStart) {
				recoveringCur++
			}
		}
	}

	switch {
	case ingesting > b.Target:
		return b.Fire(ctx, TriggerScaleDownRequested)
	case rotationDue && !hasNextWindow:
		// Don't re-arm a cascade whose next-window cohort already exists.
		return b.Fire(ctx, TriggerRotationNeeded)
	case ingesting < b.Target && launchingCur == 0 && recoveringCur == 0:
		return b.Fire(ctx, TriggerScaleUpRequested)
	}
	return nil
}

// evaluateWarming gates WARMING → ROTATING: every incoming shard (the
// newest window) must be INGESTING and the cohort must be at least
// Target strong, so rotation runs at 2·Target hot capacity.
//
// TODO(window-extension): the plan's WARMING.OnTick policy — extend the
// outgoing shards' WindowEnd when cascade_started_at is older than a
// stall threshold and incoming shards are still LAUNCHING, to avoid a
// write-gap — lands with its config (CascadeStallThreshold /
// ExtensionStep / MaxExtensions), which doesn't exist yet.
func (b *BucketSM) evaluateWarming(ctx context.Context) error {
	now := b.clock().UTC()
	live, err := b.shardsInStates(liveCohort...)
	if err != nil {
		return fmt.Errorf("bucketsm %d: warming gate query: %w", b.ID, err)
	}
	// The warming window is the newest window the mint targeted. With no
	// live shards at all — the entire mint died (GONE) before any shard
	// reached a live state — fall back to the current window so the
	// re-mint guard below refills it instead of waiting forever.
	warmStart, ok := latestWindowStart(live)
	if !ok {
		warmStart, _ = b.CurrentWindow(now)
	}
	total, ingesting, recovering, launching := 0, 0, 0, 0
	for _, r := range live {
		if !windowStartEq(r, warmStart) {
			continue
		}
		total++
		switch {
		case ingestingLike(r.State):
			ingesting++
		case recoveringLike(r.State):
			recovering++
		case r.State == StateLaunching:
			launching++
		}
	}
	// Gate the WARMING→ROTATING transition on: the warming cohort has
	// reached Target serving capacity AND no in-flight launches or
	// recoveries remain. The original equality check (ingesting==total)
	// broke as soon as a SUSPENDED/UPGRADING shard joined the cohort:
	// total counts it but ingesting doesn't, so the gate would stick.
	if total > 0 && ingesting >= b.Target && recovering == 0 && launching == 0 {
		return b.Fire(ctx, TriggerAllReplacementsReady)
	}
	// Re-mint guard: nothing is in flight (no LAUNCHING/recovering) yet the
	// cohort is still under Target — the previous mint died (e.g. every
	// launch failed on a deregistered AMI and the shards went GONE).
	// onEnterWarming only mints on state entry, so without this the bucket
	// waits forever for a cohort that will never arrive and ingest stalls.
	// mintWarmingCohort is idempotent (skips covered stripes), so this
	// safely re-arms the mint without double-launching live stripes.
	if ingesting < b.Target && launching == 0 && recovering == 0 {
		return b.mintWarmingCohort(ctx, now)
	}
	return nil
}

// evaluateRotating gates ROTATING → STASIS: no INGESTING shard remains
// in a window older than the newest. onEnterRotating fired
// RotateRequested at entry; this waits for each to land in READ_ONLY.
func (b *BucketSM) evaluateRotating(ctx context.Context) error {
	ing, err := b.shardsInStates(servingCohort...)
	if err != nil {
		return fmt.Errorf("bucketsm %d: rotating gate query: %w", b.ID, err)
	}
	warmStart, ok := latestWindowStart(ing)
	if !ok {
		return b.Fire(ctx, TriggerOldShardsRotated) // nothing INGESTING at all
	}
	for _, r := range ing {
		if windowStartBefore(r, warmStart) {
			return nil // an outgoing shard hasn't rotated yet
		}
	}
	return b.Fire(ctx, TriggerOldShardsRotated)
}

// evaluateScalingDown gates SCALING_DOWN → STASIS: the INGESTING count
// is back down to Target. onEnterScalingDown rotated the excess; this
// waits for those flips to land.
func (b *BucketSM) evaluateScalingDown(ctx context.Context) error {
	ing, err := b.shardsInStates(servingCohort...)
	if err != nil {
		return fmt.Errorf("bucketsm %d: scaling-down gate query: %w", b.ID, err)
	}
	if len(ing) <= b.Target {
		return b.Fire(ctx, TriggerSettled)
	}
	return nil
}

// sweepRetention fires ArchiveRequested on every READ_ONLY shard whose
// WindowEnd is older than the bucket's retention (= WindowDuration).
// Fire-and-forget: the shard's own ARCHIVING Evaluate does the export;
// the bucket never waits. A zero WindowDuration disables archival.
func (b *BucketSM) sweepRetention(ctx context.Context, now time.Time) error {
	if b.controller == nil || b.WindowDuration <= 0 {
		return nil
	}
	ro, err := b.shardsInStates(StateReadOnly)
	if err != nil {
		return err
	}
	for _, r := range ro {
		end, ok := windowEnd(r)
		if !ok || now.Sub(end) <= b.WindowDuration {
			continue
		}
		if err := b.controller.FireShard(ctx, r.ID, TriggerArchiveRequested); err != nil {
			weftlog.Infof(ctx, "bucketsm %d (%s): archive %s: %v", b.ID, b.Label, r.ID, err)
		}
	}
	return nil
}

// ── cohort query + window helpers ───────────────────────────────

// shardsInStates returns the bucket's shard rows in any of the given
// states. The bucket SM's only data source for cascade context — the
// incoming/outgoing/retention cohorts are all derived from these rows
// (the "3NF, not JSONB" decision in the plan).
func (b *BucketSM) shardsInStates(states ...ShardState) ([]*Shard, error) {
	ss := make([]string, len(states))
	for i, s := range states {
		ss[i] = string(s)
	}
	var shards []*Shard
	if err := b.db.Where("bucket_id = ? AND state IN ?", b.ID, ss).Find(&shards).Error; err != nil {
		return nil, err
	}
	return shards, nil
}

// liveCohort / servingCohort name the state sets the bucket gates query
// for. A shard is a live cohort member while warming up (LAUNCHING),
// serving (INGESTING, or its DEGRADED write-pause — still a cohort
// member, just out of write routing), or recovering (SUSPENDED via
// HeartbeatStale / UPGRADING via operator roll — neither is serving but
// both are recovering to INGESTING via evaluateRecovery, so they hold a
// stripe slot and must be counted so the bucket doesn't mint a
// duplicate-stripe replacement during the in-flight reap-and-relaunch).
// servingCohort drops LAUNCHING and the recovery states. Counting
// DEGRADED here is what keeps the ingest circuit breaker from churning a
// replacement during its cooldown window.
var (
	liveCohort    = []ShardState{StateLaunching, StateIngesting, StateDegraded, StateSuspended, StateUpgrading}
	servingCohort = []ShardState{StateIngesting, StateDegraded}
)

// recoveringLike reports whether a state is mid-recovery — SUSPENDED
// (HeartbeatStale path) or UPGRADING (operator-roll path). Both go
// through evaluateRecovery's reap-then-relaunch and should be counted
// by bucket gates as in-flight cohort members.
func recoveringLike(s ShardState) bool {
	return s == StateSuspended || s == StateUpgrading
}

// ingestingLike reports whether a state is a serving-cohort member —
// INGESTING or its DEGRADED breaker pause. The per-shard predicate the
// gates use where they previously compared == StateIngesting.
func ingestingLike(s ShardState) bool {
	return s == StateIngesting || s == StateDegraded
}

// countServingIn counts serving-cohort rows (INGESTING or DEGRADED) whose
// window_start equals start.
func (b *BucketSM) countServingIn(shards []*Shard, start time.Time) int {
	n := 0
	for _, r := range shards {
		if ingestingLike(r.State) && windowStartEq(r, start) {
			n++
		}
	}
	return n
}

// stripeSet returns the set of stripe indices already covered by live
// shards in the given window — used to make minting idempotent.
func stripeSet(shards []*Shard, window time.Time) map[int]bool {
	set := map[int]bool{}
	for _, r := range shards {
		if !windowStartEq(r, window) {
			continue
		}
		if si, ok := stripeOf(r); ok {
			set[si] = true
		}
	}
	return set
}

// latestWindowStart returns the newest window_start across shards.
func latestWindowStart(shards []*Shard) (time.Time, bool) {
	var max time.Time
	found := false
	for _, r := range shards {
		if r.WindowStart.IsZero() {
			continue
		}
		if !found || r.WindowStart.After(max) {
			max, found = r.WindowStart, true
		}
	}
	return max, found
}

func stripeOf(r *Shard) (int, bool) {
	if r.StripeIndex == nil {
		return 0, false
	}
	return *r.StripeIndex, true
}

func windowStartEq(r *Shard, t time.Time) bool {
	return r.WindowStart.Equal(t)
}

func windowStartBefore(r *Shard, t time.Time) bool {
	return r.WindowStart.Before(t)
}

func windowEnd(r *Shard) (time.Time, bool) {
	return r.WindowEnd, !r.WindowEnd.IsZero()
}
