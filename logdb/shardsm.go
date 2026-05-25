package logdb

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
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

	// launchInFlight is set while an async launch goroutine is working,
	// so successive Evaluate ticks don't kick a second launch for the
	// same shard. In-memory only: a restart resets it to false and the
	// next Evaluate re-drives any LAUNCHING row that never got an
	// InstanceID (the launch is idempotent on retry).
	launchInFlight atomic.Bool
}

// ShardSMDeps bundles the collaborators the action methods need.
// Kept narrow so test fixtures can mock individual fields rather
// than constructing a full Provisioner.
type ShardSMDeps struct {
	Store    *store.Store
	Registry *ShardRegistry
	Launcher ShardLaunchAPI // launches/terminates instances; nil in tests that don't drive launches
	Volumes  VolumeManager  // resolves a shard's volume for the SUSPENDED stranded check; nil skips it
	Clock    func() time.Time
	// StaleThreshold mirrors heartbeatStaleThreshold; carried in deps
	// so tests can shrink it without touching the package-level const.
	StaleThreshold time.Duration
	// LaunchRetryBudget caps LaunchAttempts before LAUNCHING fires
	// LaunchFailed and transitions to GONE. Zero defaults to 5.
	LaunchRetryBudget int
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
	if deps.LaunchRetryBudget == 0 {
		deps.LaunchRetryBudget = 5
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
		Permit(TriggerRelaunch, StateLaunching).
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

// ── Evaluate — the convergence driver ───────────────────────────
//
// Evaluate runs once per reconciler tick per shard. It reads the
// persisted state, observes reality (the entry + clock), and fires
// whatever trigger moves the shard toward where it should be. All
// side-effectful convergence lives here rather than in OnEntry
// actions, so a restart that drops straight into Evaluate re-drives
// any half-finished work — there is no boot-vs-running mode.
//
// B1.1 covers the LAUNCHING (fresh launch + retry budget) and active
// (stale-detection) paths. SUSPENDED recovery and ARCHIVED cleanup
// land in B1.2.
func (s *ShardSM) Evaluate(ctx context.Context) error {
	st, err := s.State(ctx)
	if err != nil {
		return err
	}
	switch st {
	case StateLaunching:
		return s.evaluateLaunching(ctx)
	case StateIngesting, StateReadOnly:
		return s.evaluateActive(ctx)
	case StateSuspended:
		return s.evaluateSuspended(ctx)
	default:
		return nil
	}
}

// evaluateLaunching drives a LAUNCHING shard toward INGESTING. With an
// InstanceID already stamped it just waits for the heartbeat (which
// promotes it). Otherwise it either gives up (budget exhausted →
// LaunchFailed → GONE) or kicks an async launch — async because the
// launch polls AWS for minutes, far too long to block the tick.
func (s *ShardSM) evaluateLaunching(ctx context.Context) error {
	s.mu.Lock()
	instanceID := s.entry.InstanceID
	attempts := s.entry.LaunchAttempts
	s.mu.Unlock()

	if instanceID != "" {
		return nil // launched; waiting for the first heartbeat to promote
	}
	if attempts >= s.deps.LaunchRetryBudget {
		return s.Fire(ctx, TriggerLaunchFailed)
	}
	if s.deps.Launcher == nil {
		return nil // no launcher wired (dev/test path)
	}
	if !s.launchInFlight.CompareAndSwap(false, true) {
		return nil // a launch is already in flight for this shard
	}
	go s.runLaunch()
	return nil
}

// runLaunch performs the launch and persists the outcome. Detached
// from the tick context (the launch must outlive the tick), bounded by
// the launcher's own poll budget. Success stamps instance/AZ/volume;
// failure bumps launch_attempts so a later Evaluate fires LaunchFailed
// once the budget is spent.
func (s *ShardSM) runLaunch() {
	defer s.launchInFlight.Store(false)

	s.mu.Lock()
	spec := LaunchSpec{
		ShardID:          s.id,
		Tier:             s.entry.Tier,
		AZ:               s.entry.AZ,
		ExistingVolumeID: s.entry.EBSVolumeID, // "" for a fresh shard
		SizeGB:           freshShardVolumeGB,
	}
	stripeIndex := s.entry.StripeIndex
	s.mu.Unlock()

	res, err := s.deps.Launcher.Launch(context.Background(), spec)
	if err != nil {
		log.Printf("shardsm %s: launch failed: %v", s.id, err)
		if e := s.deps.Store.IncrementShardLaunchAttempts(s.id); e != nil {
			log.Printf("shardsm %s: bump launch_attempts: %v", s.id, e)
		}
		s.mu.Lock()
		s.entry.LaunchAttempts++
		s.mu.Unlock()
		return
	}

	if err := s.deps.Store.SetShardLaunchResult(s.id, res.InstanceID, res.AZ, res.VolumeID, stripeIndex); err != nil {
		log.Printf("shardsm %s: persist launch result: %v", s.id, err)
		return // leave InstanceID unstamped; next Evaluate retries
	}
	s.mu.Lock()
	s.entry.InstanceID = res.InstanceID
	s.entry.AZ = res.AZ
	s.entry.EBSVolumeID = res.VolumeID
	s.mu.Unlock()
	log.Printf("shardsm %s: launched (instance=%s vol=%s az=%s); awaiting heartbeat",
		s.id, res.InstanceID, res.VolumeID, res.AZ)
}

// evaluateActive fires HeartbeatStale when an INGESTING/READ_ONLY shard
// has gone silent past the stale threshold. The PermitDynamic on
// HeartbeatStale routes it to SUSPENDED (volume intact) or GONE (no
// volume) — see staleTargetFromActive.
func (s *ShardSM) evaluateActive(ctx context.Context) error {
	if s.actuallyStale(ctx) {
		return s.Fire(ctx, TriggerHeartbeatStale)
	}
	return nil
}

// evaluateSuspended drives recovery's reap phase. SUSPENDED holds the
// dead instance; its only job is to reap it and hand off to LAUNCHING,
// which relaunches a replacement reattaching the surviving volume. If
// the volume itself has vanished there's nothing to recover — fire
// Stranded → GONE. Reaping (Terminate + clear InstanceID) is one fast,
// idempotent API call, so unlike the launch it's fine to run inline:
// a crash before the Relaunch fires just re-reaps next tick.
func (s *ShardSM) evaluateSuspended(ctx context.Context) error {
	// Stranded: the volume that made recovery worth attempting is gone.
	if s.deps.Volumes != nil {
		info, err := s.deps.Volumes.FindByShard(ctx, s.id)
		if err != nil {
			return fmt.Errorf("shardsm %s: stranded check: %w", s.id, err)
		}
		if info == nil {
			return s.Fire(ctx, TriggerStranded) // → GONE
		}
	}

	s.mu.Lock()
	deadID := s.entry.InstanceID
	az := s.entry.AZ
	volumeID := s.entry.EBSVolumeID
	stripeIndex := s.entry.StripeIndex
	s.mu.Unlock()

	// Reap the dead instance (clears ASG bookkeeping + decrements). An
	// already-gone instance is a no-op inside Terminate. On failure,
	// stay put and retry next tick — InstanceID is still set, so we
	// haven't advanced.
	if deadID != "" && s.deps.Launcher != nil {
		if err := s.deps.Launcher.Terminate(ctx, deadID); err != nil {
			log.Printf("shardsm %s: reap %s failed, retrying next tick: %v", s.id, deadID, err)
			return nil
		}
	}

	// Clear the instance (narrow write, AZ/volume/stripe preserved) so
	// LAUNCHING is entered with InstanceID empty — the relaunch will
	// reattach EBSVolumeID and stamp the replacement's id.
	if err := s.deps.Store.SetShardLaunchResult(s.id, "", az, volumeID, stripeIndex); err != nil {
		return fmt.Errorf("shardsm %s: clear instance before relaunch: %w", s.id, err)
	}
	s.mu.Lock()
	s.entry.InstanceID = ""
	s.mu.Unlock()

	return s.Fire(ctx, TriggerRelaunch) // → LAUNCHING (recovery relaunch)
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

// onEnterSuspended tears down the dead connection but KEEPS InstanceID
// — that's the dead instance Evaluate must reap before relaunching.
// (Pure: no AWS side effects here; the reap + relaunch is Evaluate's
// job, so a restart that lands in SUSPENDED re-drives it.)
func (s *ShardSM) onEnterSuspended(ctx context.Context, args ...any) error {
	log.Printf("shardsm %s: enter SUSPENDED — connection torn down; Evaluate will reap %s and relaunch",
		s.id, s.entry.InstanceID)
	s.mu.Lock()
	defer s.mu.Unlock()
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
