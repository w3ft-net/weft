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
	weftsdk "github.com/w3ft-net/weft-go"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"

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
	// lives in compute, not here — see compute.Engine.
	ShardSecret string

	// AWSRegion is threaded into NewEC2Provisioner / NewEC2VolumeManager.
	// Each loads its own AWS config in that region.
	AWSRegion string

	// Holder identifies this process for the "shard-fsm-leader" lease
	// (ProvisionerConfig.Holder) — the same holder id main.go's
	// coordinatorHolder() mints for the NAT/poller/cron leases. Empty
	// disables the election: the Provisioner stays leader unconditionally,
	// the pre-#5 single-process behavior. See
	// docs/plans/coordinator-horizontal-scaling.md #5.
	Holder string
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
		weftsdk.Warnf(context.Background(), "warning: load shards from store: %v", err)
	} else {
		weftsdk.Infof(context.Background(), "coordinator: restored shards from store")
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
				weftsdk.Warnf(context.Background(), "warning: %s: malformed pair %q (want az=subnet), skipping", env, pair)
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

// parseShardVolumeSizes reads the per-tier fresh data-volume sizes (in GiB)
// from the environment — SHARD_SMALL_VOLUME_GB / SHARD_MEDIUM_VOLUME_GB /
// SHARD_LARGE_VOLUME_GB, each a single integer, e.g.
//
//	SHARD_SMALL_VOLUME_GB="25"
//
// These come from Terraform (the asg-weft-application stack's
// shard_<tier>_volume_gb vars). Returns tier→GiB for the tiers that set a
// positive value; a tier missing or malformed here falls back to
// defaultShardVolumeGB at launch (see ShardDeps.volumeSizeGB). Empty when no
// tier is configured (dev mode), which is fine — the docker launcher ignores
// volume size.
func parseShardVolumeSizes() map[string]int32 {
	out := map[string]int32{}
	for tier, env := range map[string]string{
		"small":  "SHARD_SMALL_VOLUME_GB",
		"medium": "SHARD_MEDIUM_VOLUME_GB",
		"large":  "SHARD_LARGE_VOLUME_GB",
	} {
		raw := strings.TrimSpace(os.Getenv(env))
		if raw == "" {
			continue
		}
		gb, err := strconv.Atoi(raw)
		if err != nil || gb <= 0 {
			weftsdk.Warnf(context.Background(), "warning: %s=%q is not a positive integer, ignoring (tier %s falls back to %d GiB)", env, raw, tier, defaultShardVolumeGB)
			continue
		}
		out[tier] = int32(gb)
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

// exportConfig is the ephemeral archive export's deployment config
// (docs/plans/ephemeral-archive-export.md), all from the environment so
// Terraform owns it:
//
//	EXPORT_LAUNCH_TEMPLATE  the dedicated export launch template id (Role=export)
//	EXPORT_MAX_CONCURRENT   cap on export instances in flight (default 2)
//
// Export instances launch into the shard tiers' subnets, so the subnet map is
// derived from SHARD_<TIER>_SUBNETS rather than configured separately.
type exportConfig struct {
	LaunchTemplate string
	MaxConcurrent  int // 0 → defaultMaxConcurrentExports
	Subnets        map[string]string
}

// parseExportConfig reads the export config. tierSubnets is tier→az→subnet; the
// export uses one subnet per AZ, the smaller tier's winning where tiers differ.
func parseExportConfig(tierSubnets map[string]map[string]string) exportConfig {
	c := exportConfig{
		LaunchTemplate: strings.TrimSpace(os.Getenv("EXPORT_LAUNCH_TEMPLATE")),
		Subnets:        map[string]string{},
	}
	for _, tier := range []string{"small", "medium", "large"} {
		for az, subnet := range tierSubnets[tier] {
			if _, taken := c.Subnets[az]; !taken {
				c.Subnets[az] = subnet
			}
		}
	}
	if raw := strings.TrimSpace(os.Getenv("EXPORT_MAX_CONCURRENT")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			weftsdk.Warnf(context.Background(), "warning: EXPORT_MAX_CONCURRENT=%q is not a positive integer, using the default (%d)", raw, defaultMaxConcurrentExports)
		} else {
			c.MaxConcurrent = n
		}
	}
	return c
}

// usable reports whether the export can run, and if not, why. The export
// writes its manifest under the archive prefix over S3 and the coordinator
// reads it back, so a local parquet dir cannot carry it.
func (c exportConfig) usable(parquetDir string) (bool, string) {
	switch {
	case c.LaunchTemplate == "":
		return false, "EXPORT_LAUNCH_TEMPLATE is not set"
	case len(c.Subnets) == 0:
		return false, "no shard subnets to launch export instances into"
	case !strings.HasPrefix(parquetDir, "s3://"):
		return false, fmt.Sprintf("PARQUET_DIR %q is not an s3:// prefix", parquetDir)
	}
	return true, ""
}

// wireArchiveExport enables the ephemeral archive export on provConfig when it
// is configured. When it is not — or cannot be built — shards keep archiving in
// process, which is the pre-export behavior: loud in the log, never a boot
// failure, because a coordinator that will not start is worse than a slow
// archive.
func wireArchiveExport(ctx context.Context, provConfig *ProvisionerConfig, region string, tierSubnets map[string]map[string]string) {
	c := parseExportConfig(tierSubnets)
	if ok, why := c.usable(provConfig.ParquetDir); !ok {
		weftsdk.Infof(ctx, "provisioner: archive export disabled (%s); shards archive in process", why)
		return
	}
	exports, err := NewExportResources(region, c.Subnets, c.LaunchTemplate)
	if err != nil {
		weftsdk.Infof(ctx, "provisioner: archive export disabled, ExportResources: %v; shards archive in process", err)
		return
	}
	manifests, err := shard.NewS3ManifestStore(ctx, region)
	if err != nil {
		weftsdk.Infof(ctx, "provisioner: archive export disabled, manifest store: %v; shards archive in process", err)
		return
	}
	outputs, err := NewS3ObjectDeleter(ctx, region)
	if err != nil {
		weftsdk.Infof(ctx, "provisioner: archive export disabled, output deleter: %v; shards archive in process", err)
		return
	}
	provConfig.Exports = exports
	provConfig.ExportManifests = manifests
	provConfig.ExportOutputs = outputs
	provConfig.MaxConcurrentExports = c.MaxConcurrent
	maxConcurrent := c.MaxConcurrent
	if maxConcurrent == 0 {
		maxConcurrent = defaultMaxConcurrentExports
	}
	weftsdk.Infof(ctx, "provisioner: archive export enabled (template=%s subnets=%v max_concurrent=%d)",
		c.LaunchTemplate, c.Subnets, maxConcurrent)
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
	weftsdk.Infof(context.Background(), "provisioner: EC2Provisioner (direct, per-AZ) for %d tier(s): subnets=%v templates=%v", len(subnets), subnets, templates)

	provConfig := DefaultProvisionerConfig()
	provConfig.ShardSecret = cfg.ShardSecret
	provConfig.ParquetDir = os.Getenv("PARQUET_DIR")
	provConfig.LambdaFunctionURL = lambdaURL
	provConfig.VolumeSizesGB = parseShardVolumeSizes()
	provConfig.Holder = cfg.Holder
	wireArchiveExport(cfg.Ctx, &provConfig, cfg.AWSRegion, subnets)
	weftsdk.Infof(context.Background(), "provisioner: per-tier volume sizes (GiB) from env: %v (tiers absent fall back to %d)", provConfig.VolumeSizesGB, defaultShardVolumeGB)
	prov := NewProvisioner(reg, launcher, provConfig)
	prov.SetVolumeManager(vm)

	prov.SetStore(cfg.DB)
	// The warm pool of volume-less spares that upgrades, recoveries and
	// mints claim instead of waiting on EC2 (docs/plans/
	// shard-seamless-upgrade.md). Spares run the coordinator's own AMI.
	pool := NewSparePool(ShardStore{cfg.DB}, ep, nil)
	ep.SetSparePool(pool)
	prov.SetSparePool(pool)

	// Build the FSM control plane (one SM per persisted bucket + shard).
	// The tick drives these — they own all lifecycle convergence.
	prov.LoadStateMachines()

	prov.Start(cfg.Ctx)
	startRetentionJanitor(cfg.Ctx, prov, cfg.AWSRegion)
	startArchiveSweeper(cfg.Ctx, prov)
	return prov, nil
}

// startArchiveSweeper starts the loop that removes orphaned export resources and
// logs an ALERT for shards stuck in ARCHIVING. It always runs in prod: the
// stuck-archive alert applies to in-process archives too. Orphan removal needs
// the export wired; without it only the alert runs.
func startArchiveSweeper(ctx context.Context, prov *Provisioner) {
	sw := &ArchiveSweeper{
		Store:    ShardStore{prov.store},
		Clock:    time.Now,
		IsLeader: prov.IsLeader,
	}
	if lister, ok := prov.config.Exports.(ExportLister); ok {
		sw.Resources = lister
	}
	sw.Start(ctx)
	weftsdk.Infof(ctx, "coordinator: archive sweeper started (orphan removal=%v, stuck-archive alert after %s)", sw.Resources != nil, archivingStuckAfter)
}

// startRetentionJanitor wires and starts the daily archive retention
// janitor when JANITOR_ENABLED=1. Off by default: it permanently deletes
// customer data, so each environment opts in explicitly — after the
// compactor Lambda is deployed and COMPACTOR_FUNCTION_URL is set. With no
// compactor URL it falls back to an in-process compactor (dev / pre-Lambda).
func startRetentionJanitor(ctx context.Context, prov *Provisioner, region string) {
	if prov == nil || os.Getenv("JANITOR_ENABLED") != "1" {
		return
	}

	var compactor shard.Compactor
	if url := os.Getenv("COMPACTOR_FUNCTION_URL"); url != "" {
		compactor = shard.NewLambdaCompactor(url, lambdaSigningCreds(ctx, region), region)
		weftsdk.Infof(ctx, "janitor: compactor Lambda at %s", url)
	} else {
		compactor = shard.LocalCompactor{S3: shard.S3ConfigFromEnv()}
		weftsdk.Infof(ctx, "janitor: in-process compactor (COMPACTOR_FUNCTION_URL unset)")
	}

	var deleter ObjectDeleter = LocalFileDeleter{}
	if strings.HasPrefix(prov.config.ParquetDir, "s3://") {
		d, err := NewS3ObjectDeleter(ctx, region)
		if err != nil {
			weftsdk.Infof(ctx, "janitor: S3 deleter unavailable, not starting: %v", err)
			return
		}
		deleter = d
	}

	NewProvisionerJanitor(prov, compactor, deleter, 24*time.Hour).Start(ctx)
	weftsdk.Infof(ctx, "coordinator: retention janitor enabled (daily)")
}

// lambdaSigningCreds loads the credential provider used to SigV4-sign
// compactor Function URL calls. Returns nil (unsigned) on failure — the
// invoke then fails loudly at call time rather than here at boot.
func lambdaSigningCreds(ctx context.Context, region string) aws.CredentialsProvider {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		weftsdk.Infof(ctx, "janitor: aws creds for compactor signing: %v", err)
		return nil
	}
	return cfg.Credentials
}

// startDev wires the docker-based dev launcher and provisioner. Fatal
// on missing required env (no in-process fallback in this branch — the
// operator gets a precise error rather than silent data loss).
func startDev(cfg Config, reg *ShardRegistry) (*Provisioner, error) {
	launcher := buildDockerLauncher(cfg.ShardSecret)
	provConfig := DevProvisionerConfig()
	provConfig.ShardSecret = cfg.ShardSecret
	provConfig.ParquetDir = os.Getenv("PARQUET_DIR")
	provConfig.Holder = cfg.Holder
	prov := NewProvisioner(reg, launcher, provConfig)

	prov.SetStore(cfg.DB)

	// Build the FSM control plane so restored shards get an SM (the tick
	// and ProcessHeartbeat both resolve shards through the SM maps).
	prov.LoadStateMachines()

	prov.Start(cfg.Ctx)
	weftsdk.Infof(context.Background(), "provisioner: started with docker launcher (dev mode), image=%s data=%s",
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
