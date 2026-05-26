package logdb

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/qmuntal/stateless"
	"gorm.io/gorm"

	"github.com/kevlarcon/weft/store"
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

	sm    *stateless.StateMachine
	db    *gorm.DB         // persists the FSM's state column (external storage)
	clock func() time.Time // test seam; stamps cascade_started_at

	mu sync.Mutex // serialises action-internal mutations
}

// ErrUnknownBucket is the bucket-FSM counterpart to
// ErrUnknownShard. Returned by the state accessor when the
// underlying buckets row is gone.
var ErrUnknownBucket = errors.New("bucket record not found")

// NewBucketSM constructs a fresh SM bound to bucket. Like the shard
// SM, the initial state is read lazily from the store via the
// ExternalStorage accessor on the first transition.
func NewBucketSM(bucket *Bucket, db *gorm.DB, clock func() time.Time) *BucketSM {
	if clock == nil {
		clock = time.Now
	}
	b := &BucketSM{
		Bucket: bucket,
		db:     db,
		clock:  clock,
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

// ── Action stubs — Phase 2 wires the real bodies ────────────────

func (b *BucketSM) onEnterWarming(ctx context.Context, args ...any) error {
	now := b.clock().UTC()
	if err := b.db.Model(&store.Bucket{}).Where("id = ?", b.ID).
		Update("cascade_started_at", &now).Error; err != nil {
		return fmt.Errorf("bucketsm %d: set cascade_started_at: %w", b.ID, err)
	}
	log.Printf("bucketsm %d (%s): enter WARMING (stub — Phase 2 mints incoming shard SMs)",
		b.ID, b.Label)
	return nil
}

func (b *BucketSM) onEnterRotating(ctx context.Context, args ...any) error {
	log.Printf("bucketsm %d (%s): enter ROTATING (stub — Phase 2 fires RotateRequested on outgoing shards)",
		b.ID, b.Label)
	return nil
}

func (b *BucketSM) onEnterScalingDown(ctx context.Context, args ...any) error {
	log.Printf("bucketsm %d (%s): enter SCALING_DOWN (stub — Phase 2 picks excess shards to rotate)",
		b.ID, b.Label)
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
