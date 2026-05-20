// Package logdb is the shard-data-plane control plane. It owns shard
// lifecycle (launch, christen, archive, reap), the Registry's
// state-machine transitions, the launchers (ASG in prod, Docker in
// dev), the lifecycle-hook poller, and the per-app bucket layout.
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
// PARQUET_DIR, SHARD_VOLUME_DEVICE, LIFECYCLE_QUEUE_URL, per-tier
// volume sizes, WEFT_* docker launcher vars) is read from this
// process's environment inside Start — those values are pure
// deployment policy that the coordinator's main has no reason to
// know about.
type Config struct {
	// Ctx scopes the long-running goroutines Start spawns
	// (Provisioner reconciler, LifecyclePoller SQS loop). Cancel it
	// to shut the control plane down.
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

	// AWSRegion is threaded into NewASGLauncher / NewEC2VolumeManager
	// / BuildLifecyclePoller. Each loads its own AWS config in that
	// region.
	AWSRegion string

	// ShardTiers maps tier name (small/medium/large) to the ASG name
	// that launches shards of that tier. Empty selects dev mode
	// (DockerLauncher) instead of production ASG mode.
	ShardTiers map[string]string
}

// Start hydrates the shard registry, constructs the Provisioner with
// the right launcher for the deploy mode (ASG when ShardTiers is
// non-empty, Docker otherwise), starts it, and — in ASG mode — wires
// up the lifecycle poller when LIFECYCLE_QUEUE_URL is set.
//
// Returns the started Provisioner. Caller assigns it to
// Coordinator.Provisioner so the rest of the runtime
// (drain stats, OnboardApp, ProcessHeartbeat, ForceArchive/Delete)
// can reach it.
func Start(cfg Config) (*Provisioner, error) {
	// Hydrate the shard registry from postgres BEFORE handing it to
	// the provisioner. The lifecycle handler's recognisedAsDead
	// checks the registry to decide whether an orphan EBS volume
	// should be recovered; an empty registry causes every volume to
	// be abandoned, orphaning data on every deploy. The store-
	// fallback in recognisedAsDead is defense in depth; populating
	// the registry first is the primary fix.
	reg := NewShardRegistry()
	reg.SetStore(cfg.DB)
	lambdaURL := os.Getenv("LAMBDA_FUNCTION_URL")
	reconstructCfg := ReconstructConfig{
		ShardSecret:       cfg.ShardSecret,
		LambdaFunctionURL: lambdaURL,
	}
	if err := reg.LoadFromStore(func(rec store.ShardRecord) (shard.Shard, error) {
		return ReconstructShard(rec, reconstructCfg)
	}); err != nil {
		log.Printf("warning: load shards from store: %v", err)
	} else {
		log.Printf("coordinator: restored shards from store")
	}

	if len(cfg.ShardTiers) > 0 {
		return startProd(cfg, reg, lambdaURL)
	}
	return startDev(cfg, reg)
}

// startProd wires the ASG launcher, provisioner, volume manager, and
// (optionally) lifecycle poller for production deploys.
func startProd(cfg Config, reg *ShardRegistry, lambdaURL string) (*Provisioner, error) {
	asgLauncher, err := NewASGLauncher(cfg.AWSRegion, cfg.ShardTiers, 9000)
	if err != nil {
		// Non-fatal historically: dev environments without AWS creds
		// hit this and degrade gracefully. Preserve that.
		log.Printf("warning: ASG launcher unavailable: %v", err)
		return nil, nil
	}

	provConfig := DefaultProvisionerConfig()
	provConfig.ShardSecret = cfg.ShardSecret
	provConfig.ParquetDir = os.Getenv("PARQUET_DIR")
	provConfig.LambdaFunctionURL = lambdaURL
	prov := NewProvisioner(reg, asgLauncher, provConfig)

	// Attach a VolumeManager so christen() can tag EBS volumes with
	// the minted shard id. Unavailable when the EC2 SDK can't load
	// (warning only — the rest of the provisioner still functions).
	if vm, err := NewEC2VolumeManager(cfg.AWSRegion); err != nil {
		log.Printf("warning: VolumeManager unavailable: %v", err)
	} else {
		prov.SetVolumeManager(vm)
	}

	prov.SetStore(cfg.DB)
	// No-cache: SetStore on the bucket manager is all that's needed;
	// every lookup hits the DB.
	prov.Buckets.SetStore(cfg.DB)

	prov.Start(cfg.Ctx)
	log.Printf("provisioner: started with ASG tiers %v", cfg.ShardTiers)

	// Lifecycle poller: consume ASG launch hook events from SQS and
	// decide christen/recover for each new shard instance.
	// Controlled by LIFECYCLE_QUEUE_URL so dev/docker coordinators
	// stay out of this path.
	if queueURL := os.Getenv("LIFECYCLE_QUEUE_URL"); queueURL != "" {
		tierByASG := make(map[string]string, len(cfg.ShardTiers))
		for tier, asgName := range cfg.ShardTiers {
			tierByASG[asgName] = tier
		}
		lcCfg := LifecycleHandlerConfig{
			Registry:   reg,
			TierByASG:  tierByASG,
			SizeByTier: readTierSizes(),
			DeviceName: os.Getenv("SHARD_VOLUME_DEVICE"),
			Store:      cfg.DB,
			ShardOps:   cfg.DB,
		}
		poller, err := BuildLifecyclePoller(cfg.Ctx, cfg.AWSRegion, queueURL, lcCfg)
		if err != nil {
			log.Printf("warning: lifecycle poller unavailable: %v", err)
		} else if poller != nil {
			poller.Start(cfg.Ctx)
			log.Printf("lifecycle: subscribed to %s", queueURL)
		}
	}

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
	prov.Buckets.SetStore(cfg.DB)

	prov.Start(cfg.Ctx)
	log.Printf("provisioner: started with docker launcher (dev mode), image=%s data=%s",
		launcher.Image, launcher.HostDataRoot)
	return prov, nil
}

// readTierSizes returns per-tier data volume sizes from env vars
// populated by load-app-env out of the TF-managed config blob (see
// stacks/asg-weft-application/main.tf → aws_ssm_parameter.app_config_blob).
// Falls back to sensible defaults when a var is missing or unparseable.
func readTierSizes() map[string]int32 {
	defaults := map[string]int32{"small": 100, "medium": 500, "large": 2000}
	sizes := map[string]int32{}
	for tier, def := range defaults {
		envKey := fmt.Sprintf("SHARD_%s_VOLUME_GB", strings.ToUpper(tier))
		if val, ok := parsePositiveInt32(os.Getenv(envKey)); ok {
			sizes[tier] = val
		} else {
			sizes[tier] = def
		}
	}
	return sizes
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

func parsePositiveInt32(s string) (int32, bool) {
	if s == "" {
		return 0, false
	}
	var v int32
	if _, err := fmt.Sscanf(s, "%d", &v); err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}
