package logdb

import (
	"context"
)

// Standbys is the warm-standby capability: booted, volume-less shard instances
// that a shard can move onto without waiting for EC2. It is what the seamless
// upgrade is built on, and it also
// serves recoveries and mints.
//
// It is optional, and hosted-only: it needs EC2, a pool table and a pause on
// the shard's gate. Nil — the open build, dev, docker — leaves UPGRADING as
// reap-and-relaunch and every launch on a fresh instance. The FSM sees only
// these verbs; the pool's rows, the phase facts it persists and the calls that
// pause and resume an instance are the implementation's.
type Standbys interface {
	// BeginSwap records that sh's volume is to move onto a standby. It runs
	// before the shard enters UPGRADING, so routing never sees an UPGRADING
	// shard without the facts it reads.
	BeginSwap(ctx context.Context, sh *Shard) error
	// EvaluateSwap drives the swap one step: claim a standby, pause the old
	// instance, move the volume, resume, or roll back. Resumable: every step
	// re-derives from the persisted phase, on any coordinator.
	EvaluateSwap(ctx context.Context, sh *Shard) error

	// ClaimFor claims a ready standby in (tier, az) for a launch of shardID,
	// nil when none is. A standby the shard already holds comes back as is.
	ClaimFor(ctx context.Context, shardID, tier, az string) (*Standby, error)

	// Reconcile drives the pool one step toward a heartbeating, current-image
	// standby for each desired pair. use says what a claimed standby is to
	// its path.
	Reconcile(ctx context.Context, desired map[StandbyPair]bool, use func(Standby) StandbyUse) error
	// ObserveHeartbeat records a standby's heartbeat. Any coordinator may
	// receive one; the pool's row is the shared record.
	ObserveHeartbeat(instanceID, addr, binarySHA string) error

	// fenced is the capability with every call guarded by the lease fence in
	// its context (see fence.go).
	fenced() Standbys
}

// StandbyService is the hosted Standbys: a pool of EC2 instances, the pause
// that hands a shard's traffic to its successor, and the store the swap's
// phase facts persist in.
type StandbyService struct {
	pool   *StandbyPool
	pauser ShardPauser
	facts  UpgradeStore
}

var _ Standbys = (*StandbyService)(nil)

// NewStandbyService builds the hosted capability over a pool, a pauser and the
// store of upgrade facts.
func NewStandbyService(pool *StandbyPool, pauser ShardPauser, facts UpgradeStore) *StandbyService {
	return &StandbyService{pool: pool, pauser: pauser, facts: facts}
}

func (s *StandbyService) BeginSwap(ctx context.Context, sh *Shard) error {
	sh.mu.RLock()
	origin, instanceID, addr := sh.State, sh.InstanceID, sh.Addr
	sh.mu.RUnlock()
	now := sh.deps.Clock()
	return sh.setUpgradeFacts(ctx, UpgradeFacts{
		Phase: PhaseClaiming, Origin: origin, PhaseAt: &now,
		DemotedInstanceID: instanceID, DemotedAddr: addr,
		TraceID: traceIDOf(ctx), ParentSpanID: spanIDOf(ctx),
	})
}

func (s *StandbyService) EvaluateSwap(ctx context.Context, sh *Shard) error {
	return sh.evaluateUpgrading(ctx)
}

func (s *StandbyService) ClaimFor(ctx context.Context, shardID, tier, az string) (*Standby, error) {
	return s.pool.Claim(ctx, shardID, tier, az)
}

func (s *StandbyService) Reconcile(ctx context.Context, desired map[StandbyPair]bool, use func(Standby) StandbyUse) error {
	return s.pool.Reconcile(ctx, desired, use)
}

func (s *StandbyService) ObserveHeartbeat(instanceID, addr, binarySHA string) error {
	return s.pool.ObserveHeartbeat(instanceID, addr, binarySHA)
}

func (s *StandbyService) fenced() Standbys {
	return &StandbyService{
		pool:   NewStandbyPool(s.pool.store, fencedStandbyLauncher{s.pool.ec2}, s.pool.clock),
		pauser: fencedPauser{s.pauser},
		facts:  s.facts,
	}
}

// standby is the hosted implementation behind d.Standbys: the swap's phase
// code is part of it and reaches the pool, the pauser and the facts store
// through it. Nil when no standbys are wired.
func (sh *Shard) standby() *StandbyService {
	s, _ := sh.deps.Standbys.(*StandbyService)
	return s
}
