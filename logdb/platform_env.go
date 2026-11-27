package logdb

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/w3ft-net/weft/shard"
)

// PlatformFromEnv is the Platform the environment names: EC2 launched directly
// when SHARD_<TIER>_SUBNETS is configured, Docker otherwise. It is where the
// deployment policy that the coordinator's main has no reason to know lives
// (SHARD_*, EXPORT_*, LAMBDA_FUNCTION_URL, PARQUET_DIR, the WEFT_* Docker
// variables). Both halves are being moved out of core, to ext/launcher/ec2 and
// ext/launcher/docker; a deployment then builds its Platform itself.
func PlatformFromEnv(ctx context.Context, region, shardSecret string) (Platform, error) {
	subnets := parseShardSubnets()
	if len(subnets) > 0 {
		return ec2Platform(ctx, region, shardSecret, subnets, parseShardTemplates())
	}
	return dockerPlatform(shardSecret), nil
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

// exportConfig is the ephemeral archive export's deployment config,
// all from the environment so
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

// ec2Platform is the EC2Provisioner and volume manager for production deploys. The coordinator launches shard instances directly via
// RunInstances/TerminateInstances (no Auto Scaling Group) — it is the sole
// capacity authority, so a shard is backed by exactly one instance and there
// is no "ghost" failure mode. subnets is tier→az→subnet; templates is
// tier→launch-template-id.
func ec2Platform(ctx context.Context, region, shardSecret string, subnets map[string]map[string]string, templates map[string]string) (Platform, error) {
	// One VolumeManager, shared: the EC2Provisioner creates/attaches volumes
	// on launch, and the provisioner's shard SM retires them on archive.
	vm, err := NewEC2VolumeManager(region)
	if err != nil {
		return Platform{}, fmt.Errorf("EC2Provisioner needs a VolumeManager (launch creates/attaches volumes): %w", err)
	}
	ep, err := NewEC2Provisioner(region, subnets, templates, vm)
	if err != nil {
		return Platform{}, fmt.Errorf("EC2 provisioner: %w", err)
	}
	weftsdk.Infof(ctx, "provisioner: EC2Provisioner (direct, per-AZ) for %d tier(s): subnets=%v templates=%v", len(subnets), subnets, templates)

	provConfig := DefaultProvisionerConfig()
	provConfig.ParquetDir = os.Getenv("PARQUET_DIR")
	provConfig.LambdaFunctionURL = os.Getenv("LAMBDA_FUNCTION_URL")
	provConfig.VolumeSizesGB = parseShardVolumeSizes()
	wireArchiveExport(ctx, &provConfig, region, subnets)
	weftsdk.Infof(ctx, "provisioner: per-tier volume sizes (GiB) from env: %v (tiers absent fall back to %d)", provConfig.VolumeSizesGB, defaultShardVolumeGB)
	return Platform{Launcher: ep, Volumes: vm, Standby: ep, Config: provConfig}, nil
}

// dockerPlatform is the Docker launcher the compose file configures.
func dockerPlatform(shardSecret string) Platform {
	provConfig := DevProvisionerConfig()
	provConfig.ParquetDir = os.Getenv("PARQUET_DIR")
	launcher := buildDockerLauncher(shardSecret)
	weftsdk.Infof(context.Background(), "provisioner: docker launcher, image=%s data=%s", launcher.Image, launcher.HostDataRoot)
	return Platform{Launcher: launcher, Config: provConfig}
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
