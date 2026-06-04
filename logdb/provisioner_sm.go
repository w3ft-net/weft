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

// LoadStateMachines binds the FSM machinery onto every registered shard
// (the registry's entries map IS the shard-SM set now — no separate map)
// and builds one BucketSM per bucket, after the registry has hydrated
// from the store. Called once at boot (startProd/startDev). Safe to
// re-run — ensureSM is idempotent per shard and the bucket map is rebuilt
// from scratch.
func (p *Provisioner) LoadStateMachines() {
	deps := p.shardDeps()
	for _, e := range p.registry.Entries() {
		e.ensureSM(deps)
	}

	p.smMu.Lock()
	defer p.smMu.Unlock()
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
		p.bucketSMs[bk.ID] = NewBucketSM(bk, p.store.DB(), p, p.clock)
	}
	log.Printf("provisioner: built %d bucket SMs + %d shard SMs", len(p.bucketSMs), len(p.registry.Entries()))
}

// shardDeps returns the one ShardDeps bundle every shard references by
// pointer. Built lazily and cached (sync.Once): the collaborators
// (store, volumes, configurer, launcher) are all set at boot before the
// first shard is minted or driven, so a single shared instance is
// correct. The launcher is wired only when it satisfies ShardLaunchAPI
// (the EC2Provisioner) — a bare ShardLauncher (e.g. the dev DockerLauncher
// also satisfies it; a future stub might not) leaves Launcher nil and the
// launch paths no-op (evaluateLaunching guards on it).
func (p *Provisioner) shardDeps() *ShardDeps {
	p.depsOnce.Do(func() {
		d := ShardDeps{
			Store:             ShardStore{p.store},
			Registry:          p.registry,
			Volumes:           p.volumes,
			Configurer:        p.configurer,
			ParquetDir:        p.config.ParquetDir,
			LambdaFunctionURL: p.config.LambdaFunctionURL,
			Clock:             p.clock,
		}
		if api, ok := p.launcher.(ShardLaunchAPI); ok {
			d.Launcher = api
		}
		p.sharedDeps = newShardDeps(d)
	})
	return p.sharedDeps
}

// MintShard is the bucket SM's factory hook (ShardController). It INSERTs
// a LAUNCHING shard_records row, registers the in-memory entry, narrow-
// writes the FSM-owned AZ + stripe columns, stamps shard_services so read
// routing finds it, and binds its SM. The shard hydrates into
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
	now := p.nowUTC()
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

	// Bind the FSM onto the entry — the registry map is the SM set, so
	// there's no separate registration. The shard hydrates into LAUNCHING
	// from the row just written; its Evaluate drives the EC2 launch.
	entry.ensureSM(p.shardDeps())

	log.Printf("provisioner: minted LAUNCHING shard %s for bucket %d (tier=%s stripe=%d az=%q window %s–%s)",
		shardID, spec.BucketID, tier, stripe, az,
		spec.WindowStart.Format(time.RFC3339), spec.WindowEnd.Format(time.RFC3339))
	return shardID, nil
}

// FireShard dispatches a trigger to an existing shard, resolving it by id
// through the registry (ShardController). A missing entry is an error the
// bucket SM logs — a shard that vanished mid-cascade is self-healed by the
// next tick's gate re-check rather than blocking the cascade.
func (p *Provisioner) FireShard(ctx context.Context, shardID string, trigger Trigger, args ...any) error {
	e := p.registry.Get(shardID)
	if e == nil {
		return fmt.Errorf("FireShard: no shard %s", shardID)
	}
	e.ensureSM(p.shardDeps())
	return e.Fire(ctx, trigger, args...)
}
