// Package logdb is the shard-data-plane control plane. It owns shard
// lifecycle (launch, christen, archive, recovery) via per-shard and
// per-bucket state machines, the Registry, the Platform it runs shards on,
// and the per-app bucket layout.
//
// The runtime side of the coordinator (query routing, ingest fan-out,
// drain) reads from the Registry but does not drive its state machine
// — that job lives here. Dependency arrow is coordinator → logdb;
// nothing in logdb knows about queries.
package logdb

import (
	"context"
	"fmt"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/w3ft-net/weft/backends/coordination"
	"github.com/w3ft-net/weft/shard"
)

// Config carries the runtime-derived inputs Start needs from the
// coordinator process. Where shards run is the Platform's.
type Config struct {
	// Ctx scopes the long-running goroutines Start spawns (the
	// Provisioner's FSM convergence loop). Cancel it to shut the control
	// plane down.
	Ctx context.Context

	// Shards is the shard-state store: hydration, lifecycle persistence,
	// buckets and heartbeats. The hosted build supplies logdb/shardpg, the
	// open build defaults. Nil: nothing is persisted.
	Shards ShardStore

	// Services is the sources registry the provisioner reads a bucket's
	// services from. Nil: no bucket has any.
	Services ServiceDirectory

	// Standbys is the warm pool's rows. Nil: no warm pool. The pool persists
	// upgrade facts through Shards.
	Standbys StandbyStore

	// ShardSecret is the SHARD_SECRET bearer for outbound coord→shard
	// gRPC dials made by the Provisioner (heartbeat-adopt, rebind)
	// and by ReconstructShard during boot. The query path's secret
	// lives in compute, not here — see compute.Engine.
	ShardSecret string

	// Holder identifies this process for the "shard-fsm-leader" lease
	// (ProvisionerConfig.Holder) — the same holder id main.go's
	// coordinatorHolder() mints for its leases. Empty
	// disables the election: the Provisioner stays leader unconditionally,
	// the single-process behavior.
	Holder string

	// Leases grants that lease (ProvisionerConfig.Leases). Nil: no election.
	Leases coordination.Leases

	// Retention is the optional retention feature: told of each archive's
	// stats and started with the provisioner. Nil: archives are kept forever.
	Retention Retention

	// LegacyFactTable names the table of a field fact that identifies its
	// source by customer, from a shard that predates table names or from the
	// store. Nil: such facts are unknown.
	LegacyFactTable LegacyFactTable
}

// wireStore hands the provisioner its stores.
func (cfg Config) wireStore(prov *Provisioner) {
	prov.SetServices(cfg.Services)
	if cfg.Shards != nil {
		prov.SetShardStore(cfg.Shards)
	}
}

// Start hydrates the shard registry, constructs the Provisioner over the
// platform's launcher, and starts it.
//
// Returns the started Provisioner. Caller assigns it to
// Coordinator.Provisioner so the rest of the runtime
// (drain stats, OnboardService, ProcessHeartbeat, ForceArchive/Delete)
// can reach it.
func Start(cfg Config, plat Platform) (*Provisioner, error) {
	if plat.Launcher == nil {
		return nil, fmt.Errorf("logdb: the platform has no launcher")
	}
	// Hydrate the shard registry from the store BEFORE handing it to the
	// provisioner so LoadStateMachines builds an SM per persisted row and
	// the first tick converges from real state rather than an empty world.
	reg := NewShardRegistry()
	if cfg.Shards != nil {
		reg.SetStore(cfg.Shards)
	}
	reg.SetLegacyFactTable(cfg.LegacyFactTable)
	reconstructCfg := ReconstructConfig{
		ShardSecret:       cfg.ShardSecret,
		LambdaFunctionURL: plat.Config.LambdaFunctionURL,
	}
	if err := reg.LoadFromStore(func(sh *Shard) (shard.Shard, error) {
		return ReconstructShard(sh, reconstructCfg)
	}); err != nil {
		weftsdk.Warnf(context.Background(), "warning: load shards from store: %v", err)
	} else {
		weftsdk.Infof(context.Background(), "coordinator: restored shards from store")
	}

	provConfig := plat.Config
	provConfig.ShardSecret = cfg.ShardSecret
	provConfig.Holder = cfg.Holder
	provConfig.Leases = cfg.Leases
	provConfig.Retention = cfg.Retention
	prov := NewProvisioner(reg, plat.Launcher, provConfig)
	if plat.Volumes != nil {
		prov.SetVolumeManager(plat.Volumes)
	}
	cfg.wireStore(prov)

	// The warm pool of volume-less standbys that upgrades, recoveries and
	// mints claim instead of waiting on the platform (docs/plans/
	// shard-seamless-upgrade.md). Standbys run the coordinator's own image.
	if plat.Standby != nil && cfg.Shards != nil && cfg.Standbys != nil {
		pool := NewStandbyPool(cfg.Standbys, plat.Standby, nil)
		standbys := NewStandbyService(pool, grpcPauser{shardSecret: cfg.ShardSecret}, cfg.Shards)
		if sa, ok := plat.Launcher.(interface{ SetStandbys(Standbys) }); ok {
			sa.SetStandbys(standbys)
		}
		prov.SetStandbys(standbys)
	}

	// Build the FSM control plane (one SM per persisted bucket + shard).
	// The tick drives these — they own all lifecycle convergence.
	prov.LoadStateMachines()

	prov.Start(cfg.Ctx)
	if cfg.Retention != nil {
		cfg.Retention.Start(cfg.Ctx, prov)
	}
	startArchiveSweeper(cfg.Ctx, prov)
	return prov, nil
}

// startArchiveSweeper starts the loop that removes orphaned export resources and
// logs an ALERT for shards stuck in ARCHIVING. It always runs in prod: the
// stuck-archive alert applies to in-process archives too. Orphan removal needs
// the export wired; without it only the alert runs.
func startArchiveSweeper(ctx context.Context, prov *Provisioner) {
	sw := &ArchiveSweeper{
		Store:    prov.shards,
		Clock:    time.Now,
		IsLeader: prov.IsLeader,
	}
	if lister, ok := prov.config.Exports.(ExportLister); ok {
		sw.Resources = lister
	}
	sw.Start(ctx)
	weftsdk.Infof(ctx, "coordinator: archive sweeper started (orphan removal=%v, stuck-archive alert after %s)", sw.Resources != nil, archivingStuckAfter)
}
