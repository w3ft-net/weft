package logdb

import (
	"context"
	"time"

	"github.com/w3ft-net/weft/backends/shardstore"

	weftsdk "github.com/w3ft-net/weft-go"
)

// reconcileStandbysTimeout bounds one pool pass inside the leader's tick: a
// few EC2 calls, each normally well under a second. It is the longest
// thing the tick itself waits on, so it sets progressStaleFloor. A pass
// cut short finishes on a later tick.
const reconcileStandbysTimeout = 10 * time.Second

// SetStandbys wires the warm-standby capability
// (docs/plans/shard-seamless-upgrade.md). Nil — the open build, dev, docker,
// tests that don't exercise it — leaves the upgrade, recovery and mint paths on
// their launch-a-fresh-instance behavior.
func (p *Provisioner) SetStandbys(s Standbys) { p.standbys = s }

// ObserveStandbyHeartbeat records a standby's heartbeat. Any coordinator may
// receive one (the NLB doesn't route by leader); the row is shared.
func (p *Provisioner) observeStandbyHeartbeat(hb shardstore.ShardHeartbeat) {
	if p.standbys == nil {
		return
	}
	if err := p.standbys.ObserveHeartbeat(hb.InstanceID, hb.Addr, hb.BinarySHA); err != nil {
		weftsdk.Infof(context.Background(), "provisioner: spare heartbeat from %s: %v", hb.InstanceID, err)
	}
}

// reconcileStandbys runs one pool pass. Leader-only (from
// evaluateStateMachines): it launches and terminates instances.
func (p *Provisioner) reconcileStandbys(ctx context.Context) {
	if p.standbys == nil {
		return
	}
	desired, err := p.desiredStandbyPairs()
	if err != nil {
		weftsdk.Infof(ctx, "provisioner: spare pool desired set: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, reconcileStandbysTimeout)
	defer cancel()
	if err := p.standbys.Reconcile(ctx, desired, p.standbyUse); err != nil {
		weftsdk.Infof(ctx, "provisioner: spare pool: %v", err)
	}
}

// desiredStandbyPairs is one standby per (tier, AZ) some bucket stripes into —
// stripe i of a bucket lands in StripeAZ(tier, i) — plus every pair a live
// shard still occupies (an older shard after its bucket's Target shrank).
// A tier with no bucket and no shard costs nothing.
func (p *Provisioner) desiredStandbyPairs() (map[StandbyPair]bool, error) {
	striper, ok := p.launcher.(interface {
		StripeAZ(tier string, index int) string
	})
	if !ok {
		return map[StandbyPair]bool{}, nil
	}
	desired := map[StandbyPair]bool{}
	if p.Buckets != nil {
		buckets, err := p.Buckets.All()
		if err != nil {
			return nil, err
		}
		for _, b := range buckets {
			tier := b.Tier
			if tier == "" {
				tier = "small"
			}
			for i := 0; i < b.Target; i++ {
				if az := striper.StripeAZ(tier, i); az != "" {
					desired[StandbyPair{tier, az}] = true
				}
			}
		}
	}
	for _, e := range p.registry.Entries() {
		e.mu.RLock()
		tier, az, st := e.Tier, e.AZ, e.State
		e.mu.RUnlock()
		if az == "" || !holdsInstance(st) {
			continue
		}
		desired[StandbyPair{tier, az}] = true
	}
	return desired, nil
}

// holdsInstance reports whether a shard in st has (or is getting) an
// instance a recovery or an upgrade could move to a standby.
func holdsInstance(st ShardState) bool {
	switch st {
	case StateLaunching, StateIngesting, StateDegraded, StateReadOnly, StateSuspended, StateUpgrading:
		return true
	}
	return false
}

// standbyUse settles a claimed standby against its claimant's current facts.
func (p *Provisioner) standbyUse(sp Standby) StandbyUse {
	e := p.registry.Get(*sp.ClaimedBy)
	if e == nil {
		return StandbyAbandoned // the shard is gone
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	switch {
	case e.InstanceID == sp.InstanceID:
		return StandbyAdopted // the standby is the shard's instance now
	case e.State == StateGone:
		return StandbyAbandoned
	case e.State == StateLaunching:
		return StandbyInUse // a launch in flight may be attaching to it
	case e.State == StateUpgrading && (e.Upgrade.SuccessorInstanceID == sp.InstanceID || e.Upgrade.SuccessorInstanceID == ""):
		// Its successor — or claimed in the moment before the upgrade
		// recorded it as one.
		return StandbyInUse
	}
	return StandbyAbandoned
}
