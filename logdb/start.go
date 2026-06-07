// Package logdb is the shard-data-plane control plane. It owns shard
// lifecycle (launch, christen, archive, recovery) via per-shard and
// per-bucket state machines, the Registry, the launchers (ASG in prod,
// Docker in dev), and the per-app bucket layout.
//
// The runtime side of the coordinator (query routing, ingest fan-out,
// drain) reads from the Registry but does not drive its state machine
// — that job lives here. Dependency arrow is coordinator → logdb;
// nothing in logdb knows about queries.
package logdb

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/graybeardcorp/weft/shard"
	"github.com/graybeardcorp/weft/store"
)

// Config carries the runtime-derived inputs Start needs from the
// coordinator process. Everything else (LAMBDA_FUNCTION_URL,
// PARQUET_DIR, WEFT_* docker launcher vars) is read from this process's
// environment inside Start — those values are pure deployment policy
// that the coordinator's main has no reason to know about.
type Config struct {
	// Ctx scopes the long-running goroutines Start spawns (the
	// Provisioner's FSM convergence loop). Cancel it to shut the control
	// plane down.
	Ctx context.Context

	// DB is the postgres-backed shard store. Used for hydration,
	// lifecycle persistence, app-bucket assignment, and the
	// lifecycle handler's ShardOps.
	DB *store.Store

	// ShardSecret is the SHARD_SECRET bearer for outbound coord→shard
	// gRPC dials made by the Provisioner (heartbeat-adopt, rebind)
	// and by ReconstructShard during boot. The query path's secret
	// lives in compute, not here — see compute.LocalComputePool.
	ShardSecret string

	// AWSRegion is threaded into NewEC2Provisioner / NewEC2VolumeManager.
	// Each loads its own AWS config in that region.
	AWSRegion string
}

// Start hydrates the shard registry, constructs the Provisioner with
// the right launcher for the deploy mode (EC2-direct when
// SHARD_<TIER>_SUBNETS is configured, Docker otherwise), and starts it.
//
// Returns the started Provisioner. Caller assigns it to
// Coordinator.Provisioner so the rest of the runtime
// (drain stats, OnboardService, ProcessHeartbeat, ForceArchive/Delete)
// can reach it.
func Start(cfg Config) (*Provisioner, error) {
	// Hydrate the shard registry from postgres BEFORE handing it to the
	// provisioner so LoadStateMachines builds an SM per persisted row and
	// the first tick converges from real state rather than an empty world.
	reg := NewShardRegistry()
	reg.SetStore(ShardStore{cfg.DB})
	lambdaURL := os.Getenv("LAMBDA_FUNCTION_URL")
	reconstructCfg := ReconstructConfig{
		ShardSecret:       cfg.ShardSecret,
		LambdaFunctionURL: lambdaURL,
	}
	if err := reg.LoadFromStore(func(sh *Shard) (shard.Shard, error) {
		return ReconstructShard(sh, reconstructCfg)
	}); err != nil {
		log.Printf("warning: load shards from store: %v", err)
	} else {
		log.Printf("coordinator: restored shards from store")
	}

	subnets := parseShardSubnets()
	if len(subnets) > 0 {
		return startProd(cfg, reg, lambdaURL, subnets, parseShardTemplates())
	}
	return startDev(cfg, reg)
}

// parseShardSubnets reads the per-AZ subnet topology from the environment —
// SHARD_SMALL_SUBNETS / SHARD_MEDIUM_SUBNETS / SHARD_LARGE_SUBNETS — each a
// comma-separated list of az=subnet-id pairs, e.g.
//
//	SHARD_SMALL_SUBNETS="us-east-2a=subnet-0aa,us-east-2b=subnet-0bb"
//
// Returns tier→az→subnet. Empty when no tier is configured, in which case
// Start picks dev mode. Malformed pairs are logged and skipped rather than
// failing boot. (Replaces the old SHARD_<TIER>_ASGS — the coordinator now
// launches instances directly, not via an Auto Scaling Group.)
func parseShardSubnets() map[string]map[string]string {
	out := map[string]map[string]string{}
	for tier, env := range map[string]string{
		"small":  "SHARD_SMALL_SUBNETS",
		"medium": "SHARD_MEDIUM_SUBNETS",
		"large":  "SHARD_LARGE_SUBNETS",
	} {
		raw := os.Getenv(env)
		if raw == "" {
			continue
		}
		azMap := map[string]string{}
		for _, pair := range strings.Split(raw, ",") {
			az, subnet, ok := strings.Cut(strings.TrimSpace(pair), "=")
			az, subnet = strings.TrimSpace(az), strings.TrimSpace(subnet)
			if !ok || az == "" || subnet == "" {
				log.Printf("warning: %s: malformed pair %q (want az=subnet), skipping", env, pair)
				continue
			}
			azMap[az] = subnet
		}
		if len(azMap) > 0 {
			out[tier] = azMap
		}
	}
	return out
}

// parseShardTemplates reads the per-tier EC2 launch template ids from the
// environment — SHARD_SMALL_LAUNCH_TEMPLATE / SHARD_MEDIUM_LAUNCH_TEMPLATE /
// SHARD_LARGE_LAUNCH_TEMPLATE — each a single launch-template id, e.g.
//
//	SHARD_SMALL_LAUNCH_TEMPLATE="lt-0abc123"
//
// Returns tier→template-id for the tiers that set one. A tier present in
// SHARD_<TIER>_SUBNETS but missing a template here will fail its first
// launch with a clear "no launch template configured" error.
func parseShardTemplates() map[string]string {
	out := map[string]string{}
	for tier, env := range map[string]string{
		"small":  "SHARD_SMALL_LAUNCH_TEMPLATE",
		"medium": "SHARD_MEDIUM_LAUNCH_TEMPLATE",
		"large":  "SHARD_LARGE_LAUNCH_TEMPLATE",
	} {
		if id := strings.TrimSpace(os.Getenv(env)); id != "" {
			out[tier] = id
		}
	}
	return out
}

// startProd wires the EC2Provisioner, provisioner, and volume manager for
// production deploys. The coordinator launches shard instances directly via
// RunInstances/TerminateInstances (no Auto Scaling Group) — it is the sole
// capacity authority, so a shard is backed by exactly one instance and there
// is no "ghost" failure mode. subnets is tier→az→subnet; templates is
// tier→launch-template-id.
func startProd(cfg Config, reg *ShardRegistry, lambdaURL string, subnets map[string]map[string]string, templates map[string]string) (*Provisioner, error) {
	// One VolumeManager, shared: the EC2Provisioner creates/attaches volumes
	// on launch, and the provisioner's shard SM retires them on archive.
	vm, vmErr := NewEC2VolumeManager(cfg.AWSRegion)
	if vmErr != nil {
		return nil, fmt.Errorf("EC2Provisioner needs a VolumeManager (launch creates/attaches volumes): %w", vmErr)
	}

	ep, err := NewEC2Provisioner(cfg.AWSRegion, subnets, templates, vm)
	if err != nil {
		return nil, fmt.Errorf("EC2 provisioner: %w", err)
	}
	var launcher ShardLauncher = ep
	log.Printf("provisioner: EC2Provisioner (direct, per-AZ) for %d tier(s): subnets=%v templates=%v", len(subnets), subnets, templates)

	provConfig := DefaultProvisionerConfig()
	provConfig.ShardSecret = cfg.ShardSecret
	provConfig.ParquetDir = os.Getenv("PARQUET_DIR")
	provConfig.LambdaFunctionURL = lambdaURL
	prov := NewProvisioner(reg, launcher, provConfig)
	prov.SetVolumeManager(vm)

	prov.SetStore(cfg.DB)

	// Build the FSM control plane (one SM per persisted bucket + shard).
	// The tick drives these — they own all lifecycle convergence.
	prov.LoadStateMachines()

	prov.Start(cfg.Ctx)
	return prov, nil
}

// startDev wires the docker-based dev launcher and provisioner. Fatal
// on missing required env (no in-process fallback in this branch — the
// operator gets a precise error rather than silent data loss).
func startDev(cfg Config, reg *ShardRegistry) (*Provisioner, error) {
	launcher := buildDockerLauncher(cfg.ShardSecret)
	provConfig := DevProvisionerConfig()
	provConfig.ShardSecret = cfg.ShardSecret
	provConfig.ParquetDir = os.Getenv("PARQUET_DIR")
	prov := NewProvisioner(reg, launcher, provConfig)

	prov.SetStore(cfg.DB)

	// Build the FSM control plane so restored shards get an SM (the tick
	// and ProcessHeartbeat both resolve shards through the SM maps).
	prov.LoadStateMachines()

	prov.Start(cfg.Ctx)
	log.Printf("provisioner: started with docker launcher (dev mode), image=%s data=%s",
		launcher.Image, launcher.HostDataRoot)
	return prov, nil
}

// buildDockerLauncher constructs a DockerLauncher from env vars set
// on the coordinator container by docker-compose. Fatal on missing/
// empty required vars — there's no in-process fallback in this
// branch, so the operator gets a precise error rather than silent
// data loss.
//
// Required env vars:
//
//	WEFT_SHARD_IMAGE      — the image tag to spawn (typically the
//	                         same image the coordinator runs from)
//	WEFT_DOCKER_NETWORK   — the docker network name from compose so
//	                         the spawned shard can resolve
//	                         "coordinator" by service DNS
//	WEFT_HOST_DATA_ROOT   — host-side absolute path bind-mounted to
//	                         WEFT_LOCAL_DATA_ROOT inside both the
//	                         coordinator and each spawned shard
//	WEFT_LOCAL_DATA_ROOT  — same volume's path inside the
//	                         coordinator container; used to mkdir
//	                         per-shard subdirs
//	WEFT_COORDINATOR_ADDR — the address shards send heartbeats to;
//	                         typically "coordinator:9020"
func buildDockerLauncher(shardSecret string) *DockerLauncher {
	requireEnv := func(name string) string {
		v := os.Getenv(name)
		if v == "" {
			log.Fatalf("docker launcher: %s is required (no in-process fallback in dev mode)", name)
		}
		return v
	}
	return &DockerLauncher{
		Image:             requireEnv("WEFT_SHARD_IMAGE"),
		Network:           requireEnv("WEFT_DOCKER_NETWORK"),
		HostDataRoot:      requireEnv("WEFT_HOST_DATA_ROOT"),
		LocalDataRoot:     requireEnv("WEFT_LOCAL_DATA_ROOT"),
		CoordinatorAddr:   requireEnv("WEFT_COORDINATOR_ADDR"),
		HeartbeatInterval: 10 * time.Second,
		ShardSecret:       shardSecret,
	}
}
