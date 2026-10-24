package logdb

import (
	"context"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/graybeardcorp/weft/store"
)

// reconcileSparesTimeout bounds one pool pass inside the leader's tick: a
// few EC2 calls, each normally well under a second. It is the longest
// thing the tick itself waits on, so it sets progressStaleFloor. A pass
// cut short finishes on a later tick.
const reconcileSparesTimeout = 10 * time.Second

// SetSparePool wires the warm pool (docs/plans/shard-seamless-upgrade.md).
// Nil — dev, docker, tests that don't exercise it — leaves the upgrade,
// recovery and mint paths on their launch-a-fresh-instance behavior.
func (p *Provisioner) SetSparePool(pool *SparePool) { p.spares = pool }

// ObserveSpareHeartbeat records a spare's heartbeat. Any coordinator may
// receive one (the NLB doesn't route by leader); the row is shared.
func (p *Provisioner) observeSpareHeartbeat(hb store.ShardHeartbeat) {
	if p.spares == nil {
		return
	}
	if err := p.spares.ObserveHeartbeat(hb.InstanceID, hb.Addr, hb.BinarySHA); err != nil {
		weftsdk.Infof(context.Background(), "provisioner: spare heartbeat from %s: %v", hb.InstanceID, err)
	}
}

// reconcileSpares runs one pool pass. Leader-only (from
// evaluateStateMachines): it launches and terminates instances.
func (p *Provisioner) reconcileSpares(ctx context.Context) {
	if p.spares == nil {
		return
	}
	desired, err := p.desiredSparePairs()
	if err != nil {
		weftsdk.Infof(ctx, "provisioner: spare pool desired set: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, reconcileSparesTimeout)
	defer cancel()
	if err := p.spares.Reconcile(ctx, desired, p.spareUse); err != nil {
		weftsdk.Infof(ctx, "provisioner: spare pool: %v", err)
	}
}

// desiredSparePairs is one spare per (tier, AZ) some bucket stripes into —
// stripe i of a bucket lands in StripeAZ(tier, i) — plus every pair a live
// shard still occupies (an older shard after its bucket's Target shrank).
// A tier with no bucket and no shard costs nothing.
func (p *Provisioner) desiredSparePairs() (map[SparePair]bool, error) {
	striper, ok := p.launcher.(interface {
		StripeAZ(tier string, index int) string
	})
	if !ok {
		return map[SparePair]bool{}, nil
	}
	desired := map[SparePair]bool{}
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
					desired[SparePair{tier, az}] = true
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
		desired[SparePair{tier, az}] = true
	}
	return desired, nil
}

// holdsInstance reports whether a shard in st has (or is getting) an
// instance a recovery or an upgrade could move to a spare.
func holdsInstance(st ShardState) bool {
	switch st {
	case StateLaunching, StateIngesting, StateDegraded, StateReadOnly, StateSuspended, StateUpgrading:
		return true
	}
	return false
}

// spareUse settles a claimed spare against its claimant's current facts.
func (p *Provisioner) spareUse(sp Spare) SpareUse {
	e := p.registry.Get(*sp.ClaimedBy)
	if e == nil {
		return SpareAbandoned // the shard is gone
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	switch {
	case e.InstanceID == sp.InstanceID:
		return SpareAdopted // the spare is the shard's instance now
	case e.State == StateGone:
		return SpareAbandoned
	case e.State == StateLaunching:
		return SpareInUse // a launch in flight may be attaching to it
	case e.State == StateUpgrading && (e.UpgradeSuccessorInstanceID == sp.InstanceID || e.UpgradeSuccessorInstanceID == ""):
		// Its successor — or claimed in the moment before the upgrade
		// recorded it as one.
		return SpareInUse
	}
	return SpareAbandoned
}
