package logdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"time"

	"github.com/kevlarcon/weft/store"
)

// This file is the bucket SM ↔ provisioner seam (Slice C step 1 of
// docs/plans/asg-as-api-stateless.md). It is built *additively*: the SM
// maps and the ShardController implementation exist, but check() still
// runs the old reconciler and ProcessHeartbeat still christens — the
// switch to FSM-driven convergence is the next commit. Nothing here is
// reachable from the live tick yet; MintShard/FireShard run only when a
// bucket SM's Evaluate drives them, which the unit tests do via a fake
// controller and the switch commit will do for real.

// LoadStateMachines builds one ShardSM per registered shard and one
// BucketSM per bucket, after the registry has hydrated from the store.
// Called once at boot (startProd/startDev) so the maps are populated and
// ready for the Slice C switch; running it has no effect on the live
// reconciler. Safe to re-run — it rebuilds both maps from scratch.
func (p *Provisioner) LoadStateMachines() {
	p.smMu.Lock()
	defer p.smMu.Unlock()

	p.shardSMs = map[string]*ShardSM{}
	for _, e := range p.registry.Entries() {
		p.shardSMs[e.ID] = NewShardSM(e, p.shardSMDeps())
	}

	p.bucketSMs = map[int64]*BucketSM{}
	if p.Buckets == nil || p.store == nil {
		return
	}
	all, err := p.Buckets.All()
	if err != nil {
		log.Printf("provisioner: LoadStateMachines: Buckets.All: %v", err)
		return
	}
	for _, bk := range all {
		p.bucketSMs[bk.ID] = NewBucketSM(bk, p.store.DB(), p, nil)
	}
	log.Printf("provisioner: built %d bucket SMs + %d shard SMs", len(p.bucketSMs), len(p.shardSMs))
}

// shardSMDeps assembles the dependency bundle every ShardSM shares. The
// launcher is wired only when it satisfies ShardLaunchAPI (the new
// ASGProvisioner) — the legacy ShardLauncher doesn't, so Launcher stays
// nil until the launcher cutover, and the shard SM's launch paths no-op
// meanwhile (evaluateLaunching guards on a nil launcher).
func (p *Provisioner) shardSMDeps() ShardSMDeps {
	deps := ShardSMDeps{
		Store:             ShardStore{p.store},
		Registry:          p.registry,
		Volumes:           p.volumes,
		Configurer:        p.configurer,
		ParquetDir:        p.config.ParquetDir,
		LambdaFunctionURL: p.config.LambdaFunctionURL,
	}
	if api, ok := p.launcher.(ShardLaunchAPI); ok {
		deps.Launcher = api
	}
	return deps
}

// MintShard is the bucket SM's factory hook (ShardController). It INSERTs
// a LAUNCHING shard_records row, registers the in-memory entry, narrow-
// writes the FSM-owned AZ + stripe columns, stamps shard_services so read
// routing finds it, and creates the ShardSM. The new SM hydrates into
// LAUNCHING from the row it just wrote; the actual EC2 launch is driven
// by that shard's Evaluate, not fired here (the lean-Evaluate model the
// shard SM already uses).
//
// AZ is resolved from the stripe index via the launcher's striper when
// one is wired; the bucket SM holds no AZ knowledge of its own.
func (p *Provisioner) MintShard(ctx context.Context, spec MintSpec) (string, error) {
	if p.store == nil {
		return "", fmt.Errorf("MintShard: no store configured")
	}
	tier := spec.Tier
	if tier == "" {
		tier = "small"
	}

	az := ""
	if striper, ok := p.launcher.(interface {
		StripeAZ(tier string, index int) string
	}); ok {
		az = striper.StripeAZ(tier, spec.StripeIndex)
	}

	suf := make([]byte, 4)
	if _, err := rand.Read(suf); err != nil {
		return "", fmt.Errorf("mint shard id suffix: %w", err)
	}
	shardID := fmt.Sprintf("shard-%s-%s", spec.WindowStart.Format("20060102T1504"), hex.EncodeToString(suf))

	token, err := store.GenerateShardHeartbeatToken()
	if err != nil {
		return "", fmt.Errorf("mint heartbeat token for %s: %w", shardID, err)
	}

	stripe := spec.StripeIndex
	now := time.Now().UTC()
	entry := &Shard{
		ID:             shardID,
		WindowStart:    spec.WindowStart,
		WindowEnd:      spec.WindowEnd,
		State:          StateLaunching,
		Tier:           tier,
		CreatedAt:      now,
		BucketID:       spec.BucketID,
		HeartbeatToken: token,
		AZ:             az,
		StripeIndex:    &stripe,
	}
	// Add persists the whole entry via a full Save — including AZ + stripe
	// — so unlike the pre-collapse path there's no separate narrow
	// SetShardLaunchResult needed to restore the columns the old field-
	// omitting persist would have wiped.
	p.registry.Add(entry)
	if err := p.store.StampShardServices(shardID, spec.BucketID); err != nil {
		log.Printf("provisioner: MintShard %s: stamp shard_services: %v", shardID, err)
	}

	sm := NewShardSM(entry, p.shardSMDeps())
	p.smMu.Lock()
	p.shardSMs[shardID] = sm
	p.smMu.Unlock()

	log.Printf("provisioner: minted LAUNCHING shard %s for bucket %d (tier=%s stripe=%d az=%q window %s–%s)",
		shardID, spec.BucketID, tier, stripe, az,
		spec.WindowStart.Format(time.RFC3339), spec.WindowEnd.Format(time.RFC3339))
	return shardID, nil
}

// FireShard dispatches a trigger to an existing shard's SM, resolving it
// by id (ShardController). A missing SM is an error the bucket SM logs —
// a shard that vanished mid-cascade is self-healed by the next tick's
// gate re-check rather than blocking the cascade.
func (p *Provisioner) FireShard(ctx context.Context, shardID string, trigger Trigger, args ...any) error {
	p.smMu.RLock()
	sm, ok := p.shardSMs[shardID]
	p.smMu.RUnlock()
	if !ok {
		return fmt.Errorf("FireShard: no SM for shard %s", shardID)
	}
	return sm.Fire(ctx, trigger, args...)
}
