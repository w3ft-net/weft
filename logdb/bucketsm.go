package logdb

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/qmuntal/stateless"

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
	id     int64
	bucket *AppBucket
	deps   BucketSMDeps
	sm     *stateless.StateMachine

	mu sync.Mutex // serialises action-internal mutations
}

// BucketSMDeps mirrors ShardSMDeps in shape — narrow on purpose so
// test fixtures can mock individual pieces. The shard-SM factory is
// passed in so WARMING.OnEntry can mint child SMs without circular
// imports.
type BucketSMDeps struct {
	Store          BucketSMStore
	Buckets        Buckets // for refreshes (Apps list, target, tier)
	NewShardSM     func(entry *ShardEntry) *ShardSM
	Clock          func() time.Time
	// CascadeStallThreshold is how long WARMING can run before
	// OnTick starts extending outgoing-shard WindowEnds. Zero
	// defaults to PrewarmLead.
	CascadeStallThreshold time.Duration
	// ExtensionStep is the per-extension window-end push when the
	// cascade stalls. Zero defaults to PrewarmLead/2.
	ExtensionStep time.Duration
	// MaxExtensions caps WindowEnd extensions before WARMING gives
	// up and proceeds with whatever incoming shards are ready.
	MaxExtensions int
}

// BucketSMStore is the subset of *store.Store the bucket FSM
// mutates. The shard-lookup methods are how Evaluate computes
// incoming/outgoing.
type BucketSMStore interface {
	GetBucket(id int64) (store.Bucket, bool, error)
	UpdateBucketState(id int64, state string) error
	UpdateCascadeStartedAt(id int64, t *time.Time) error
	ShardsInBucketWithWindow(bucketID int64, windowStart time.Time, states []string) ([]store.ShardRecord, error)
	ShardsInBucketBeforeWindow(bucketID int64, windowStart time.Time, states []string) ([]store.ShardRecord, error)
}

// ErrUnknownBucket is the bucket-FSM counterpart to
// ErrUnknownShard. Returned by the state accessor when the
// underlying buckets row is gone.
var ErrUnknownBucket = errors.New("bucket record not found")

// NewBucketSM constructs a fresh SM bound to bucket. Like the shard
// SM, the initial state is read lazily from the store via the
// ExternalStorage accessor on the first transition.
func NewBucketSM(bucket *AppBucket, deps BucketSMDeps) *BucketSM {
	if deps.Clock == nil {
		deps.Clock = time.Now
	}
	if deps.CascadeStallThreshold == 0 {
		deps.CascadeStallThreshold = PrewarmLead
	}
	if deps.ExtensionStep == 0 {
		deps.ExtensionStep = PrewarmLead / 2
	}
	if deps.MaxExtensions == 0 {
		deps.MaxExtensions = 3
	}
	b := &BucketSM{
		id:     bucket.ID,
		bucket: bucket,
		deps:   deps,
	}
	b.sm = stateless.NewStateMachineWithExternalStorage(
		b.readState, b.writeState, stateless.FiringQueued,
	)
	b.configure()
	return b
}

// ID returns the bucket's surrogate id.
func (b *BucketSM) ID() int64 { return b.id }

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
	rec, ok, err := b.deps.Store.GetBucket(b.id)
	if err != nil {
		return nil, fmt.Errorf("bucketsm read %d: %w", b.id, err)
	}
	if !ok {
		return nil, ErrUnknownBucket
	}
	if rec.State == "" {
		// Defensive: should never happen post-migration-052, but if a
		// row predates the migration somehow, treat as STASIS so the
		// FSM can converge on the next tick.
		return BucketStasis, nil
	}
	return BucketState(rec.State), nil
}

func (b *BucketSM) writeState(ctx context.Context, st stateless.State) error {
	newState := st.(BucketState)
	if err := b.deps.Store.UpdateBucketState(b.id, string(newState)); err != nil {
		return fmt.Errorf("bucketsm write %d=%s: %w", b.id, newState, err)
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

	b.sm.Configure(BucketRotating).
		OnEntry(b.onEnterRotating).
		Permit(TriggerOldShardsRotated, BucketArchiving)

	b.sm.Configure(BucketArchiving).
		OnEntry(b.onEnterArchiving).
		Permit(TriggerArchivalsComplete, BucketStasis)

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
	now := b.deps.Clock().UTC()
	if err := b.deps.Store.UpdateCascadeStartedAt(b.id, &now); err != nil {
		return fmt.Errorf("bucketsm %d: set cascade_started_at: %w", b.id, err)
	}
	log.Printf("bucketsm %d (%s): enter WARMING (stub — Phase 2 mints incoming shard SMs)",
		b.id, b.bucket.Label)
	return nil
}

func (b *BucketSM) onEnterRotating(ctx context.Context, args ...any) error {
	log.Printf("bucketsm %d (%s): enter ROTATING (stub — Phase 2 fires RotateRequested on outgoing shards)",
		b.id, b.bucket.Label)
	return nil
}

func (b *BucketSM) onEnterArchiving(ctx context.Context, args ...any) error {
	log.Printf("bucketsm %d (%s): enter ARCHIVING (stub — Phase 2 fires ArchiveRequested on outgoing shards)",
		b.id, b.bucket.Label)
	return nil
}

func (b *BucketSM) onEnterScalingDown(ctx context.Context, args ...any) error {
	log.Printf("bucketsm %d (%s): enter SCALING_DOWN (stub — Phase 2 picks excess shards to rotate)",
		b.id, b.bucket.Label)
	return nil
}

func (b *BucketSM) onEnterStasis(ctx context.Context, args ...any) error {
	if err := b.deps.Store.UpdateCascadeStartedAt(b.id, nil); err != nil {
		return fmt.Errorf("bucketsm %d: clear cascade_started_at: %w", b.id, err)
	}
	return nil
}
