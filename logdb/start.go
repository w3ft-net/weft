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

	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/store"
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

	// AWSRegion is threaded into NewASGLauncher / NewEC2VolumeManager.
	// Each loads its own AWS config in that region.
	AWSRegion string

	// ShardTiers maps tier name (small/medium/large) to the ASG name
	// that launches shards of that tier. Empty selects dev mode
	// (DockerLauncher) instead of production ASG mode.
	ShardTiers map[string]string
}

// Start hydrates the shard registry, constructs the Provisioner with
// the right launcher for the deploy mode (ASG when ShardTiers is
// non-empty, Docker otherwise), and starts it.
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

	asgs := parseShardASGs()
	if len(asgs) > 0 || len(cfg.ShardTiers) > 0 {
		return startProd(cfg, reg, lambdaURL, asgs)
	}
	return startDev(cfg, reg)
}

// parseShardASGs reads the per-AZ ASG topology from the environment —
// SHARD_SMALL_ASGS / SHARD_MEDIUM_ASGS / SHARD_LARGE_ASGS — each a
// comma-separated list of az=asg-name pairs, e.g.
//
//	SHARD_SMALL_ASGS="us-east-2a=weft-shard-small-2a,us-east-2b=weft-shard-small-2b"
//
// Returns tier→az→asg. Empty when no tier is configured, in which case
// startProd falls back to the legacy single-ASG-per-tier ShardLauncher (or
// Start picks dev mode). Malformed pairs are logged and skipped rather
// than failing boot.
func parseShardASGs() map[string]map[string]string {
	out := map[string]map[string]string{}
	for tier, env := range map[string]string{
		"small":  "SHARD_SMALL_ASGS",
		"medium": "SHARD_MEDIUM_ASGS",
		"large":  "SHARD_LARGE_ASGS",
	} {
		raw := os.Getenv(env)
		if raw == "" {
			continue
		}
		azMap := map[string]string{}
		for _, pair := range strings.Split(raw, ",") {
			az, asg, ok := strings.Cut(strings.TrimSpace(pair), "=")
			az, asg = strings.TrimSpace(az), strings.TrimSpace(asg)
			if !ok || az == "" || asg == "" {
				log.Printf("warning: %s: malformed pair %q (want az=asg), skipping", env, pair)
				continue
			}
			azMap[az] = asg
		}
		if len(azMap) > 0 {
			out[tier] = azMap
		}
	}
	return out
}

// startProd wires the launcher, provisioner, and volume manager for
// production deploys. When asgs (SHARD_<TIER>_ASGS) is present it uses the
// per-AZ ASGProvisioner — the real FSM launch primitive (Launch(spec)
// creates+attaches the EBS volume and the shard FSM drives it). Otherwise
// it falls back to the legacy single-ASG-per-tier ASGLauncher, which is
// NOT a ShardLaunchAPI, so the FSM's launch paths no-op (mint-but-don't-
// launch) until the per-AZ config lands — the pre-cutover state.
func startProd(cfg Config, reg *ShardRegistry, lambdaURL string, asgs map[string]map[string]string) (*Provisioner, error) {
	// One VolumeManager, shared: the ASGProvisioner creates/attaches volumes
	// on launch, and the provisioner's shard SM retires them on archive.
	vm, vmErr := NewEC2VolumeManager(cfg.AWSRegion)
	if vmErr != nil {
		log.Printf("warning: VolumeManager unavailable: %v", vmErr)
	}

	var launcher ShardLauncher
	if len(asgs) > 0 {
		if vmErr != nil {
			return nil, fmt.Errorf("ASGProvisioner needs a VolumeManager (per-AZ launch creates/attaches volumes): %w", vmErr)
		}
		ap, err := NewASGProvisioner(cfg.AWSRegion, asgs, vm)
		if err != nil {
			return nil, fmt.Errorf("ASG provisioner: %w", err)
		}
		launcher = ap
		log.Printf("provisioner: ASGProvisioner (per-AZ) for %d tier(s): %v", len(asgs), asgs)
	} else {
		asgLauncher, err := NewASGLauncher(cfg.AWSRegion, cfg.ShardTiers, 9000)
		if err != nil {
			// Non-fatal historically: dev environments without AWS creds
			// hit this and degrade gracefully. Preserve that.
			log.Printf("warning: ASG launcher unavailable: %v", err)
			return nil, nil
		}
		launcher = asgLauncher
		log.Printf("provisioner: legacy ASGLauncher for tiers %v — set SHARD_<TIER>_ASGS to enable per-AZ launching", cfg.ShardTiers)
	}

	provConfig := DefaultProvisionerConfig()
	provConfig.ShardSecret = cfg.ShardSecret
	provConfig.ParquetDir = os.Getenv("PARQUET_DIR")
	provConfig.LambdaFunctionURL = lambdaURL
	prov := NewProvisioner(reg, launcher, provConfig)
	if vmErr == nil {
		prov.SetVolumeManager(vm)
	}

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
