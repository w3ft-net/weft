package logdb

import (
	"context"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/w3ft-net/weft/backends/coordination"
)

// shardFSMLeaseName is the coordination.Leases name guarding which
// coordinator instance drives shard/bucket FSM convergence
// (evaluateStateMachines) and FSM-transition-firing heartbeat handling
// (promoteOnHeartbeat).
const shardFSMLeaseName = "shard-fsm-leader"

// progressStaleFactor is how many PollIntervals of no observed convergence
// progress (see lastProgress) the elector tolerates before it refuses to
// keep renewing the shard-fsm-leader lease and proactively releases it —
// the deadman's switch for a leader that's alive enough to reach Postgres
// but stuck inside one Evaluate call. Liveness (can still hit the DB) is
// not the same thing as progress (is actually converging); the lease
// primitive alone only proves the former. See "Real gap #3" in the plan
// doc.
const progressStaleFactor = 2

// progressStaleFloor is the least time without progress the deadman's
// switch tolerates, however fast the tick: the tick itself waits on AWS
// only in the warm pool's reconcile (reconcileStandbysTimeout), so that plus
// two ticks. Shard Evaluates that wait longer (a swap, an attach) run on
// workers and don't hold the tick up. Kept under coordination.LeaseTTL, so a
// stuck leader gives way about as fast as a dead one.
const progressStaleFloor = reconcileStandbysTimeout + 10*time.Second

// leases is who grants the shard-fsm-leader lease: config.Leases, else nobody.
func (p *Provisioner) leases() coordination.Leases { return p.config.Leases }

// startLeaderElection starts the shard-fsm-leader election loop in a
// background goroutine, unless Holder or the store is unset — the
// expected shape for every test that builds a Provisioner directly via
// NewProvisioner without going through Start, and for any deploy that
// never wires a holder id. In that case IsLeader() keeps returning
// NewProvisioner's default of true: this process behaves as the sole,
// permanent leader, matching every pre-#5 caller's expectations.
func (p *Provisioner) startLeaderElection(ctx context.Context) {
	if p.config.Holder == "" || p.leases() == nil {
		weftsdk.Infof(ctx, "provisioner: no holder id or leases configured — not running shard-fsm-leader election (this process stays leader unconditionally)")
		return
	}
	// Seed the progress clock so a freshly-elected leader gets a full
	// progressStaleFactor*PollInterval window before the deadman's switch
	// can fire, rather than judging it against a zero timestamp (which
	// time.Since would read as "ancient" and trip the switch immediately).
	p.lastProgress.Store(time.Now().UnixNano())
	p.electing.Store(true)
	p.electionDone = make(chan struct{})
	go p.runLeaderElection(ctx)
}

// LeaderElectionDone is closed once the election loop has stopped and, if this
// process led, given the lease back. A process that exits right after
// cancelling the context waits on it first: the release runs in the loop's
// goroutine, and without the wait a leader that is stopped (a deploy, a
// restart) usually exits before the release lands, so its successor sits out a
// whole LeaseTTL as a follower. It is already closed when no election runs.
func (p *Provisioner) LeaderElectionDone() <-chan struct{} {
	if p.electionDone == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	return p.electionDone
}

// runLeaderElection is the election loop: continuous-retry, not
// coordination.AcquireAndHold's try-once-at-boot, since a non-leader has to
// keep retrying every tick in case the leader dies or gives up the lease
// (progressStale below).
func (p *Provisioner) runLeaderElection(ctx context.Context) {
	defer close(p.electionDone)
	p.leaderTick()

	ticker := time.NewTicker(coordination.LeaseRenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if p.IsLeader() {
				// Best-effort — an unreleased lease just expires on its own
				// (LeaseTTL later), so skipping this on a crash is never a
				// correctness problem, only a slower handoff.
				if err := p.leases().ReleaseLease(shardFSMLeaseName, p.config.Holder); err != nil {
					weftsdk.Infof(ctx, "provisioner: release shard-fsm-leader lease on shutdown: %v", err)
				} else {
					coordination.LogLeaseEvent(shardFSMLeaseName, p.config.Holder, "released")
				}
			}
			return
		case <-ticker.C:
			p.leaderTick()
		}
	}
}

// leaderTick attempts to acquire/renew the shard-fsm-leader lease, unless
// this process is the current leader and its own convergence progress has
// gone stale — in which case it proactively releases the lease and skips
// the renewal attempt entirely, so a genuinely stuck process can't keep
// winning the DB-reachability check forever while doing no actual work.
// See progressStaleFactor and "Real gap #3" in the plan doc.
func (p *Provisioner) leaderTick() {
	if p.IsLeader() && p.progressStale() {
		weftsdk.Infof(context.Background(), "provisioner: shard-fsm-leader progress stale (no convergence in %s) — releasing lease rather than renewing", time.Since(p.lastProgressTime()))
		if err := p.leases().ReleaseLease(shardFSMLeaseName, p.config.Holder); err != nil {
			weftsdk.Infof(context.Background(), "provisioner: release stale shard-fsm-leader lease: %v", err)
		} else {
			coordination.LogLeaseEvent(shardFSMLeaseName, p.config.Holder, "deadman_released")
		}
		p.leading.Store(false)
		return
	}

	// Taken before the call, so the expiry it implies is never later than the
	// lease row's own.
	asked := time.Now()
	ok, err := p.leases().AcquireLease(shardFSMLeaseName, p.config.Holder, coordination.LeaseTTL)
	if ok && err == nil {
		p.leaseUntil.Store(asked.Add(coordination.LeaseTTL).UnixNano())
	}
	if err != nil {
		weftsdk.Errorf(context.Background(), "provisioner: shard-fsm-leader lease acquire error: %v", err)
		return
	}
	switch {
	case ok && !p.IsLeader():
		weftsdk.Infof(context.Background(), "provisioner: acquired shard-fsm-leader lease")
		coordination.LogLeaseEvent(shardFSMLeaseName, p.config.Holder, "acquired")
		// Reset the progress window on a fresh acquire so a just-elected
		// leader isn't immediately judged stale before its first tick has
		// had a chance to run.
		p.lastProgress.Store(time.Now().UnixNano())
		p.leading.Store(true)
	case !ok && p.IsLeader():
		weftsdk.Infof(context.Background(), "provisioner: lost shard-fsm-leader lease")
		coordination.LogLeaseEvent(shardFSMLeaseName, p.config.Holder, "lost")
		p.leading.Store(false)
	}
}

// leaseSafetyMargin is how far before its computed expiry a lease stops
// counting: the lease row expires by the database's clock, this process reads
// its own.
const leaseSafetyMargin = 5 * time.Second

// leaseValid reports whether this process may still act as the shard-fsm
// leader: it holds the lease, and the lease has not run out. The flag alone
// isn't enough. A process that was stopped for longer than the lease lasts
// (a long pause, a frozen VM) wakes with the flag still set, because the
// ticker that clears it hasn't run yet; the wall clock has already passed the
// expiry, so this refuses it at once.
func (p *Provisioner) leaseValid() bool {
	if !p.IsLeader() {
		return false
	}
	if !p.electing.Load() {
		return true
	}
	return time.Now().Add(leaseSafetyMargin).UnixNano() < p.leaseUntil.Load()
}

// progressStale reports whether this process has gone more than
// progressStaleFactor*PollInterval (at least progressStaleFloor) since
// evaluateStateMachines last made any observed progress (see markProgress).
func (p *Provisioner) progressStale() bool {
	return time.Since(p.lastProgressTime()) > max(progressStaleFactor*p.config.PollInterval, progressStaleFloor)
}

func (p *Provisioner) lastProgressTime() time.Time {
	return time.Unix(0, p.lastProgress.Load())
}
