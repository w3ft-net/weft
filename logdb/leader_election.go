package logdb

import (
	"context"
	"time"

	"github.com/graybeardcorp/weft/internal/weftlog"
	"github.com/graybeardcorp/weft/store"
)

// shardFSMLeaseName is the store.AcquireLease name guarding which
// coordinator instance drives shard/bucket FSM convergence
// (evaluateStateMachines) and FSM-transition-firing heartbeat handling
// (promoteOnHeartbeat). See docs/plans/coordinator-horizontal-scaling.md
// #5 and coordinator/nat.go's natLeaseName for the sibling lease this one
// is modeled on.
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

// startLeaderElection starts the shard-fsm-leader election loop in a
// background goroutine, unless Holder or the store is unset — the
// expected shape for every test that builds a Provisioner directly via
// NewProvisioner without going through Start, and for any deploy that
// never wires a holder id. In that case isLeader() keeps returning
// NewProvisioner's default of true: this process behaves as the sole,
// permanent leader, matching every pre-#5 caller's expectations.
func (p *Provisioner) startLeaderElection(ctx context.Context) {
	if p.config.Holder == "" || p.store == nil {
		weftlog.Infof(ctx, "provisioner: no holder id or store configured — not running shard-fsm-leader election (this process stays leader unconditionally)")
		return
	}
	// Seed the progress clock so a freshly-elected leader gets a full
	// progressStaleFactor*PollInterval window before the deadman's switch
	// can fire, rather than judging it against a zero timestamp (which
	// time.Since would read as "ancient" and trip the switch immediately).
	p.lastProgress.Store(time.Now().UnixNano())
	go p.runLeaderElection(ctx)
}

// runLeaderElection is the election loop: continuous-retry, same shape as
// coordinator/nat.go's NATElector.run — not poller/cron's try-once-at-boot
// AcquireAndHold, since a non-leader has to keep retrying every tick in
// case the leader dies or gives up the lease (progressStale below).
func (p *Provisioner) runLeaderElection(ctx context.Context) {
	p.leaderTick()

	ticker := time.NewTicker(store.LeaseRenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if p.isLeader() {
				// Best-effort — an unreleased lease just expires on its own
				// (LeaseTTL later), so skipping this on a crash is never a
				// correctness problem, only a slower handoff.
				if err := p.store.ReleaseLease(shardFSMLeaseName, p.config.Holder); err != nil {
					weftlog.Infof(ctx, "provisioner: release shard-fsm-leader lease on shutdown: %v", err)
				} else {
					store.LogLeaseEvent(shardFSMLeaseName, p.config.Holder, "released")
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
	if p.isLeader() && p.progressStale() {
		weftlog.Infof(context.Background(), "provisioner: shard-fsm-leader progress stale (no convergence in %s) — releasing lease rather than renewing", time.Since(p.lastProgressTime()))
		if err := p.store.ReleaseLease(shardFSMLeaseName, p.config.Holder); err != nil {
			weftlog.Infof(context.Background(), "provisioner: release stale shard-fsm-leader lease: %v", err)
		} else {
			store.LogLeaseEvent(shardFSMLeaseName, p.config.Holder, "deadman_released")
		}
		p.leading.Store(false)
		return
	}

	ok, err := p.store.AcquireLease(shardFSMLeaseName, p.config.Holder, store.LeaseTTL)
	if err != nil {
		weftlog.Errorf(context.Background(), "provisioner: shard-fsm-leader lease acquire error: %v", err)
		return
	}
	switch {
	case ok && !p.isLeader():
		weftlog.Infof(context.Background(), "provisioner: acquired shard-fsm-leader lease")
		store.LogLeaseEvent(shardFSMLeaseName, p.config.Holder, "acquired")
		// Reset the progress window on a fresh acquire so a just-elected
		// leader isn't immediately judged stale before its first tick has
		// had a chance to run.
		p.lastProgress.Store(time.Now().UnixNano())
		p.leading.Store(true)
	case !ok && p.isLeader():
		weftlog.Infof(context.Background(), "provisioner: lost shard-fsm-leader lease")
		store.LogLeaseEvent(shardFSMLeaseName, p.config.Holder, "lost")
		p.leading.Store(false)
	}
}

// progressStale reports whether this process has gone more than
// progressStaleFactor*PollInterval since evaluateStateMachines last made
// any observed progress (see markProgress).
func (p *Provisioner) progressStale() bool {
	return time.Since(p.lastProgressTime()) > progressStaleFactor*p.config.PollInterval
}

func (p *Provisioner) lastProgressTime() time.Time {
	return time.Unix(0, p.lastProgress.Load())
}
