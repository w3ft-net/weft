package logdb

import (
	"context"
	"errors"
	"fmt"
	weftsdk "github.com/w3ft-net/weft-go"
	"time"

	"github.com/qmuntal/stateless"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
	"github.com/graybeardcorp/weft/store"
)

// configureTimeout bounds the gRPC Dial+Configure on the
// LAUNCHING/SUSPENDED → INGESTING heartbeat path. archiveTimeout bounds
// the (much longer) ARCHIVED-cleanup work — export to S3, volume
// snapshot/detach/delete, and instance termination (both the archive-time
// and recovery-reap Terminate calls) — mirroring the values the pre-FSM
// provisioner used (christen and archive respectively). Every one of
// these is reachable from evaluateStateMachines, which #5's shard-fsm-
// leader lease assumes converges within a bounded time; an unbounded
// blocking call here would defeat that bound. See
// docs/plans/coordinator-horizontal-scaling.md #5 "Real gap #3".
const (
	configureTimeout = 30 * time.Second
	archiveTimeout   = 10 * time.Minute
)

// suspectStaleAfter is the silence (three missed 10s heartbeats) after
// which the stale check asks EC2 whether the shard's instance is gone. A
// gone instance is stale now; a running one waits out StaleThreshold, since
// recovery force-detaches the volume and a paused or cut-off process may
// still hold it.
const suspectStaleAfter = 30 * time.Second

// InstanceChecker reports whether an instance is dead or dying.
type InstanceChecker interface {
	InstanceGone(ctx context.Context, instanceID string) (bool, error)
}

// launchRetryBackoff is the production LaunchRetryBackoff: with the
// default budget of 5 attempts, a shard rides out at least four minutes
// of failing launches before it is given up.
const launchRetryBackoff = time.Minute

// The shard IS its own finite state machine. The methods below operate
// on *Shard directly (the sm/deps/launchInFlight fields live on the
// struct in registry.go); collapsing the old ShardSM wrapper into the
// type deletes the SM<->entry pairing the registry used to maintain (see
// docs/plans/shard-type-collapse.md Phase 2).
//
// All state transitions flow through Fire(). Action methods
// (onEnterLaunching, configureOnHeartbeat, …) are the only place side
// effects happen — registry mutation, gRPC dials, AWS calls. Guards
// (heartbeatMatchesLaunching, staleTargetFromActive, …) are pure
// predicates over the shard's fields.
//
// Concurrency: stateless's FiringQueued mode serializes triggers on this
// shard. The provisioner's per-shard heartbeat dispatch and the
// reconciler's tick can both Fire concurrently without racing the
// transition machinery. Action methods that touch the registry must
// still respect the registry's mutex; methods that touch the shard's own
// mutable fields take sh.mu.

// ShardDeps bundles the collaborators the action methods need. The
// provisioner owns ONE instance and every shard references it by pointer
// (bindSM) — it's services, not per-shard data. Kept narrow so test
// fixtures can mock individual fields rather than constructing a full
// Provisioner.
type ShardDeps struct {
	Store    shardRowStore // shard_records persistence (ShardStore in prod; nil on dev/test paths without persistence)
	Registry *ShardRegistry
	Launcher ShardLaunchAPI // launches/terminates instances; nil in tests that don't drive launches
	Volumes  VolumeManager  // resolves a shard's volume for the SUSPENDED stranded check + archive retire; nil skips both
	// Configurer dials a freshly-heartbeating shard over gRPC and sends
	// the Configure RPC, returning a live handle. Nil on dev/test paths
	// that don't drive real gRPC — the shard then records the heartbeat's
	// addr without attaching a connection.
	Configurer ShardConfigurer
	// ParquetDir is the S3 prefix root archive exports write under;
	// LambdaFunctionURL is threaded into the ColdShard handle the
	// archive swaps in. Both consumed only by the ARCHIVED-cleanup path.
	ParquetDir        string
	LambdaFunctionURL string
	Clock             func() time.Time
	// StaleThreshold mirrors heartbeatStaleThreshold; carried in deps so
	// tests can shrink it without touching the package-level const.
	StaleThreshold time.Duration
	// BootedAt is when this coordinator process started observing shards.
	// actuallyStale anchors its reference point at BootedAt so a shard
	// loaded after a restart — whose persisted last_heartbeat_at predates
	// the new process — gets a full StaleThreshold to re-report before it's
	// judged dead, instead of being suspended on the first post-restart tick.
	// Defaults to Clock() at construction (see newShardDeps).
	BootedAt time.Time
	// LaunchRetryBudget caps LaunchAttempts before LAUNCHING fires
	// LaunchFailed and transitions to GONE. Zero defaults to 5.
	LaunchRetryBudget int
	// LaunchRetryBackoff spaces a failed launch from the next attempt, so
	// the retry budget spans time rather than ticks (a fast tick would
	// otherwise spend it on one short capacity blip). Zero retries at the
	// next tick (tests).
	LaunchRetryBackoff time.Duration
	// Instances confirms a silent shard's instance is gone before
	// StaleThreshold (see suspectStaleAfter). Nil waits out the threshold.
	Instances InstanceChecker
	// VolumeSizesGB maps tier → fresh data-volume size in GiB, sourced from
	// Terraform (SHARD_<TIER>_VOLUME_GB) at boot. A tier absent here — or in
	// dev/test, where it's nil — falls back to defaultShardVolumeGB. Ignored
	// on recovery launches, which reattach the surviving volume.
	VolumeSizesGB map[string]int32

	// Exports and ExportManifests enable the ephemeral archive export
	// (docs/plans/ephemeral-archive-export.md): ARCHIVING snapshots the shard's
	// volume and has a short-lived instance export it, instead of the shard
	// exporting on its own (possibly tiny) host. Both nil — dev, docker, tests —
	// keeps the in-process Archive. They are set together or not at all.
	Exports         ExportOps
	ExportManifests shard.ManifestReader
	// ExportOutputs removes a failed export attempt's partial Parquet. Each
	// attempt writes to its own location (shard.ExportOutputSuffix), so this
	// only reclaims storage; correctness does not depend on it.
	ExportOutputs ObjectDeleter
	// ExportSizing overrides defaultExportSizing; MaxConcurrentExports caps
	// export instances in flight across all shards (0 → the default).
	ExportSizing         []ExportSize
	MaxConcurrentExports int

	// Spares and Pauser enable the seamless upgrade
	// (docs/plans/shard-seamless-upgrade.md): UPGRADING moves the shard's
	// volume to a claimed warm-pool spare behind a shard-enforced pause.
	// Both nil — dev, docker — keeps UPGRADING's reap-and-relaunch path.
	// They are set together or not at all.
	Spares *SparePool
	Pauser ShardPauser
	// UpgradeRouteGrace is how long a demoted instance keeps redirecting
	// after the swap before it may be terminated; zero defaults to
	// defaultUpgradeRouteGrace.
	UpgradeRouteGrace time.Duration

	// Leading reports whether this process still holds the shard-fsm-leader
	// lease. Every call a shard's FSM makes that changes the world (EC2, the
	// shard rows, a shard's pause) is refused once it returns false, however
	// far into an Evaluate the process has got: Evaluate checks leadership only
	// when it starts, and an upgrade runs for tens of seconds. Nil, as in
	// tests and dev, never refuses.
	Leading func() bool
}

// volumeSizeGB returns the fresh-launch data-volume size for tier, falling
// back to defaultShardVolumeGB when no per-tier override is wired (dev/test, or
// a tier missing from the env). Never returns 0 — CreateForShard rejects that.
func (d *ShardDeps) volumeSizeGB(tier string) int32 {
	if sz, ok := d.VolumeSizesGB[tier]; ok && sz > 0 {
		return sz
	}
	return defaultShardVolumeGB
}

// ErrUnknownShard is returned by the state accessor when the underlying
// shard_records row has been deleted out from under the shard (typically
// by GONE.OnEntry, or admin delete). The SM treats this as terminal —
// there is nothing left to do.
var ErrUnknownShard = errors.New("shard record not found")

// newShardDeps fills in the per-deployment defaults (Clock, thresholds,
// retry budget) and returns a pointer the provisioner shares across every
// shard. Apply defaults here, once, rather than per-bind.
func newShardDeps(d ShardDeps) *ShardDeps {
	if d.Clock == nil {
		d.Clock = time.Now
	}
	if d.StaleThreshold == 0 {
		d.StaleThreshold = heartbeatStaleThreshold
	}
	if d.BootedAt.IsZero() {
		d.BootedAt = d.Clock()
	}
	if d.LaunchRetryBudget == 0 {
		d.LaunchRetryBudget = 5
	}
	if d.UpgradeRouteGrace == 0 {
		d.UpgradeRouteGrace = defaultUpgradeRouteGrace
	}
	if d.Leading != nil {
		fenceDeps(&d)
	}
	return &d
}

// ensureSM attaches the FSM machinery to the shard if it isn't already:
// it wires a stateless.StateMachine whose external storage reads/writes
// shard_records.state and holds the shared deps. Idempotent — the
// provisioner calls it at mint/load time and again on every drive path
// (Evaluate, heartbeat dispatch), so an entry that entered the registry
// any other way (adoption, a late LoadFromStore) gets bound on first
// touch. Double-checked locking keeps the per-tick hot path cheap. The
// shard's initial state is read lazily from the store on the first Fire
// (via readState), so callers must not assume a state until the first
// trigger lands.
func (sh *Shard) ensureSM(deps *ShardDeps) {
	sh.mu.RLock()
	bound := sh.sm != nil
	sh.mu.RUnlock()
	if bound {
		return
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.sm != nil {
		return
	}
	sh.deps = deps
	sh.sm = stateless.NewStateMachineWithExternalStorage(
		sh.readState, sh.writeState, stateless.FiringQueued,
	)
	sh.configureSM()
}

// smState returns the shard's currently-persisted FSM state. Reads via
// the external-storage accessor, which goes to the DB; not for hot paths
// (use sh.State for in-memory inspection). Named smState — not State — to
// avoid colliding with the State field.
func (sh *Shard) smState(ctx context.Context) (ShardState, error) {
	st, err := sh.sm.State(ctx)
	if err != nil {
		return "", err
	}
	if st == nil {
		return "", ErrUnknownShard
	}
	return st.(ShardState), nil
}

// Fire dispatches a trigger. Wraps the stateless API so call sites don't
// have to import that package or pass context for the common case.
// Errors include guard-not-satisfied, unknown-trigger, and action-method
// failures.
func (sh *Shard) Fire(ctx context.Context, trigger Trigger, args ...any) error {
	return sh.sm.FireCtx(ctx, trigger, args...)
}

// readState is the stateless ExternalStorage accessor. Looks up the
// persisted state column. A missing row returns StateGone (treated as
// terminal; transitions out are ignored).
func (sh *Shard) readState(ctx context.Context) (stateless.State, error) {
	row, ok, err := sh.deps.Store.GetShard(sh.ID)
	if err != nil {
		return nil, fmt.Errorf("shardsm read %s: %w", sh.ID, err)
	}
	if !ok {
		return StateGone, nil
	}
	return row.State, nil
}

// writeState is the stateless ExternalStorage mutator. Writes the new
// state to shard_records and mirrors it onto the in-memory State field so
// non-FSM readers (the registry's MatchingEntries walk, dashboards) see
// the same value without another round-trip.
func (sh *Shard) writeState(ctx context.Context, st stateless.State) error {
	if err := leaseGone(ctx); err != nil {
		return err
	}
	newState := st.(ShardState)
	// Narrow write: only the state column (SetShardState). Lifecycle-owned
	// fields (window bounds, services, addr) move through SaveShard, so
	// this transition can't trample a concurrent SaveShard over them.
	if err := sh.deps.Store.SetShardState(sh.ID, newState); err != nil {
		return fmt.Errorf("shardsm write %s=%s: %w", sh.ID, newState, err)
	}
	sh.mu.Lock()
	sh.State = newState
	sh.mu.Unlock()
	return nil
}

// stateNow is the in-memory mirror of the persisted state.
func (sh *Shard) stateNow() ShardState {
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	return sh.State
}

// configureSM wires every state's permits + entry/exit actions. Reads
// top-to-bottom matching the FSM table in
// docs/plans/asg-as-api-stateless.md.
func (sh *Shard) configureSM() {
	sh.sm.Configure(StateLaunching).
		OnEntry(sh.onEnterLaunching).
		PermitDynamic(TriggerHeartbeat, sh.heartbeatTargetFromLaunching, sh.heartbeatMatchesLaunching).
		Permit(TriggerLaunchFailed, StateGone)

	sh.sm.Configure(StateIngesting).
		OnEntryFrom(TriggerHeartbeat, sh.configureOnHeartbeat).
		Permit(TriggerRotateRequested, StateReadOnly).
		Permit(TriggerIngestDegraded, StateDegraded).
		Permit(TriggerRelaunchRequested, StateUpgrading).
		PermitDynamic(TriggerHeartbeatStale, sh.staleTargetFromActive, sh.actuallyStale)

	// DEGRADED is INGESTING's circuit-breaker pause: out of write routing,
	// still read-queryable. It re-admits to INGESTING after a cooldown
	// (IngestRecovered), rotates out normally (RotateRequested), and — if
	// its instance also dies — drops to the recovery branch on the same
	// HeartbeatStale edge INGESTING has.
	//
	// OnEntryFrom(TriggerHeartbeat, configureOnHeartbeat): DEGRADED, like
	// READ_ONLY below, can be a SUSPENDED shard's rescue target
	// (heartbeatTargetFromSuspended → postRecoveryTarget) — see the
	// READ_ONLY comment for why this action must run here too.
	sh.sm.Configure(StateDegraded).
		OnEntry(sh.onEnterDegraded).
		OnEntryFrom(TriggerHeartbeat, sh.configureOnHeartbeat).
		Permit(TriggerIngestRecovered, StateIngesting).
		Permit(TriggerRotateRequested, StateReadOnly).
		Permit(TriggerRelaunchRequested, StateUpgrading).
		PermitDynamic(TriggerHeartbeatStale, sh.staleTargetFromActive, sh.actuallyStale)

	// OnEntryFrom(TriggerHeartbeat, configureOnHeartbeat): READ_ONLY is a
	// valid SUSPENDED-rescue target (a shard whose pre-recovery state
	// was READ_ONLY — heartbeatTargetFromSuspended → postRecoveryTarget
	// returns it) and this action is what actually records the
	// heartbeat: Addr/InstanceID/LastHeartbeatAt/RowCount/BinarySHA/Live.
	// Without it (as shipped, and the cause of a real 2026-09-24
	// production incident — see
	// TestShardSM_SuspendedRescueToReadOnlyRefreshesLiveness),
	// LastHeartbeatAt is never refreshed on the rescue, so the very
	// next Evaluate reads the same ancient value, immediately judges
	// the shard stale again, and fires straight back to SUSPENDED — a
	// permanent, minute-by-minute bounce for a perfectly healthy,
	// continuously-heartbeating shard, since it can never stay
	// READ_ONLY long enough for evaluateRecovery to get a stable
	// SUSPENDED read to reap-and-relaunch from either.
	sh.sm.Configure(StateReadOnly).
		OnEntryFrom(TriggerHeartbeat, sh.configureOnHeartbeat).
		Permit(TriggerArchiveRequested, StateArchiving).
		Permit(TriggerRelaunchRequested, StateUpgrading).
		PermitDynamic(TriggerHeartbeatStale, sh.staleTargetFromActive, sh.actuallyStale)

	// ARCHIVING is a deliberate non-substate of READ_ONLY (see states.go):
	// query-routability is handled in MatchingEntries, and inheriting
	// READ_ONLY's HeartbeatStale edge here would wrongly SUSPEND a shard
	// whose instance we're intentionally terminating.
	//
	// AdminDelete is the operator escape hatch for an archive that can never
	// complete — the host and volume are gone and no export was recorded, so
	// ArchiveComplete (which needs a parquet path) is unreachable. See
	// Provisioner.ForceRemove for the guard against orphaning live resources.
	sh.sm.Configure(StateArchiving).
		Permit(TriggerArchiveComplete, StateArchived).
		Permit(TriggerAdminDelete, StateGone)

	sh.sm.Configure(StateSuspended).
		OnEntry(sh.onEnterSuspended).
		PermitDynamic(TriggerHeartbeat, sh.heartbeatTargetFromSuspended, sh.heartbeatMatchesSuspended).
		Permit(TriggerRelaunch, StateLaunching).
		Permit(TriggerStranded, StateGone)

	// UPGRADING is SUSPENDED's operator-driven sibling: same Evaluate
	// (reap-then-relaunch via evaluateRecovery), same teardown
	// (onEnterUpgrading mirrors onEnterSuspended), but NO heartbeat-
	// promotion edge. RelaunchShard fires TriggerRelaunchRequested
	// against a HEALTHY shard whose process keeps heartbeating, and
	// SUSPENDED's blip-rescue permit would race the reap and silently
	// no-op the roll. Without the heartbeat permit, the only exits are
	// TriggerRelaunch (post-reap) → LAUNCHING or TriggerStranded → GONE.
	//
	// With the warm pool wired (upgrade.go) UPGRADING is instead a serving
	// state: evaluateUpgrading moves the volume to a spare and fires
	// UpgradeComplete back to the origin state, and a bucket rotation that
	// lands mid-upgrade amends the origin to READ_ONLY in place.
	sh.sm.Configure(StateUpgrading).
		OnEntry(sh.onEnterUpgrading).
		Permit(TriggerRelaunch, StateLaunching).
		Permit(TriggerStranded, StateGone).
		PermitDynamic(TriggerUpgradeComplete, sh.upgradeOriginTarget).
		InternalTransition(TriggerRotateRequested, sh.rotateDuringUpgrade)

	sh.sm.Configure(StateArchived).
		OnEntry(sh.onEnterArchived).
		Permit(TriggerAdminDelete, StateGone)

	sh.sm.Configure(StateGone).
		OnEntry(sh.onEnterGone)
}

// ── Guards ──────────────────────────────────────────────────────

// heartbeatMatchesLaunching gates LAUNCHING → INGESTING. The heartbeat
// must carry a non-empty InstanceID that matches the one we stamped
// during launch, and a non-empty Addr (the shard's gRPC listener).
func (sh *Shard) heartbeatMatchesLaunching(_ context.Context, args ...any) bool {
	hb, ok := heartbeatArg(args)
	if !ok {
		return false
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if hb.InstanceID == "" || hb.Addr == "" {
		return false
	}
	if sh.InstanceID != "" && hb.InstanceID != sh.InstanceID {
		// LAUNCHING with a known InstanceID can only be claimed by that
		// instance — a heartbeat from a different InstanceID is a stray and
		// must not promote this shard.
		return false
	}
	return true
}

// heartbeatTargetFromLaunching returns the state the shard should land
// in once its first post-launch heartbeat lands. For a fresh shard this
// is INGESTING. For a shard that was rolled (UPGRADING) or recovered
// (SUSPENDED) from READ_ONLY or DEGRADED, the post-launch destination
// must mirror the pre-recovery state — otherwise a READ_ONLY shard's
// roll briefly becomes write-routable (no data damage in practice but
// it triggers a redundant SCALING_DOWN cascade as the bucket scrambles
// to return the shard to READ_ONLY).
//
// We look up the prior state from shard_operations via
// Store.LatestPreRecoveryState (the canonical transition log). On any
// lookup error, or when there's no recovery history, default to
// INGESTING — the fresh-launch path.
func (sh *Shard) heartbeatTargetFromLaunching(_ context.Context, _ ...any) (stateless.State, error) {
	return sh.postRecoveryTarget()
}

// heartbeatMatchesSuspended gates SUSPENDED → INGESTING. The heartbeat
// must claim our shard id and present a non-empty Addr. We don't enforce
// InstanceID here — recovery may have already updated InstanceID to the
// replacement's id during SUSPENDED.OnEntry, or the heartbeat may arrive
// before that update lands (the launch action races the boot path).
func (sh *Shard) heartbeatMatchesSuspended(_ context.Context, args ...any) bool {
	hb, ok := heartbeatArg(args)
	if !ok {
		return false
	}
	return hb.ShardID == sh.ID && hb.Addr != ""
}

// heartbeatTargetFromSuspended mirrors heartbeatTargetFromLaunching's
// pre-recovery-state lookup: SUSPENDED's blip-rescue heartbeat-promote
// edge must return the shard to its pre-stale state, not unconditionally
// INGESTING.
func (sh *Shard) heartbeatTargetFromSuspended(_ context.Context, _ ...any) (stateless.State, error) {
	return sh.postRecoveryTarget()
}

// postRecoveryTarget is the shared lookup behind both
// heartbeatTargetFromLaunching and heartbeatTargetFromSuspended.
// Returns READ_ONLY or DEGRADED when shard_operations shows the shard
// transitioned into UPGRADING/SUSPENDED from one of those states;
// returns INGESTING otherwise (including when the lookup fails or
// when deps.Store is nil — the dev/test path).
func (sh *Shard) postRecoveryTarget() (stateless.State, error) {
	if sh.deps.Store == nil {
		return StateIngesting, nil
	}
	prior, err := sh.deps.Store.LatestPreRecoveryState(sh.ID)
	if err != nil {
		weftsdk.Errorf(context.Background(), "shardsm %s: LatestPreRecoveryState lookup failed, defaulting to INGESTING: %v", sh.ID, err)
		return StateIngesting, nil
	}
	switch prior {
	case StateReadOnly, StateDegraded:
		return prior, nil
	default:
		return StateIngesting, nil
	}
}

// actuallyStale guards the HeartbeatStale trigger so a tick that fires
// while a fresh heartbeat is already in flight doesn't move us to
// SUSPENDED prematurely. Returns true only if LastHeartbeatAt was older
// than the staleness threshold as of the registry's last heartbeat sweep
// (ShardRegistry.LivenessAsOf), so a sweep that runs late can't suspend a
// shard that was beating when last read; the clock stands in only when no
// sweep has run.
//
// Past suspectStaleAfter it asks EC2: an instance that is gone (terminated,
// stopped) is stale at once, without waiting out the threshold.
func (sh *Shard) actuallyStale(ctx context.Context, _ ...any) bool {
	silent, instanceID := sh.silence()
	if silent > sh.deps.StaleThreshold {
		return true
	}
	if silent <= suspectStaleAfter || sh.deps.Instances == nil || instanceID == "" {
		return false
	}
	gone, err := sh.deps.Instances.InstanceGone(ctx, instanceID)
	if err != nil {
		weftsdk.Infof(ctx, "shardsm %s: silent %s; can't tell whether %s is gone: %v", sh.ID, silent, instanceID, err)
		return false
	}
	if gone {
		weftsdk.Infof(ctx, "shardsm %s: silent %s and instance %s is gone", sh.ID, silent, instanceID)
	}
	return gone
}

// silence is how long the shard has gone without a heartbeat, and the
// instance it is on. Zero for a shard never observed: fresh until a
// heartbeat or evaluator clears it.
func (sh *Shard) silence() (time.Duration, string) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.LastHeartbeatAt.IsZero() {
		return 0, sh.InstanceID
	}
	// Anchor at boot: a heartbeat from before this coordinator started (a
	// shard loaded after a restart, its persisted last_heartbeat_at predating
	// the new process) can't prove current liveness, but it must not read as
	// instantly stale either — that suspended+relaunched every healthy shard
	// on the first post-restart tick. The shard gets a full StaleThreshold to
	// re-report; if it never does, it's correctly staled after the window.
	ref := sh.LastHeartbeatAt
	if ref.Before(sh.deps.BootedAt) {
		ref = sh.deps.BootedAt
	}
	asOf := sh.deps.Clock()
	if sh.deps.Registry != nil && sh.deps.Registry.livenessReadAt.Load() != 0 {
		asOf = sh.deps.Registry.LivenessAsOf()
	}
	return asOf.Sub(ref), sh.InstanceID
}

// staleTargetFromActive picks SUSPENDED when the shard has an EBS volume
// (data preservable; replacement coming) and GONE when it doesn't (data
// already lost with the instance).
func (sh *Shard) staleTargetFromActive(context.Context, ...any) (stateless.State, error) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.EBSVolumeID != "" {
		return StateSuspended, nil
	}
	return StateGone, nil
}

// ── Evaluate — the convergence driver ───────────────────────────
//
// Evaluate runs once per reconciler tick per shard. It reads the
// persisted state, observes reality (the shard's fields + clock), and
// fires whatever trigger moves the shard toward where it should be. All
// side-effectful convergence lives here rather than in OnEntry actions,
// so a restart that drops straight into Evaluate re-drives any
// half-finished work — there is no boot-vs-running mode.
//
// LAUNCHING drives a fresh launch (+ retry budget); INGESTING/READ_ONLY
// run stale-detection; SUSPENDED runs recovery's reap-then-relaunch; and
// ARCHIVING runs the export → ColdShard swap → volume retire → terminate
// sequence, firing ArchiveComplete → ARCHIVED once it's all done. That
// work lives in Evaluate rather than an OnEntry for the same reason the
// launch does: a crash mid-cleanup re-drives it on the next tick.
// ARCHIVED is terminal — nothing to converge.
func (sh *Shard) Evaluate(ctx context.Context) error {
	if sh.deps.Leading != nil {
		ctx = withLeaderFence(ctx, sh.deps.Leading)
	}
	st, err := sh.smState(ctx)
	if err != nil {
		return err
	}
	switch st {
	case StateLaunching:
		return sh.evaluateLaunching(ctx)
	case StateIngesting:
		return sh.evaluateIngesting(ctx)
	case StateReadOnly:
		return sh.evaluateActive(ctx)
	case StateDegraded:
		return sh.evaluateDegraded(ctx)
	case StateSuspended:
		return sh.evaluateRecovery(ctx)
	case StateUpgrading:
		if sh.upgradesInPlace() {
			return sh.evaluateUpgrading(ctx)
		}
		return sh.evaluateRecovery(ctx)
	case StateArchiving:
		return sh.evaluateArchiving(ctx)
	default:
		return nil
	}
}

// evaluateLaunching drives a LAUNCHING shard toward INGESTING. With an
// InstanceID already stamped it just waits for the heartbeat (which
// promotes it). Otherwise it either gives up (budget exhausted →
// LaunchFailed → GONE) or kicks an async launch — async because the
// launch polls AWS for minutes, far too long to block the tick.
func (sh *Shard) evaluateLaunching(ctx context.Context) error {
	sh.mu.Lock()
	instanceID := sh.InstanceID
	attempts := sh.LaunchAttempts
	sh.mu.Unlock()

	if instanceID != "" {
		return nil // launched; waiting for the first heartbeat to promote
	}
	if attempts >= sh.deps.LaunchRetryBudget {
		return sh.Fire(ctx, TriggerLaunchFailed)
	}
	if sh.deps.Launcher == nil {
		return nil // no launcher wired (dev/test path)
	}
	if !sh.launchInFlight.CompareAndSwap(false, true) {
		return nil // a launch is already in flight for this shard
	}
	go sh.runLaunch()
	return nil
}

// runLaunch performs the launch and persists the outcome. Detached from
// the tick context (the launch must outlive the tick), bounded by the
// launcher's own poll budget. Success stamps instance/AZ/volume; failure
// bumps launch_attempts so a later Evaluate fires LaunchFailed once the
// budget is spent.
func (sh *Shard) runLaunch() {
	defer sh.launchInFlight.Store(false)

	sh.mu.Lock()
	spec := LaunchSpec{
		ShardID:          sh.ID,
		Tier:             sh.Tier,
		AZ:               sh.AZ,
		ExistingVolumeID: sh.EBSVolumeID, // "" for a fresh shard
		SizeGB:           sh.deps.volumeSizeGB(sh.Tier),
	}
	stripeIndex := sh.StripeIndex
	sh.mu.Unlock()

	res, err := sh.deps.Launcher.Launch(context.Background(), spec)
	if err != nil {
		weftsdk.Errorf(context.Background(), "shardsm %s: launch failed: %v", sh.ID, err)
		if e := sh.deps.Store.IncrementShardLaunchAttempts(sh.ID); e != nil {
			weftsdk.Infof(context.Background(), "shardsm %s: bump launch_attempts: %v", sh.ID, e)
		}
		sh.mu.Lock()
		sh.LaunchAttempts++
		sh.mu.Unlock()
		// Hold the in-flight flag through the backoff: Evaluate starts no
		// new attempt until it clears.
		time.Sleep(sh.deps.LaunchRetryBackoff)
		return
	}

	if err := sh.deps.Store.SetShardLaunchResult(sh.ID, res.InstanceID, res.AZ, res.VolumeID, stripeIndex); err != nil {
		weftsdk.Infof(context.Background(), "shardsm %s: persist launch result: %v", sh.ID, err)
		return // leave InstanceID unstamped; next Evaluate retries
	}
	sh.mu.Lock()
	sh.InstanceID = res.InstanceID
	sh.locEpoch++
	sh.AZ = res.AZ
	sh.EBSVolumeID = res.VolumeID
	sh.mu.Unlock()
	weftsdk.Infof(context.Background(), "shardsm %s: launched (instance=%s vol=%s az=%s); awaiting heartbeat",
		sh.ID, res.InstanceID, res.VolumeID, res.AZ)
}

// evaluateActive fires HeartbeatStale when an INGESTING/READ_ONLY shard
// has gone silent past the stale threshold. The PermitDynamic on
// HeartbeatStale routes it to SUSPENDED (volume intact) or GONE (no
// volume) — see staleTargetFromActive.
func (sh *Shard) evaluateActive(ctx context.Context) error {
	if sh.actuallyStale(ctx) {
		return sh.Fire(ctx, TriggerHeartbeatStale)
	}
	return nil
}

// evaluateIngesting is evaluateActive plus the ingest circuit breaker: a
// shard trips to DEGRADED (out of write routing) when its consecutive
// ingest failures OR its consecutive coordinator→shard reconnect failures
// cross SuspendFailureThreshold. The reconnect arm catches the shard that
// heartbeats fine but the coordinator can't dial back (a new-AZ
// security-group gap), which would otherwise sit INGESTING with a nil
// handle, fresh-but-unroutable. Staleness wins — a silent shard goes to the
// recovery branch regardless of either count.
func (sh *Shard) evaluateIngesting(ctx context.Context) error {
	if sh.actuallyStale(ctx) {
		return sh.Fire(ctx, TriggerHeartbeatStale)
	}
	sh.mu.RLock()
	unhealthy := sh.IngestFailures >= SuspendFailureThreshold ||
		sh.reconnectFailures >= SuspendFailureThreshold
	sh.mu.RUnlock()
	if unhealthy {
		return sh.Fire(ctx, TriggerIngestDegraded)
	}
	return nil
}

// evaluateDegraded drives the breaker. A DEGRADED shard whose instance
// also went silent drops to the recovery branch (same as INGESTING).
// Otherwise, once the cooldown elapses it half-opens: reset the failure
// count and re-admit to INGESTING to retry writes. A zero degradedAt
// (lost across a restart) re-admits immediately — the shard retries and
// re-degrades on the next tick if still broken.
func (sh *Shard) evaluateDegraded(ctx context.Context) error {
	if sh.actuallyStale(ctx) {
		return sh.Fire(ctx, TriggerHeartbeatStale)
	}
	sh.mu.RLock()
	degradedAt := sh.degradedAt
	sh.mu.RUnlock()
	if !degradedAt.IsZero() && sh.deps.Clock().Sub(degradedAt) < ingestRecoveryCooldown {
		return nil // still cooling down
	}
	// Persist the reset before making it in memory: RefreshFromStore copies
	// ingest_failures from the row every tick, so a reset only in memory
	// comes straight back and the shard re-degrades without retrying a write.
	if sh.deps.Store != nil {
		if err := sh.deps.Store.SetShardIngestFailures(sh.ID, 0); err != nil {
			weftsdk.Infof(ctx, "shardsm %s: persist half-open ingest failure reset: %v", sh.ID, err)
		}
	}
	sh.mu.Lock()
	sh.IngestFailures = 0
	sh.reconnectFailures = 0
	sh.mu.Unlock()
	return sh.Fire(ctx, TriggerIngestRecovered) // → INGESTING (half-open)
}

// evaluateRecovery drives the reap phase shared by SUSPENDED (organic
// failure via HeartbeatStale) and UPGRADING (operator-driven roll via
// RelaunchShard). Both states hold the dead-or-stale instance; their
// only job is to reap it and hand off to LAUNCHING, which relaunches a
// replacement reattaching the surviving volume. If the volume itself
// has vanished there's nothing to recover — fire Stranded → GONE.
// Reaping (Terminate + clear InstanceID) is one fast, idempotent API
// call, so unlike the launch it's fine to run inline: a crash before
// the Relaunch fires just re-reaps next tick.
func (sh *Shard) evaluateRecovery(ctx context.Context) error {
	// Stranded: the volume that made recovery worth attempting is gone.
	if sh.deps.Volumes != nil {
		info, err := sh.deps.Volumes.FindByShard(ctx, sh.ID)
		if err != nil {
			return fmt.Errorf("shardsm %s: stranded check: %w", sh.ID, err)
		}
		if info == nil {
			return sh.Fire(ctx, TriggerStranded) // → GONE
		}
	}

	sh.mu.Lock()
	deadID := sh.InstanceID
	az := sh.AZ
	volumeID := sh.EBSVolumeID
	stripeIndex := sh.StripeIndex
	sh.mu.Unlock()

	// Reap the dead instance (clears ASG bookkeeping + decrements). An
	// already-gone instance is a no-op inside Terminate. On failure, stay
	// put and retry next tick — InstanceID is still set, so we haven't
	// advanced.
	if deadID != "" && sh.deps.Launcher != nil {
		termCtx, cancel := context.WithTimeout(ctx, archiveTimeout)
		err := sh.deps.Launcher.Terminate(termCtx, deadID)
		cancel()
		if err != nil {
			weftsdk.Errorf(ctx, "shardsm %s: reap %s failed, retrying next tick: %v", sh.ID, deadID, err)
			return nil
		}
	}

	// Clear the instance (narrow write, AZ/volume/stripe preserved) so
	// LAUNCHING is entered with InstanceID empty — the relaunch will
	// reattach EBSVolumeID and stamp the replacement's id.
	if err := sh.deps.Store.SetShardLaunchResult(sh.ID, "", az, volumeID, stripeIndex); err != nil {
		return fmt.Errorf("shardsm %s: clear instance before relaunch: %w", sh.ID, err)
	}
	// The failures that sent the shard here were the reaped instance's, and
	// the replacement starts with none: carried over, they have it trip the
	// breaker the moment it heartbeats in and sit out a full cooldown healthy.
	// Persisted first, since RefreshFromStore copies the row's count back
	// every tick.
	if err := sh.deps.Store.SetShardIngestFailures(sh.ID, 0); err != nil {
		return fmt.Errorf("shardsm %s: clear ingest failures before relaunch: %w", sh.ID, err)
	}
	sh.mu.Lock()
	sh.InstanceID = ""
	sh.IngestFailures = 0
	sh.reconnectFailures = 0
	sh.locEpoch++
	sh.mu.Unlock()

	return sh.Fire(ctx, TriggerRelaunch) // → LAUNCHING (recovery relaunch)
}

// evaluateArchiving runs the export → ColdShard swap → volume retire →
// terminate sequence, then fires ArchiveComplete → ARCHIVED once all of
// it has landed. The shard stays query-routable throughout (the live
// DuckDB serves until the export swaps in the ColdShard). Each step is
// gated on a persisted fact (ParquetPath set, volume cleared, instance
// cleared) so a completed step is never repeated, and any step's failure
// leaves the shard mid-cleanup for the next tick rather than unwinding the
// archive or advancing to ARCHIVED early.
func (sh *Shard) evaluateArchiving(ctx context.Context) error {
	sh.mu.Lock()
	parquetPath := sh.ParquetPath
	liveShard := sh.Live
	volumeID := sh.EBSVolumeID
	instanceID := sh.InstanceID
	services := sh.Services
	rowCount := sh.RowCount
	sh.mu.Unlock()

	// Step 1 — get the archive recorded (once). With the ephemeral export
	// wired (and data to export), a short-lived instance does the work off this
	// shard's host (advanceExport); otherwise the live shard exports in
	// process. A shard with no recorded path and no live handle has nothing the
	// in-process path can export; it falls through to the guard below.
	exportsOn := sh.deps.Exports != nil && rowCount > 0
	switch {
	case parquetPath != "":
		// Already recorded by an earlier tick.
	case exportsOn:
		if err := sh.advanceExport(ctx); err != nil {
			return err
		}
		sh.mu.RLock()
		parquetPath = sh.ParquetPath
		sh.mu.RUnlock()
		if parquetPath == "" {
			return nil // export still in progress; advanceExport logged why
		}
	case liveShard != nil:
		exportCtx, cancel := context.WithTimeout(ctx, archiveTimeout)
		path, stats, err := liveShard.Archive(exportCtx, sh.archiveS3Prefix(services))
		cancel()
		if err != nil {
			weftsdk.Errorf(ctx, "shardsm %s: archive export failed, retrying next tick: %v", sh.ID, err)
			return nil
		}
		if err := sh.recordArchive(path, stats); err != nil {
			weftsdk.Infof(ctx, "shardsm %s: %v; retrying next tick", sh.ID, err)
			return nil
		}
		parquetPath = path
	}

	// Step 1b — the export's snapshot and instance must be gone before the
	// shard's own volume is retired. Runs on every tick that still has export
	// facts, so a cleanup that failed after the archive was recorded is
	// retried here.
	if sh.deps.Exports != nil && sh.hasExportFacts() {
		if err := sh.endExportAttempt(ctx, false); err != nil {
			return err
		}
		if sh.hasExportFacts() {
			return nil // cleanup failed; endExportAttempt logged it
		}
	}

	// Steps 2 and 3 destroy the only copy of any unexported data, so they run
	// only once the export is recorded — or when there is provably nothing to
	// export (no live handle and no rows ever reported). A shard that holds
	// data but has no live handle stays ARCHIVING and retries; AdminDelete is
	// the operator escape hatch.
	if parquetPath == "" && rowCount > 0 {
		weftsdk.Infof(ctx, "shardsm %s: ARCHIVING with no export recorded and no live handle (%d rows); keeping volume %q and instance %q", sh.ID, rowCount, volumeID, instanceID)
		return nil
	}

	// Step 2 — retire the data volume (detach → delete).
	if volumeID != "" && sh.deps.Volumes != nil {
		if err := sh.retireVolume(ctx, volumeID); err != nil {
			weftsdk.Infof(ctx, "shardsm %s: retire volume %s, retrying next tick: %v", sh.ID, volumeID, err)
			return nil
		}
		if err := sh.deps.Store.ClearShardVolume(sh.ID); err != nil {
			return fmt.Errorf("shardsm %s: clear volume: %w", sh.ID, err)
		}
		sh.mu.Lock()
		sh.EBSVolumeID = ""
		sh.mu.Unlock()
	}

	// Step 3 — terminate the now-idle instance. Terminate is idempotent, so
	// re-running against an already-gone instance is a no-op; we still clear
	// the id so the completion check below can fire ArchiveComplete.
	if instanceID != "" && sh.deps.Launcher != nil {
		termCtx, cancel := context.WithTimeout(ctx, archiveTimeout)
		err := sh.deps.Launcher.Terminate(termCtx, instanceID)
		cancel()
		if err != nil {
			weftsdk.Infof(ctx, "shardsm %s: terminate %s, retrying next tick: %v", sh.ID, instanceID, err)
			return nil
		}
		if err := sh.deps.Store.ClearShardInstance(sh.ID); err != nil {
			return fmt.Errorf("shardsm %s: clear instance: %w", sh.ID, err)
		}
		sh.mu.Lock()
		sh.InstanceID = ""
		sh.locEpoch++
		sh.mu.Unlock()
	}

	// All three steps have landed (data in S3, volume retired, instance
	// gone) → the archive is durable and query-ready via the ColdShard.
	// Flip ARCHIVING → ARCHIVED. This is the one transition the shard fires
	// itself: only it knows the export finished.
	sh.mu.Lock()
	done := sh.ParquetPath != "" && sh.EBSVolumeID == "" && sh.InstanceID == ""
	sh.mu.Unlock()
	if done {
		return sh.Fire(ctx, TriggerArchiveComplete) // → ARCHIVED
	}
	return nil
}

// recordArchive records a finished archive: it swaps the live handle for a
// ColdShard (so registry walks still see a non-nil pointer), persists the
// parquet path and the per-customer stats, and closes the live handle. The
// ColdShard is built first so a failure there leaves nothing half-recorded.
func (sh *Shard) recordArchive(path string, stats []shard.CustomerStat) error {
	sh.mu.RLock()
	services, live := sh.Services, sh.Live
	sh.mu.RUnlock()

	cold, err := shard.NewColdShard(sh.ID, serviceNames(services), path, sh.deps.LambdaFunctionURL, nil)
	if err != nil {
		return fmt.Errorf("build cold shard: %w", err)
	}
	if err := sh.deps.Store.SetShardParquetPath(sh.ID, path); err != nil {
		return fmt.Errorf("persist parquet path: %w", err)
	}
	// Best-effort: archive stats drive the retention janitor but aren't
	// load-bearing for the archive itself. A failure here just leaves the shard
	// without stats; the janitor falls back to scanning that file. Don't fail
	// the whole archive over metadata.
	if err := sh.deps.Store.RecordArchiveStats(sh.ID, stats); err != nil {
		weftsdk.Infof(context.Background(), "shardsm %s: record archive stats: %v", sh.ID, err)
	}
	if live != nil {
		live.Close()
	}
	sh.mu.Lock()
	sh.Live = cold
	sh.ParquetPath = path
	sh.LastHeartbeatAt = time.Time{}
	sh.mu.Unlock()
	weftsdk.Infof(context.Background(), "shardsm %s: archived to %s", sh.ID, path)
	return nil
}

// archiveS3Prefix mirrors the pre-FSM provisioner's layout:
// <ParquetDir>/<service-label>, where the label collapses multi-service
// shards to "N-apps" and an empty set to "shared".
func (sh *Shard) archiveS3Prefix(services []store.ShardService) string {
	return fmt.Sprintf("%s/%s", sh.deps.ParquetDir, joinServices(serviceNames(services)))
}

// retireVolume runs detach → delete on an archived shard's data volume.
// The parquet export already landed the data in S3 (Step 1), so the
// volume itself is disposable. Detach is forced: the hosting instance
// may already be terminated (Step 3 hasn't run, or a prior failure left
// it dead), and a non-force detach hangs on a gone instance. Any step's
// error returns so the caller leaves the volume in place for a retry.
func (sh *Shard) retireVolume(ctx context.Context, volumeID string) error {
	ctx, cancel := context.WithTimeout(ctx, archiveTimeout)
	defer cancel()

	if err := sh.deps.Volumes.DetachFromInstance(ctx, volumeID, true); err != nil {
		return fmt.Errorf("detach: %w", err)
	}
	if err := sh.deps.Volumes.DeleteVolume(ctx, volumeID); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	return nil
}

// ── Action methods ──────────────────────────────────────────────

func (sh *Shard) onEnterLaunching(ctx context.Context, args ...any) error {
	weftsdk.Infof(ctx, "shardsm %s: enter LAUNCHING (stub — Phase 2 wires EC2 + volume work)", sh.ID)
	return nil
}

// configureOnHeartbeat is the OnEntryFrom(Heartbeat) action shared by
// every state a heartbeat-driven promotion can land in — INGESTING
// (fresh launch or SUSPENDED rescue), and READ_ONLY/DEGRADED (a
// SUSPENDED rescue whose pre-recovery state was one of those — see
// heartbeatTargetFromSuspended/postRecoveryTarget). It records the
// heartbeat's addr/instance/liveness and attaches the live handle the
// promote path already dialed. The Configure dial happens BEFORE this
// transition (Provisioner.promoteOnHeartbeat → Shard.dialConfigure), so
// by the time OnEntry runs the coordinator already has a working
// connection — an unreachable shard never reaches any of these states.
// The dialed handle rides in through the Fire args; it's nil on
// dev/test paths with no Configurer (and on direct sm.Fire callers),
// which then just record addr/instance without attaching a connection.
//
// Every state this is wired to must actually use it: LastHeartbeatAt is
// the field actuallyStale() reads, and it's set nowhere else on the
// promotion path. Landing in a state without this action means a
// perfectly healthy shard's own liveness clock never advances, and the
// very next Evaluate immediately re-detects it as stale — see
// TestShardSM_SuspendedRescueToReadOnlyRefreshesLiveness, the
// regression test for exactly that gap on READ_ONLY.
//
// SUSPENDED .OnEntry already dropped any dead handle before this runs, so
// there's no prior connection to close (see docs/plans/asg-as-api-
// stateless.md — "old Live was already nil; no deferred close needed").
func (sh *Shard) configureOnHeartbeat(_ context.Context, args ...any) error {
	hb, ok := heartbeatArg(args)
	if !ok {
		return fmt.Errorf("shardsm %s: configureOnHeartbeat missing heartbeat arg", sh.ID)
	}
	dialed := dialedArg(args)

	sh.mu.Lock()
	sh.Addr = hb.Addr
	sh.InstanceID = hb.InstanceID
	sh.locEpoch++
	sh.LastHeartbeatAt = sh.deps.Clock()
	if hb.RowCount > 0 {
		sh.RowCount = hb.RowCount
	}
	if hb.DiskBytes > 0 {
		sh.DiskBytes = hb.DiskBytes
	}
	if hb.BinarySHA != "" {
		sh.BinarySHA = hb.BinarySHA
	}
	if dialed != nil {
		sh.Live = dialed
	}
	sh.mu.Unlock()

	// Persist the live addr (narrow write) so a coordinator restart can
	// eager-dial this shard via ReconstructShard and RefreshFromStore
	// stops clobbering it with the stale empty row. The heartbeat path is
	// the sole owner of shard_records.addr.
	if err := sh.deps.Store.SetShardAddr(sh.ID, hb.InstanceID, hb.Addr); err != nil {
		return fmt.Errorf("shardsm %s: persist addr %s: %w", sh.ID, hb.Addr, err)
	}
	return nil
}

// dialConfigure dials the shard's advertised gRPC addr and sends Configure,
// returning the live handle. It's the reachability gate the heartbeat
// promote path runs BEFORE firing TriggerHeartbeat: a shard the coordinator
// can't reach (e.g. a new-AZ security-group gap that blocks coordinator →
// shard:9000 while the shard's own heartbeats still get through) must not be
// promoted into the ingest cohort, where a nil handle would silently exclude
// it from write routing while it strands a Target slot.
//
// Returns (nil, nil) when no Configurer is wired (dev/test) — those promote
// without a live handle, as before. No state mutation: the caller threads
// the returned handle into Fire so configureOnHeartbeat attaches it.
func (sh *Shard) dialConfigure(ctx context.Context, hb store.ShardHeartbeat) (shard.Shard, error) {
	if sh.deps.Configurer == nil {
		return nil, nil
	}
	sh.mu.RLock()
	req := &pb.ConfigureRequest{
		ShardId:        sh.ID,
		Services:       serviceNames(sh.Services),
		WindowStart:    sh.WindowStart.Format(time.RFC3339),
		WindowEnd:      sh.WindowEnd.Format(time.RFC3339),
		HeartbeatToken: sh.HeartbeatToken,
	}
	sh.mu.RUnlock()

	dialCtx, cancel := context.WithTimeout(ctx, configureTimeout)
	defer cancel()
	s, err := sh.deps.Configurer.Dial(dialCtx, hb.Addr, req)
	if err != nil {
		return nil, fmt.Errorf("shardsm %s: configure at %s: %w", sh.ID, hb.Addr, err)
	}
	return s, nil
}

// ObserveHeartbeat handles a heartbeat for a shard already past the
// LAUNCHING/SUSPENDED promotion (INGESTING or READ_ONLY) — the
// steady-state path ProcessHeartbeat routes here instead of firing a
// trigger (INGESTING permits no Heartbeat transition). It refreshes
// liveness (LastHeartbeatAt + RowCount, so evaluateActive's stale check
// stays clear) and, when the live gRPC handle is missing or the
// advertised addr has moved, reconnects. The handle is nil after a restart
// — LoadFromStore rehydrates the row but can't redial a live shard — so
// the first post-restart heartbeat is what reattaches it.
func (sh *Shard) ObserveHeartbeat(ctx context.Context, hb store.ShardHeartbeat) {
	// A beat from an instance other than the shard's current one is not the
	// shard's: the catch-up sweep re-delivers the last beat of an instance that
	// has died or been replaced for up to a minute, and dialing its address
	// failed, which counted toward the breaker and tripped it on the healthy
	// replacement. Beats that name no instance, or a shard with none recorded
	// yet, still count.
	sh.mu.RLock()
	current := sh.InstanceID
	sh.mu.RUnlock()
	if current != "" && hb.InstanceID != "" && hb.InstanceID != current {
		return
	}

	// The handle's own address is the truth: RefreshFromStore copies the
	// row's addr onto sh.Addr every tick, so sh.Addr can already show the
	// move while the handle still talks to the old instance.
	sh.mu.Lock()
	liveAddr := sh.liveAddr()
	needDial := sh.deps.Configurer != nil && hb.Addr != "" &&
		(sh.Live == nil || sh.Addr != hb.Addr || (liveAddr != "" && liveAddr != hb.Addr))
	sh.mu.Unlock()
	if needDial {
		if err := sh.reconnect(ctx, hb); errors.Is(err, errStaleBeat) {
			// Not this shard's instance any more: nothing in the beat counts.
			return
		} else if err != nil {
			weftsdk.Infof(ctx, "shardsm %s: reconnect on heartbeat at %s: %v", sh.ID, hb.Addr, err)
			sh.mu.Lock()
			sh.reconnectFailures++
			sh.mu.Unlock()
		} else {
			sh.mu.Lock()
			sh.reconnectFailures = 0
			sh.mu.Unlock()
		}
	}

	sh.mu.Lock()
	// Stamp when the beat happened, not when it was replayed: the catch-up
	// sweep re-delivers beats up to a minute old, and stamping Clock() for
	// those would keep a dead shard looking alive. Never move backwards, so
	// the sweep re-delivering the beat the Heartbeat RPC already recorded
	// changes nothing, and neither does it cost a write.
	changed := false
	if at := sh.deps.Clock().Add(-hb.Age); at.After(sh.LastHeartbeatAt) {
		sh.LastHeartbeatAt = at
		changed = true
	}
	if hb.RowCount > 0 && hb.RowCount != sh.RowCount {
		sh.RowCount = hb.RowCount
		changed = true
	}
	if hb.DiskBytes > 0 && hb.DiskBytes != sh.DiskBytes {
		sh.DiskBytes = hb.DiskBytes
		changed = true
	}
	if hb.BinarySHA != "" && hb.BinarySHA != sh.BinarySHA {
		sh.BinarySHA = hb.BinarySHA
		changed = true
	}
	sh.mu.Unlock()
	if changed && sh.deps.Registry != nil {
		sh.deps.Registry.TouchHeartbeat(sh)
	}
}

// errStaleBeat is reconnect's refusal of a beat from an instance the shard's
// row no longer names.
var errStaleBeat = errors.New("heartbeat from an instance the shard has left")

// reconnect dials hb.Addr, sends Configure, and swaps the fresh handle
// into the shard, deferring the old handle's close so in-flight queries
// drain rather than getting cut mid-stream. Shares the ConfigureRequest
// shape with configureOnHeartbeat. Caller has already checked that a dial
// is warranted (handle nil or addr moved) and that a Configurer exists.
func (sh *Shard) reconnect(ctx context.Context, hb store.ShardHeartbeat) error {
	sh.mu.Lock()
	req := &pb.ConfigureRequest{
		ShardId:        sh.ID,
		Services:       serviceNames(sh.Services),
		WindowStart:    sh.WindowStart.Format(time.RFC3339),
		WindowEnd:      sh.WindowEnd.Format(time.RFC3339),
		HeartbeatToken: sh.HeartbeatToken,
	}
	sh.mu.Unlock()

	dialCtx, cancel := context.WithTimeout(ctx, configureTimeout)
	s, err := sh.deps.Configurer.Dial(dialCtx, hb.Addr, req)
	cancel()
	if err != nil {
		return err
	}

	// Persist the (possibly moved) addr first, so it survives RefreshFromStore
	// and a later restart's eager-dial (same rationale as
	// configureOnHeartbeat), and so a beat from an instance the row no longer
	// names is refused before it can move this coordinator's handle.
	if err := sh.deps.Store.SetShardAddr(sh.ID, hb.InstanceID, hb.Addr); err != nil {
		if errors.Is(err, ErrShardMoved) {
			closeShard(s)
			return errStaleBeat
		}
		weftsdk.Infof(ctx, "shardsm %s: persist reconnect addr %s: %v", sh.ID, hb.Addr, err)
	}

	sh.mu.Lock()
	old := sh.Live
	sh.Live = s
	sh.Addr = hb.Addr
	sh.locEpoch++
	if hb.InstanceID != "" {
		sh.InstanceID = hb.InstanceID
	}
	sh.mu.Unlock()

	if old != nil {
		go func() {
			time.Sleep(defaultCloseRebindGrace)
			if e := old.Close(); e != nil {
				weftsdk.Infof(ctx, "shardsm %s: deferred close of old conn: %v", sh.ID, e)
			}
		}()
	}
	return nil
}

// onEnterSuspended tears down the dead connection but KEEPS InstanceID —
// that's the dead instance Evaluate must reap before relaunching. (Pure:
// no AWS side effects here; the reap + relaunch is evaluateRecovery's
// job, so a restart that lands in SUSPENDED re-drives it.)
func (sh *Shard) onEnterSuspended(ctx context.Context, args ...any) error {
	weftsdk.Infof(ctx, "shardsm %s: enter SUSPENDED — connection torn down; Evaluate will reap %s and relaunch",
		sh.ID, sh.InstanceID)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.Addr = ""
	sh.Live = nil
	return nil
}

// onEnterUpgrading mirrors onEnterSuspended for the operator-driven
// roll path: same teardown, same expectation that evaluateRecovery
// will reap the still-running instance and hand off to LAUNCHING. The
// state differs from SUSPENDED only in lacking a heartbeat-promotion
// edge, so the still-alive shard process can't race the reap.
func (sh *Shard) onEnterUpgrading(ctx context.Context, args ...any) error {
	if sh.upgradesInPlace() {
		// Seamless: the shard keeps serving from its instance until the
		// swap moves it; nothing to tear down.
		weftsdk.Infof(ctx, "shardsm %s: enter UPGRADING — serving on %s while its volume moves to a spare", sh.ID, sh.InstanceID)
		return nil
	}
	weftsdk.Infof(ctx, "shardsm %s: enter UPGRADING — connection torn down; Evaluate will reap %s and relaunch for binary upgrade",
		sh.ID, sh.InstanceID)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.Addr = ""
	sh.Live = nil
	return nil
}

// onEnterDegraded stamps the cooldown start. The connection and instance
// are left intact (unlike SUSPENDED) — the shard keeps serving reads and
// heartbeating; it's just out of write routing until evaluateDegraded
// re-admits it. Pure: no AWS side effects.
func (sh *Shard) onEnterDegraded(ctx context.Context, args ...any) error {
	sh.mu.Lock()
	sh.degradedAt = sh.deps.Clock()
	failures := sh.IngestFailures
	sh.mu.Unlock()
	weftsdk.Infof(ctx, "shardsm %s: enter DEGRADED — %d consecutive ingest failures; out of write routing, retrying after %s",
		sh.ID, failures, ingestRecoveryCooldown)
	return nil
}

// onEnterArchived marks the clean terminal: by the time ArchiveComplete
// fires this, evaluateArchiving has already exported the data, swapped in
// the ColdShard, retired the volume, and terminated the instance. Nothing
// to do but record the arrival.
func (sh *Shard) onEnterArchived(ctx context.Context, args ...any) error {
	weftsdk.Infof(ctx, "shardsm %s: ARCHIVED — data in S3, instance + volume retired", sh.ID)
	return nil
}

func (sh *Shard) onEnterGone(ctx context.Context, args ...any) error {
	weftsdk.Infof(ctx, "shardsm %s: enter GONE — removing from registry + deleting record", sh.ID)
	// An admin delete can land mid-export; don't leave its snapshot or export
	// instance behind with no row left to track them. Only shards that ever
	// attempted an export have anything to clean.
	sh.mu.RLock()
	exported := sh.ExportAttempt > 0
	sh.mu.RUnlock()
	if exported && sh.deps.Exports != nil {
		if err := sh.deps.Exports.Cleanup(ctx, sh.ID); err != nil {
			weftsdk.Infof(ctx, "shardsm %s: export cleanup on GONE (orphans possible; the sweep is the backstop): %v", sh.ID, err)
		}
	}
	if sh.deps.Registry != nil {
		sh.deps.Registry.Remove(sh.ID)
	}
	if sh.deps.Store != nil {
		if err := sh.deps.Store.DeleteShard(sh.ID); err != nil {
			weftsdk.Infof(ctx, "shardsm %s: DeleteShard: %v", sh.ID, err)
		}
	}
	return nil
}

// ── helpers ─────────────────────────────────────────────────────

// heartbeatArg extracts a store.ShardHeartbeat from the variadic args
// passed to Fire(TriggerHeartbeat, hb). Returns (zero, false) when no
// heartbeat is present.
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

// dialedArg extracts the pre-dialed live handle the promote path threads
// through Fire(TriggerHeartbeat, hb, dialed) for configureOnHeartbeat to
// attach. Returns nil when absent — dev/test paths and direct sm.Fire
// callers pass only the heartbeat.
func dialedArg(args []any) shard.Shard {
	for _, a := range args {
		if s, ok := a.(shard.Shard); ok {
			return s
		}
	}
	return nil
}
