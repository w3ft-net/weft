package logdb

import (
	"context"
	"fmt"
	"time"

	"github.com/w3ft-net/weft/backends/shardstore"

	"github.com/qmuntal/stateless"
	weftsdk "github.com/w3ft-net/weft-go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/w3ft-net/weft/grpctoken"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shard"
)

// The seamless upgrade: an UPGRADING
// shard keeps its id, window, volume and data and is never routed away
// from. Its volume moves from the old instance to a warm-pool standby while
// the shard's gate (shard/service/gate.go) and the callers' clients hold
// the calls that arrive meanwhile:
//
//	claiming → pausing → swapping → resumed → (origin state)
//	               ↘          ↘
//	                 rolling_back → (origin state, old instance)
//
// Every phase is a persisted fact on the row (migration 104), and each
// phase's step is idempotent and re-derives its progress from the row, the
// standby's claim and the volume's attachment, so a restart or a new leader
// drives forward from wherever the last one stopped.

// Phase budgets: past one, the upgrade gives up and rolls back.
const (
	// upgradeClaimBudget covers an on-demand standby's boot when the pool had
	// none.
	upgradeClaimBudget = 10 * time.Minute
	// upgradePauseBudget covers the old instance draining its in-flight
	// calls and releasing the volume (each Pause RPC is bounded by
	// pauseRPCTimeout; the leader retries until the budget is spent).
	upgradePauseBudget = 2 * time.Minute
	// upgradeSwapBudget covers detach, attach, mount and DuckDB open on the
	// standby. Callers are held for all of it, so it is a few times the slowest
	// healthy swap seen (detach 5.6s plus a 7s database open) and no more.
	upgradeSwapBudget = 30 * time.Second
	// upgradeRollBackBudget covers putting the volume back and resuming the
	// old instance. Past it the old instance is taken for dead and the
	// shard goes to recovery.
	upgradeRollBackBudget = 5 * time.Minute
	// upgradeReplacementBudget caps how long the demoted instance waits for
	// the pool's replacement standby to heartbeat before the upgrade finishes
	// anyway: a pool that can't launch must not wedge the roll.
	upgradeReplacementBudget = 15 * time.Minute
	// defaultUpgradeRouteGrace is how long the demoted instance keeps
	// redirecting after the swap when no tick is known: every coordinator
	// re-reads the row (and repoints its handle) once per provisioner tick,
	// so the provisioner sets it to routeGraceTicks of its own tick.
	defaultUpgradeRouteGrace = 2 * time.Minute
	routeGraceTicks          = 3

	pauseRPCTimeout  = time.Minute
	resumeRPCTimeout = 2 * time.Minute
	swapProbeRetry   = 100 * time.Millisecond
)

// swapProbeTimeout is how long one tick keeps asking the standby whether it
// has opened the shard, bounded by what is left of the swap budget. It matches
// the provisioner tick, so the standby is asked every swapProbeRetry from the
// attach until it answers, rather than once a tick: callers are holding
// meanwhile. A var for tests.
var swapProbeTimeout = time.Minute

// ShardPauser drives one shard instance's gate at addr. It dials the
// instance directly, never through the shard's live handle: a pause
// redirects that handle to the successor, and a Pause or Resume sent
// through it would land on the wrong instance.
type ShardPauser interface {
	Pause(ctx context.Context, addr, successor string) error
	Resume(ctx context.Context, addr string) error
}

// grpcPauser is the production ShardPauser.
type grpcPauser struct{ shardSecret string }

func (p grpcPauser) Pause(ctx context.Context, addr, successor string) error {
	return p.with(addr, func(c pb.ShardQueryClient) error {
		_, err := c.Pause(ctx, &pb.PauseRequest{SuccessorAddr: successor})
		return err
	})
}

func (p grpcPauser) Resume(ctx context.Context, addr string) error {
	return p.with(addr, func(c pb.ShardQueryClient) error {
		_, err := c.Resume(ctx, &pb.ResumeRequest{})
		return err
	})
}

func (p grpcPauser) with(addr string, call func(pb.ShardQueryClient) error) error {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(weftsdk.GRPC.UnaryClientInterceptor(), grpctoken.ClientTokenUnaryInterceptor(p.shardSecret)),
	)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()
	return call(pb.NewShardQueryClient(conn))
}

func (sh *Shard) upgradeFacts() UpgradeFacts {
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	return sh.Upgrade.Clone()
}

// setUpgradeFacts persists f, then mirrors it onto the entry (the DB is
// never behind memory, so RefreshFromStore can't undo it). The write only
// lands if the row is still in the phase this entry last saw, so a writer
// that missed another coordinator's progress cannot regress it. A leader's
// Evaluate that has lost the lease writes nothing (see fence.go).
func (sh *Shard) setUpgradeFacts(ctx context.Context, f UpgradeFacts) error {
	if err := leaseGone(ctx); err != nil {
		return err
	}
	sh.mu.RLock()
	from := sh.Upgrade.Phase
	sh.mu.RUnlock()
	if err := sh.standby().facts.SetShardUpgrade(sh.ID, from, f); err != nil {
		return fmt.Errorf("shardsm %s: persist upgrade phase %q: %w", sh.ID, f.Phase, err)
	}
	sh.mu.Lock()
	sh.Upgrade.Phase = f.Phase
	sh.Upgrade.Origin = f.Origin
	sh.Upgrade.PhaseAt = copyTimePtr(f.PhaseAt)
	sh.Upgrade.PausedAt = copyTimePtr(f.PausedAt)
	sh.Upgrade.SuccessorInstanceID = f.SuccessorInstanceID
	sh.Upgrade.SuccessorAddr = f.SuccessorAddr
	sh.Upgrade.DemotedInstanceID = f.DemotedInstanceID
	sh.Upgrade.DemotedAddr = f.DemotedAddr
	sh.Upgrade.TraceID = f.TraceID
	sh.Upgrade.ParentSpanID = f.ParentSpanID
	sh.mu.Unlock()
	return nil
}

// enterPhase persists f moved to phase, stamped now.
func (sh *Shard) enterPhase(ctx context.Context, f UpgradeFacts, phase UpgradePhase) (UpgradeFacts, error) {
	now := sh.deps.Clock()
	f.Phase, f.PhaseAt = phase, &now
	if err := sh.setUpgradeFacts(ctx, f); err != nil {
		return f, err
	}
	weftsdk.Infof(ctx, "shardsm %s: upgrade → %s", sh.ID, phase)
	return f, nil
}

// phaseAge is how long the upgrade has been in its current phase.
func (sh *Shard) phaseAge(f UpgradeFacts) time.Duration {
	if f.PhaseAt == nil {
		return 0
	}
	return sh.deps.Clock().Sub(*f.PhaseAt)
}

// upgradesInPlace reports whether this deployment upgrades through the
// warm pool. Without one (dev, docker) UPGRADING keeps its reap-and-
// relaunch recovery path.
func (sh *Shard) upgradesInPlace() bool { return sh.deps.Standbys != nil }

// traceIDOf is the trace ctx carries, or a fresh one for a caller that
// started none (an operator calling the RPC by hand), so every upgrade has
// a trace to log under.
func traceIDOf(ctx context.Context) string {
	if id, ok := weftsdk.TraceIDFromContext(ctx); ok {
		return id
	}
	return weftsdk.NewTraceID()
}

// spanIDOf is the span ctx is in, or "" when it is in none.
func spanIDOf(ctx context.Context) string {
	id, _ := weftsdk.CurrentSpanID(ctx)
	return id
}

// upgradeCtx is ctx under the trace and span the upgrade was started in. It is the
// only place a stored trace id is read, and startUpgrade the only place one
// is written, so a tick never adopts an id from an upgrade other than its
// own shard's.
func upgradeCtx(ctx context.Context, f UpgradeFacts) context.Context {
	if f.TraceID == "" {
		return ctx
	}
	ctx = weftsdk.ContextWithTraceID(ctx, f.TraceID)
	if f.ParentSpanID != "" {
		ctx = weftsdk.ContextWithSpanID(ctx, f.ParentSpanID)
	}
	return ctx
}

// startUpgrade records the upgrade's facts — where it returns to, the
// instance it leaves — and only then fires RelaunchRequested, so routing
// (which reads UpgradeOrigin) never sees an UPGRADING shard without them.
func (sh *Shard) startUpgrade(ctx context.Context) error {
	if sh.deps.Standbys != nil {
		if err := sh.deps.Standbys.BeginSwap(ctx, sh); err != nil {
			return err
		}
	}
	if err := sh.Fire(ctx, TriggerRelaunchRequested); err != nil {
		if sh.deps.Standbys != nil {
			if cerr := sh.setUpgradeFacts(ctx, UpgradeFacts{}); cerr != nil {
				weftsdk.Errorf(ctx, "shardsm %s: clear upgrade facts after a refused start: %v", sh.ID, cerr)
			}
		}
		return err
	}
	return nil
}

// upgradeOriginTarget is where UpgradeComplete lands: the state the
// upgrade started from (as a rotation mid-upgrade may have amended it).
func (sh *Shard) upgradeOriginTarget(context.Context, ...any) (stateless.State, error) {
	sh.mu.RLock()
	origin := sh.Upgrade.Origin
	sh.mu.RUnlock()
	switch origin {
	case StateIngesting, StateReadOnly, StateDegraded:
		return origin, nil
	}
	return nil, fmt.Errorf("shardsm %s: upgrade has no origin state to return to (%q)", sh.ID, origin)
}

// rotateDuringUpgrade is RotateRequested on an UPGRADING shard: the bucket
// rotated its cohort while the volume was moving. The shard stops taking
// writes now and returns to READ_ONLY when the upgrade completes.
func (sh *Shard) rotateDuringUpgrade(ctx context.Context, _ ...any) error {
	f := sh.upgradeFacts()
	if f.Origin == StateReadOnly {
		return nil
	}
	f.Origin = StateReadOnly
	if err := sh.setUpgradeFacts(ctx, f); err != nil {
		return err
	}
	weftsdk.Infof(ctx, "shardsm %s: rotated during its upgrade; will return to READ_ONLY", sh.ID)
	return nil
}

// evaluateUpgrading drives one step of the upgrade (see the file comment).
func (sh *Shard) evaluateUpgrading(ctx context.Context) error {
	f := sh.upgradeFacts()
	ctx = upgradeCtx(ctx, f)
	// While an instance should be heartbeating as the shard — the old one
	// while a standby is claimed, the new one after the swap — its silence
	// means it died, and the upgrade can't continue past a dead instance.
	// Not while pausing: a paused instance goes quiet by design, and a pause
	// that never completes rolls back on its own budget.
	switch f.Phase {
	case PhaseClaiming, PhaseResumed:
		if sh.actuallyStale(ctx) {
			return sh.abortForDeadInstance(ctx, f, "it stopped heartbeating")
		}
	}
	switch f.Phase {
	case "":
		// An UPGRADING row with no phase predates the pool (a roll in flight
		// across the deploy that introduced it). Nothing has moved yet, so
		// start from claiming.
		origin, err := sh.deps.Store.LatestPreRecoveryState(sh.ID)
		if err != nil || origin == "" {
			origin = StateIngesting
		}
		sh.mu.RLock()
		f = UpgradeFacts{Origin: origin, DemotedInstanceID: sh.InstanceID, DemotedAddr: sh.Addr}
		sh.mu.RUnlock()
		if f, err = sh.enterPhase(ctx, f, PhaseClaiming); err != nil {
			return err
		}
		return sh.upgradeClaim(ctx, f)
	case PhaseClaiming:
		return sh.upgradeClaim(ctx, f)
	case PhasePausing:
		return sh.upgradePause(ctx, f)
	case PhaseSwapping:
		return sh.upgradeSwap(ctx, f)
	case PhaseResumed:
		return sh.upgradeFinish(ctx, f)
	case PhaseRollingBack:
		return sh.upgradeRollBack(ctx, f)
	}
	return fmt.Errorf("shardsm %s: unknown upgrade phase %q", sh.ID, f.Phase)
}

// upgradeClaim holds a standby in the shard's (tier, AZ) — from the pool, or
// launched on demand — and, once it heartbeats, pauses the old instance.
func (sh *Shard) upgradeClaim(ctx context.Context, f UpgradeFacts) error {
	sh.mu.RLock()
	tier, az := sh.Tier, sh.AZ
	sh.mu.RUnlock()
	cctx, span := weftsdk.StartSpan(ctx, "upgrade.claim")
	sp, err := sh.standby().pool.ClaimOrLaunch(cctx, sh.ID, tier, az)
	span.RecordError(err)
	span.End()
	if err != nil {
		weftsdk.Infof(ctx, "shardsm %s: claim a spare in %s/%s, retrying next tick: %v", sh.ID, tier, az, err)
		return sh.giveUpIfOver(ctx, f, upgradeClaimBudget)
	}
	if f.SuccessorInstanceID != sp.InstanceID {
		f.SuccessorInstanceID = sp.InstanceID
		if err := sh.setUpgradeFacts(ctx, f); err != nil {
			return err
		}
	}
	if sp.Addr == "" || !sp.HeartbeatSince(sh.deps.Clock().Add(-sh.deps.StaleThreshold)) {
		weftsdk.Infof(ctx, "shardsm %s: waiting for spare %s to heartbeat", sh.ID, sp.InstanceID)
		return sh.giveUpIfOver(ctx, f, upgradeClaimBudget)
	}
	f.SuccessorAddr = sp.Addr
	f, err = sh.enterPhase(ctx, f, PhasePausing)
	if err != nil {
		return err
	}
	return sh.upgradePause(ctx, f)
}

// upgradePause asks the old instance to refuse new calls with a redirect
// to the standby, drain, close DuckDB and release the volume. Pause is
// idempotent, so a lost reply is simply retried.
func (sh *Shard) upgradePause(ctx context.Context, f UpgradeFacts) error {
	if f.DemotedAddr == "" {
		return sh.cutOverCold(ctx, f)
	}
	if f.PausedAt == nil {
		now := sh.deps.Clock()
		f.PausedAt = &now
		if err := sh.setUpgradeFacts(ctx, f); err != nil {
			return err
		}
	}
	start := sh.deps.Clock()
	pctx, cancel := context.WithTimeout(ctx, pauseRPCTimeout)
	pctx, span := weftsdk.StartSpan(pctx, "upgrade.pause")
	err := sh.standby().pauser.Pause(pctx, f.DemotedAddr, f.SuccessorAddr)
	span.RecordError(err)
	span.End()
	cancel()
	if status.Code(err) == codes.Unimplemented {
		weftsdk.Errorf(ctx, "shardsm %s: old instance %s has no Pause (a pre-upgrade binary); cutting over cold", sh.ID, f.DemotedInstanceID)
		f.DemotedAddr = ""
		if err := sh.setUpgradeFacts(ctx, f); err != nil {
			return err
		}
		return sh.cutOverCold(ctx, f)
	}
	if err != nil {
		weftsdk.Infof(ctx, "shardsm %s: pause %s toward %s, retrying: %v", sh.ID, f.DemotedAddr, f.SuccessorAddr, err)
		return sh.giveUpIfOver(ctx, f, upgradePauseBudget)
	}
	weftsdk.Infof(ctx, "shardsm %s: upgrade timing: old instance %s drained and released its volume in %s", sh.ID, f.DemotedInstanceID, sh.deps.Clock().Sub(start))
	f, err = sh.enterPhase(ctx, f, PhaseSwapping)
	if err != nil {
		return err
	}
	return sh.upgradeSwap(ctx, f)
}

// cutOverCold moves the shard off an old instance whose binary predates
// Pause, which only the first deploy of the seamless upgrade meets: the
// instance is terminated, as a roll did before, and the swap force-detaches
// its volume (it never unmounted) and attaches it to the standby. Callers see
// the old roll's outage, that once.
func (sh *Shard) cutOverCold(ctx context.Context, f UpgradeFacts) error {
	tctx, cancel := context.WithTimeout(ctx, archiveTimeout)
	err := sh.deps.Launcher.Terminate(tctx, f.DemotedInstanceID)
	cancel()
	if err != nil {
		weftsdk.Infof(ctx, "shardsm %s: terminate pre-upgrade instance %s, retrying: %v", sh.ID, f.DemotedInstanceID, err)
		return sh.giveUpIfOver(ctx, f, upgradePauseBudget)
	}
	f, err = sh.enterPhase(ctx, f, PhaseSwapping)
	if err != nil {
		return err
	}
	return sh.upgradeSwap(ctx, f)
}

// upgradeSwap moves the volume from the old instance to the standby and
// waits for the standby to open the shard. Progress is read off the volume's
// attachment, so the step resumes wherever it stopped. A detach that won't
// complete is never forced: the old instance may not have unmounted, and
// rolling back is safer than corrupting the filesystem.
func (sh *Shard) upgradeSwap(ctx context.Context, f UpgradeFacts) error {
	// A standby that died mid-swap takes the volume's attachment with it, and
	// every later attach to it fails. Retried, that holds the shard for the
	// whole swap budget; the old instance is intact, so go back to it now.
	if sh.successorGone(ctx, f) {
		weftsdk.Errorf(ctx, "shardsm %s: spare %s died mid-swap; rolling back", sh.ID, f.SuccessorInstanceID)
		_, err := sh.enterPhase(ctx, f, PhaseRollingBack)
		return err
	}
	info, err := sh.deps.Volumes.FindByShard(ctx, sh.ID)
	if err != nil {
		return fmt.Errorf("shardsm %s: find volume: %w", sh.ID, err)
	}
	if info == nil {
		weftsdk.Errorf(ctx, "shardsm %s: volume vanished mid-upgrade", sh.ID)
		return sh.Fire(ctx, TriggerStranded)
	}
	vctx, cancel := context.WithTimeout(ctx, upgradeSwapBudget)
	defer cancel()
	switch info.InstanceID {
	case f.DemotedInstanceID:
		start := sh.deps.Clock()
		cold := f.DemotedAddr == ""
		dctx, span := weftsdk.StartSpan(vctx, "upgrade.detach")
		err := sh.deps.Volumes.DetachFromInstance(dctx, info.VolumeID, cold)
		span.RecordError(err)
		span.End()
		if err != nil {
			weftsdk.Infof(ctx, "shardsm %s: detach %s from %s: %v", sh.ID, info.VolumeID, f.DemotedInstanceID, err)
			return sh.giveUpIfOver(ctx, f, upgradeSwapBudget)
		}
		weftsdk.Infof(ctx, "shardsm %s: upgrade timing: detached %s in %s", sh.ID, info.VolumeID, sh.deps.Clock().Sub(start))
		fallthrough
	case "":
		start := sh.deps.Clock()
		actx, span := weftsdk.StartSpan(vctx, "upgrade.attach")
		err := sh.deps.Volumes.AttachToInstance(actx, info.VolumeID, f.SuccessorInstanceID, shardVolumeDevice)
		span.RecordError(err)
		span.End()
		if err != nil {
			weftsdk.Infof(ctx, "shardsm %s: attach %s to %s: %v", sh.ID, info.VolumeID, f.SuccessorInstanceID, err)
			return sh.giveUpIfOver(ctx, f, upgradeSwapBudget)
		}
		weftsdk.Infof(ctx, "shardsm %s: upgrade timing: attached %s to %s in %s", sh.ID, info.VolumeID, f.SuccessorInstanceID, sh.deps.Clock().Sub(start))
	case f.SuccessorInstanceID:
	default:
		weftsdk.Errorf(ctx, "shardsm %s: volume %s is attached to %s, neither the old instance %s nor the spare %s; rolling back",
			sh.ID, info.VolumeID, info.InstanceID, f.DemotedInstanceID, f.SuccessorInstanceID)
		_, err := sh.enterPhase(ctx, f, PhaseRollingBack)
		return err
	}

	// The standby admits Configure (same id: a no-op) only once its database
	// is open, so a successful dial is the "DuckDB open" signal, and it
	// yields the coordinator's new handle.
	start := sh.deps.Clock()
	pctx, pcancel := context.WithTimeout(ctx, sh.swapProbeWindow(f, swapProbeTimeout))
	defer pcancel()
	pctx, span := weftsdk.StartSpan(pctx, "upgrade.open")
	defer span.End()
	for {
		dialed, err := sh.dialConfigure(pctx, shardstore.ShardHeartbeat{Addr: f.SuccessorAddr})
		if err == nil {
			weftsdk.Infof(ctx, "shardsm %s: upgrade timing: spare %s answered as the shard after %s", sh.ID, f.SuccessorAddr, sh.deps.Clock().Sub(start))
			return sh.completeSwap(ctx, f, dialed)
		}
		if pctx.Err() != nil {
			span.RecordError(err)
			weftsdk.Infof(ctx, "shardsm %s: spare %s not open yet: %v", sh.ID, f.SuccessorAddr, err)
			return sh.giveUpIfOver(ctx, f, upgradeSwapBudget)
		}
		select {
		case <-pctx.Done():
		case <-time.After(swapProbeRetry):
		}
	}
}

// completeSwap moves the shard's instance and address to the standby.
// Followers repoint their handles on the next row read; compute rebinds on
// the next descriptor; stragglers on the old address are redirected by the
// demoted instance's gate until upgradeFinish.
func (sh *Shard) completeSwap(ctx context.Context, f UpgradeFacts, dialed shard.Shard) error {
	sh.mu.RLock()
	az, volumeID, stripe := sh.AZ, sh.EBSVolumeID, sh.StripeIndex
	sh.mu.RUnlock()
	if err := sh.deps.Store.SetShardLaunchResult(sh.ID, f.SuccessorInstanceID, az, volumeID, stripe); err != nil {
		closeShard(dialed)
		return fmt.Errorf("shardsm %s: persist the spare as the shard's instance: %w", sh.ID, err)
	}
	if err := sh.deps.Store.SetShardAddr(sh.ID, f.SuccessorInstanceID, f.SuccessorAddr); err != nil {
		closeShard(dialed)
		return fmt.Errorf("shardsm %s: persist the spare's address: %w", sh.ID, err)
	}
	sh.mu.Lock()
	old := sh.Live
	sh.InstanceID = f.SuccessorInstanceID
	sh.locEpoch++
	sh.Addr = f.SuccessorAddr
	if dialed != nil {
		sh.Live = dialed
	}
	sh.LastHeartbeatAt = sh.deps.Clock()
	sh.mu.Unlock()
	if old != nil && old != sh.Live {
		go func() {
			time.Sleep(defaultCloseRebindGrace)
			closeShard(old)
		}()
	}
	swapAge := sh.phaseAge(f)
	resumed, err := sh.enterPhase(ctx, f, PhaseResumed)
	if err != nil {
		return err
	}
	weftsdk.Infof(ctx, "shardsm %s: now on %s (%s); %s keeps redirecting until routes converge", sh.ID, f.SuccessorInstanceID, f.SuccessorAddr, f.DemotedInstanceID)
	sh.logHoldWindow(ctx, resumed, "moved volume", swapAge)
	return nil
}

// upgradeFinish terminates the demoted instance once every coordinator
// routes to the new address and the pool's replacement standby heartbeats
// (the next shard's successor in this AZ), then returns the shard to its
// origin state.
func (sh *Shard) upgradeFinish(ctx context.Context, f UpgradeFacts) error {
	grace := sh.deps.UpgradeRouteGrace
	if sh.phaseAge(f) < grace {
		return nil
	}
	if sh.deps.Standbys != nil {
		sh.mu.RLock()
		tier, az := sh.Tier, sh.AZ
		sh.mu.RUnlock()
		ready, err := sh.standby().pool.Covered(tier, az)
		if err != nil {
			return fmt.Errorf("shardsm %s: replacement spare check: %w", sh.ID, err)
		}
		if !ready {
			if sh.phaseAge(f) < upgradeReplacementBudget {
				return nil
			}
			weftsdk.Errorf(ctx, "shardsm %s: no replacement spare in %s/%s after %s; finishing the upgrade without one", sh.ID, tier, az, upgradeReplacementBudget)
		}
	}
	if f.DemotedInstanceID != "" && f.DemotedAddr != "" && sh.deps.Launcher != nil {
		tctx, cancel := context.WithTimeout(ctx, archiveTimeout)
		err := sh.deps.Launcher.Terminate(tctx, f.DemotedInstanceID)
		cancel()
		if err != nil {
			weftsdk.Infof(ctx, "shardsm %s: terminate demoted %s, retrying next tick: %v", sh.ID, f.DemotedInstanceID, err)
			return nil
		}
	}
	return sh.endUpgrade(ctx, "upgraded onto "+f.SuccessorInstanceID)
}

// upgradeRollBack returns the volume to the demoted instance and resumes
// it. The standby is paused (so callers it admitted go back), terminated,
// and never returns to the pool.
func (sh *Shard) upgradeRollBack(ctx context.Context, f UpgradeFacts) error {
	if f.DemotedAddr == "" {
		return sh.abortForDeadInstance(ctx, f, "it was terminated for a cold cut-over the spare couldn't take")
	}
	info, err := sh.deps.Volumes.FindByShard(ctx, sh.ID)
	if err != nil {
		return fmt.Errorf("shardsm %s: find volume: %w", sh.ID, err)
	}
	if info == nil {
		weftsdk.Errorf(ctx, "shardsm %s: volume vanished mid-rollback", sh.ID)
		return sh.Fire(ctx, TriggerStranded)
	}
	vctx, cancel := context.WithTimeout(ctx, upgradeSwapBudget)
	defer cancel()
	switch {
	case info.InstanceID != "" && info.InstanceID == f.SuccessorInstanceID:
		// The standby may have opened the shard: have it release the volume
		// cleanly first. If it can't, terminating it shuts it down, which
		// unmounts the filesystem and detaches the volume.
		pctx, pcancel := context.WithTimeout(ctx, pauseRPCTimeout)
		perr := sh.standby().pauser.Pause(pctx, f.SuccessorAddr, f.DemotedAddr)
		pcancel()
		if perr == nil {
			if err := sh.deps.Volumes.DetachFromInstance(vctx, info.VolumeID, false); err == nil {
				return sh.upgradeRollBack(ctx, f)
			}
		}
		if err := sh.abandonSuccessor(ctx, f); err != nil {
			weftsdk.Infof(ctx, "shardsm %s: terminate spare %s for rollback, retrying: %v", sh.ID, f.SuccessorInstanceID, err)
		}
		return nil // the volume detaches as the standby shuts down
	case info.InstanceID == "":
		if err := sh.deps.Volumes.AttachToInstance(vctx, info.VolumeID, f.DemotedInstanceID, shardVolumeDevice); err != nil {
			if sh.instanceGone(ctx, f.DemotedInstanceID) || sh.phaseAge(f) > upgradeRollBackBudget {
				return sh.abortForDeadInstance(ctx, f, fmt.Sprintf("the volume cannot go back to it: %v", err))
			}
			weftsdk.Infof(ctx, "shardsm %s: reattach %s to %s, retrying: %v", sh.ID, info.VolumeID, f.DemotedInstanceID, err)
			return nil
		}
		fallthrough
	case info.InstanceID == f.DemotedInstanceID:
		rctx, rcancel := context.WithTimeout(ctx, resumeRPCTimeout)
		err := sh.standby().pauser.Resume(rctx, f.DemotedAddr)
		rcancel()
		if err != nil {
			if sh.phaseAge(f) > upgradeRollBackBudget {
				return sh.abortForDeadInstance(ctx, f, fmt.Sprintf("it would not resume within %s: %v", upgradeRollBackBudget, err))
			}
			weftsdk.Infof(ctx, "shardsm %s: resume %s, retrying: %v", sh.ID, f.DemotedAddr, err)
			return nil
		}
	default:
		weftsdk.Errorf(ctx, "shardsm %s: rollback found volume %s attached to %s, neither %s nor %s; leaving it for an operator",
			sh.ID, info.VolumeID, info.InstanceID, f.DemotedInstanceID, f.SuccessorInstanceID)
		return nil
	}
	sh.logHoldWindow(ctx, f, "rolled back", sh.phaseAge(f))

	// A paused instance heartbeats nothing, so its last beat can be minutes old.
	// It has just answered Resume, which is as good as a beat: without this the
	// next tick reads the pause's silence as death and recovers a shard that is
	// fine (completeSwap stamps the standby's liveness the same way).
	sh.mu.Lock()
	sh.LastHeartbeatAt = sh.deps.Clock()
	sh.mu.Unlock()

	if err := sh.abandonSuccessor(ctx, f); err != nil {
		weftsdk.Infof(ctx, "shardsm %s: terminate spare %s after rollback (the pool reconciler will retry): %v", sh.ID, f.SuccessorInstanceID, err)
	}
	sh.mu.Lock()
	live := sh.Live
	sh.mu.Unlock()
	if m, ok := live.(repointable); ok {
		m.Repoint(f.DemotedAddr)
	}
	return sh.endUpgrade(ctx, "rolled back; still on "+f.DemotedInstanceID)
}

// abortForDeadInstance ends an upgrade whose serving instance died. The
// instance that isn't the shard's — the claimed standby before the swap, the
// demoted instance after it — is terminated now, since nothing tracks it
// once the upgrade's facts are cleared; then the shard returns to its
// origin state, whose stale check sends it down the recovery path (which
// reaps the dead instance and reattaches the volume to a standby). Going
// through the origin state keeps the transition log naming it, so recovery
// returns the shard to it.
func (sh *Shard) abortForDeadInstance(ctx context.Context, f UpgradeFacts, why string) error {
	sh.mu.RLock()
	serving := sh.InstanceID
	sh.mu.RUnlock()
	weftsdk.Errorf(ctx, "shardsm %s: abandoning its upgrade: instance %s is dead (%s)", sh.ID, serving, why)
	if f.SuccessorInstanceID != "" && f.SuccessorInstanceID != serving {
		if err := sh.abandonSuccessor(ctx, f); err != nil {
			weftsdk.Infof(ctx, "shardsm %s: terminate spare %s (the pool reconciler will retry): %v", sh.ID, f.SuccessorInstanceID, err)
		}
	}
	if f.DemotedInstanceID != "" && f.DemotedInstanceID != serving && sh.deps.Launcher != nil {
		tctx, cancel := context.WithTimeout(ctx, archiveTimeout)
		err := sh.deps.Launcher.Terminate(tctx, f.DemotedInstanceID)
		cancel()
		if err != nil {
			weftsdk.Infof(ctx, "shardsm %s: terminate demoted %s, retrying next tick: %v", sh.ID, f.DemotedInstanceID, err)
			return nil
		}
	}
	return sh.endUpgrade(ctx, "abandoned: instance "+serving+" died; recovery takes over")
}

// logHoldWindow logs how long callers held: from the first Pause to now,
// when the shard serves again (on the standby, or back on the old instance).
// lastPhase is how long the phase that just ended took. One line per
// upgrade, so the window can be read off the coordinator's log.
func (sh *Shard) logHoldWindow(ctx context.Context, f UpgradeFacts, outcome string, lastPhase time.Duration) {
	if f.PausedAt == nil {
		weftsdk.Infof(ctx, "shardsm %s: upgrade hold window unknown (%s; no pause recorded)", sh.ID, outcome)
		return
	}
	weftsdk.Infof(ctx, "shardsm %s: upgrade hold window %s (%s; last phase took %s)", sh.ID, sh.deps.Clock().Sub(*f.PausedAt), outcome, lastPhase)
}

// abandonSuccessor terminates the claimed standby and drops its claim.
func (sh *Shard) abandonSuccessor(ctx context.Context, f UpgradeFacts) error {
	if f.SuccessorInstanceID == "" {
		return nil
	}
	return sh.standby().pool.Abandon(ctx, f.SuccessorInstanceID)
}

// successorGone reports whether the standby an upgrade is moving the volume to is
// dead or dying, by EC2's account. Without a checker, or when EC2 can't say, it
// is not: the swap's own budget is the backstop.
func (sh *Shard) successorGone(ctx context.Context, f UpgradeFacts) bool {
	return sh.instanceGone(ctx, f.SuccessorInstanceID)
}

// instanceGone reports whether EC2 says the instance is dead or dying.
// Without a checker, an id, or an answer it is not.
func (sh *Shard) instanceGone(ctx context.Context, id string) bool {
	if sh.deps.Instances == nil || id == "" {
		return false
	}
	gone, err := sh.deps.Instances.InstanceGone(ctx, id)
	return err == nil && gone
}

// giveUpIfOver rolls the upgrade back once the current phase has outlived
// budget; otherwise the next tick retries.
func (sh *Shard) giveUpIfOver(ctx context.Context, f UpgradeFacts, budget time.Duration) error {
	if sh.phaseAge(f) <= budget {
		return nil
	}
	weftsdk.Errorf(ctx, "shardsm %s: upgrade phase %s exceeded %s; rolling back", sh.ID, f.Phase, budget)
	_, err := sh.enterPhase(ctx, f, PhaseRollingBack)
	return err
}

// endUpgrade returns the shard to its origin state, then clears the
// upgrade's facts. In that order: the transition reads the origin, and a
// crash between the two leaves stale facts on a shard that is no longer
// UPGRADING (ignored) rather than an UPGRADING shard with no facts.
func (sh *Shard) endUpgrade(ctx context.Context, outcome string) error {
	if err := sh.Fire(ctx, TriggerUpgradeComplete); err != nil {
		return fmt.Errorf("shardsm %s: complete upgrade: %w", sh.ID, err)
	}
	if err := sh.setUpgradeFacts(ctx, UpgradeFacts{}); err != nil {
		weftsdk.Infof(ctx, "shardsm %s: clear upgrade facts: %v", sh.ID, err)
	}
	weftsdk.Infof(ctx, "shardsm %s: upgrade done: %s", sh.ID, outcome)
	return nil
}

// closeShard closes a handle that may be nil.
func closeShard(s shard.Shard) {
	if s != nil {
		_ = s.Close()
	}
}

// swapProbeWindow is how long one tick asks the standby whether it has opened
// the shard: the probe timeout, cut to what is left of the swap budget.
func (sh *Shard) swapProbeWindow(f UpgradeFacts, probeTimeout time.Duration) time.Duration {
	return min(probeTimeout, max(upgradeSwapBudget-sh.phaseAge(f), swapProbeRetry))
}
