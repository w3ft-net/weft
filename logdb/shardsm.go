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

// ShardSM is the per-shard finite state machine. Wraps a
// stateless.StateMachine whose external storage reads/writes
// shard_records.state via the store, and holds the ShardEntry the
// FSM's actions mutate. One per registered shard; the registry owns
// the SM<→entry pairing.
//
// All state transitions flow through Fire(). Action methods
// (onEnterLaunching, configureOnHeartbeat, …) are the only place
// side effects happen — registry mutation, gRPC dials, AWS calls.
// Guards (heartbeatMatchesLaunching, hasVolume, …) are pure
// predicates over the entry.
//
// Concurrency: stateless's FiringQueued mode serializes triggers on
// this SM. The provisioner's per-shard heartbeat dispatch and the
// reconciler's tick can both Fire concurrently without racing the
// transition machinery. Action methods that touch the registry must
// still respect the registry's mutex.
type ShardSM struct {
	id    string
	entry *ShardEntry
	deps  ShardSMDeps
	sm    *stateless.StateMachine

	mu sync.Mutex // serialises action-internal mutations of entry
}

// ShardSMDeps bundles the collaborators the action methods need.
// Kept narrow so test fixtures can mock individual fields rather
// than constructing a full Provisioner.
type ShardSMDeps struct {
	Store    *store.Store
	Registry *ShardRegistry
	Clock    func() time.Time
	// StaleThreshold mirrors heartbeatStaleThreshold; carried in deps
	// so tests can shrink it without touching the package-level const.
	StaleThreshold time.Duration
}

// ErrUnknownShard is returned by the state accessor when the
// underlying shard_records row has been deleted out from under the
// SM (typically by GONE.OnEntry on another SM, or admin delete).
// The SM treats this as terminal — there is nothing left to do.
var ErrUnknownShard = errors.New("shard record not found")

// NewShardSM constructs a fresh SM bound to entry. The FSM's
// initial state is read lazily from the store on the first Fire
// (via the ExternalStorage accessor); callers should not assume
// the SM has a state until the first trigger lands.
//
// The returned SM is ready to Fire. Callers responsible for
// initial-state seeding (a newly minted shard) Fire(TriggerLaunch)
// immediately; callers hydrating from an existing row let the
// accessor pick up the persisted state on first transition.
func NewShardSM(entry *ShardEntry, deps ShardSMDeps) *ShardSM {
	if deps.Clock == nil {
		deps.Clock = time.Now
	}
	if deps.StaleThreshold == 0 {
		deps.StaleThreshold = heartbeatStaleThreshold
	}
	s := &ShardSM{
		id:    entry.ID,
		entry: entry,
		deps:  deps,
	}
	s.sm = stateless.NewStateMachineWithExternalStorage(
		s.readState, s.writeState, stateless.FiringQueued,
	)
	s.configure()
	return s
}

// ID returns the shard id. Convenience for log lines and registry
// keys; the SM doesn't expose the underlying entry.
func (s *ShardSM) ID() string { return s.id }

// State returns the SM's currently-believed state. Reads via the
// external-storage accessor, which goes to the DB; not for hot
// paths (use entry.State for in-memory inspection).
func (s *ShardSM) State(ctx context.Context) (ShardState, error) {
	st, err := s.sm.State(ctx)
	if err != nil {
		return "", err
	}
	if st == nil {
		return "", ErrUnknownShard
	}
	return st.(ShardState), nil
}

// Fire dispatches a trigger. Wraps the stateless API so call sites
// don't have to import that package or pass context for the common
// case. Errors include guard-not-satisfied, unknown-trigger, and
// action-method failures.
func (s *ShardSM) Fire(ctx context.Context, trigger Trigger, args ...any) error {
	return s.sm.FireCtx(ctx, trigger, args...)
}

// readState is the stateless ExternalStorage accessor. Looks up the
// persisted state column. A missing row returns ErrUnknownShard,
// which the SM treats as terminal.
func (s *ShardSM) readState(ctx context.Context) (stateless.State, error) {
	rec, ok, err := s.deps.Store.GetShard(s.id)
	if err != nil {
		return nil, fmt.Errorf("shardsm read %s: %w", s.id, err)
	}
	if !ok {
		return StateGone, nil // treated as terminal; transitions out are ignored
	}
	return ShardState(rec.State), nil
}

// writeState is the stateless ExternalStorage mutator. Writes the
// new state to shard_records and mirrors it onto entry.State so
// non-FSM readers (the registry's MatchingEntries walk, dashboards)
// see the same value without another round-trip.
func (s *ShardSM) writeState(ctx context.Context, st stateless.State) error {
	newState := st.(ShardState)
	// Narrow write: only the state column. Lifecycle-owned fields
	// (window bounds, apps, addr) move through SaveShard, so this
	// transition can't trample a concurrent SaveShard over them.
	if err := s.deps.Store.DB().Model(&store.ShardRecord{}).Where("id = ?", s.id).
		Update("state", string(newState)).Error; err != nil {
		return fmt.Errorf("shardsm write %s=%s: %w", s.id, newState, err)
	}
	s.mu.Lock()
	s.entry.State = newState
	s.mu.Unlock()
	return nil
}

// configure wires every state's permits + entry/exit actions.
// Reads top-to-bottom matching the FSM table in
// docs/plans/asg-as-api-stateless.md.
func (s *ShardSM) configure() {
	s.sm.Configure(StateLaunching).
		OnEntry(s.onEnterLaunching).
		PermitDynamic(TriggerHeartbeat, s.heartbeatTargetFromLaunching, s.heartbeatMatchesLaunching).
		Permit(TriggerLaunchFailed, StateGone)

	s.sm.Configure(StateIngesting).
		OnEntryFrom(TriggerHeartbeat, s.configureOnHeartbeat).
		Permit(TriggerRotateRequested, StateReadOnly).
		PermitDynamic(TriggerHeartbeatStale, s.staleTargetFromActive, s.actuallyStale)

	s.sm.Configure(StateReadOnly).
		Permit(TriggerArchiveRequested, StateArchived).
		PermitDynamic(TriggerHeartbeatStale, s.staleTargetFromActive, s.actuallyStale)

	s.sm.Configure(StateSuspended).
		OnEntry(s.onEnterSuspended).
		PermitDynamic(TriggerHeartbeat, s.heartbeatTargetFromSuspended, s.heartbeatMatchesSuspended).
		Permit(TriggerStranded, StateGone)

	s.sm.Configure(StateArchived).
		OnEntry(s.onEnterArchived).
		Permit(TriggerAdminDelete, StateGone)

	s.sm.Configure(StateGone).
		OnEntry(s.onEnterGone)
}

// ── Guards ──────────────────────────────────────────────────────

// heartbeatMatchesLaunching gates LAUNCHING → INGESTING. The
// heartbeat must carry a non-empty InstanceID that matches the
// entry we stamped during onEnterLaunching, and a non-empty Addr
// (the shard's gRPC listener).
func (s *ShardSM) heartbeatMatchesLaunching(_ context.Context, args ...any) bool {
	hb, ok := heartbeatArg(args)
	if !ok {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if hb.InstanceID == "" || hb.Addr == "" {
		return false
	}
	if s.entry.InstanceID != "" && hb.InstanceID != s.entry.InstanceID {
		// LAUNCHING with a known InstanceID can only be claimed by
		// that instance — a heartbeat from a different InstanceID is
		// a stray and must not promote this shard.
		return false
	}
	return true
}

// heartbeatTargetFromLaunching always returns INGESTING — the only
// permitted target. Declared as a PermitDynamic only because we
// share the trigger name with SUSPENDED→INGESTING (rebind).
func (s *ShardSM) heartbeatTargetFromLaunching(context.Context, ...any) (stateless.State, error) {
	return StateIngesting, nil
}

// heartbeatMatchesSuspended gates SUSPENDED → INGESTING. The
// heartbeat must claim our shard id and present a non-empty Addr.
// We don't enforce InstanceID here — recovery may have already
// updated entry.InstanceID to the replacement's id during
// SUSPENDED.OnEntry, or the heartbeat may arrive before that
// update lands (the launch action races the boot path).
func (s *ShardSM) heartbeatMatchesSuspended(_ context.Context, args ...any) bool {
	hb, ok := heartbeatArg(args)
	if !ok {
		return false
	}
	return hb.ShardID == s.id && hb.Addr != ""
}

func (s *ShardSM) heartbeatTargetFromSuspended(context.Context, ...any) (stateless.State, error) {
	return StateIngesting, nil
}

// actuallyStale guards the HeartbeatStale trigger so a tick that
// fires while a fresh heartbeat is already in flight doesn't move
// us to SUSPENDED prematurely. Returns true only if entry's
// LastHeartbeatAt is older than the staleness threshold.
func (s *ShardSM) actuallyStale(context.Context, ...any) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entry.LastHeartbeatAt.IsZero() {
		return false // never observed — treat as fresh until a heartbeat or evaluator clears it
	}
	return s.deps.Clock().Sub(s.entry.LastHeartbeatAt) > s.deps.StaleThreshold
}

// staleTargetFromActive picks SUSPENDED when the entry has an EBS
// volume (data preservable; replacement coming) and GONE when it
// doesn't (data already lost with the instance).
func (s *ShardSM) staleTargetFromActive(context.Context, ...any) (stateless.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entry.EBSVolumeID != "" {
		return StateSuspended, nil
	}
	return StateGone, nil
}

// ── Action stubs — Phase 2 wires the real bodies. ───────────────
//
// Each method is callable today (compiles + logs) so the FSM
// configuration is exercisable by the unit tests, but the actual
// AWS / gRPC side effects land alongside the provisioner rewrite
// in Phase 2 of docs/plans/asg-as-api-stateless.md.

func (s *ShardSM) onEnterLaunching(ctx context.Context, args ...any) error {
	log.Printf("shardsm %s: enter LAUNCHING (stub — Phase 2 wires EC2 + volume work)", s.id)
	return nil
}

func (s *ShardSM) configureOnHeartbeat(ctx context.Context, args ...any) error {
	hb, ok := heartbeatArg(args)
	if !ok {
		return fmt.Errorf("shardsm %s: configureOnHeartbeat missing heartbeat arg", s.id)
	}
	log.Printf("shardsm %s: enter INGESTING from heartbeat (instance=%s addr=%s) — stub",
		s.id, hb.InstanceID, hb.Addr)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entry.Addr = hb.Addr
	s.entry.InstanceID = hb.InstanceID
	s.entry.LastHeartbeatAt = s.deps.Clock()
	if hb.RowCount > 0 {
		s.entry.RowCount = hb.RowCount
	}
	return nil
}

func (s *ShardSM) onEnterSuspended(ctx context.Context, args ...any) error {
	log.Printf("shardsm %s: enter SUSPENDED (stub — Phase 2 kicks off recovery launch)", s.id)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entry.InstanceID = ""
	s.entry.Addr = ""
	s.entry.Shard = nil
	return nil
}

func (s *ShardSM) onEnterArchived(ctx context.Context, args ...any) error {
	log.Printf("shardsm %s: enter ARCHIVED (stub — Phase 2 wires Archive RPC + volume retire + instance terminate)", s.id)
	return nil
}

func (s *ShardSM) onEnterGone(ctx context.Context, args ...any) error {
	log.Printf("shardsm %s: enter GONE — removing from registry + deleting record", s.id)
	if s.deps.Registry != nil {
		s.deps.Registry.Remove(s.id)
	}
	if s.deps.Store != nil {
		if err := s.deps.Store.DeleteShard(s.id); err != nil {
			log.Printf("shardsm %s: DeleteShard: %v", s.id, err)
		}
	}
	return nil
}

// ── helpers ─────────────────────────────────────────────────────

// heartbeatArg extracts a store.ShardHeartbeat from the variadic
// args passed to Fire(TriggerHeartbeat, hb). Returns (zero, false)
// when no heartbeat is present.
func heartbeatArg(args []any) (store.ShardHeartbeat, bool) {
	for _, a := range args {
		if hb, ok := a.(store.ShardHeartbeat); ok {
			return hb, true
		}
		if hbp, ok := a.(*store.ShardHeartbeat); ok && hbp != nil {
			return *hbp, true
		}
	}
	return store.ShardHeartbeat{}, false
}
