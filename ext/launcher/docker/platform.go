package docker

import (
	"context"
	"github.com/w3ft-net/weft/logdb"
	"log"
	"os"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"
)

// FromEnv is the Docker launcher the compose file configures.
func FromEnv(shardSecret string) logdb.Platform {
	provConfig := logdb.DevProvisionerConfig()
	provConfig.ParquetDir = os.Getenv("PARQUET_DIR")
	launcher := buildLauncher(shardSecret)
	weftsdk.Infof(context.Background(), "provisioner: docker launcher, image=%s data=%s", launcher.Image, launcher.HostDataRoot)
	return logdb.Platform{Launcher: launcher, Config: provConfig}
}

// buildLauncher constructs a Launcher from env vars set
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
func buildLauncher(shardSecret string) *Launcher {
	requireEnv := func(name string) string {
		v := os.Getenv(name)
		if v == "" {
			log.Fatalf("docker launcher: %s is required (no in-process fallback in dev mode)", name)
		}
		return v
	}
	return &Launcher{
		Image:             requireEnv("WEFT_SHARD_IMAGE"),
		Network:           requireEnv("WEFT_DOCKER_NETWORK"),
		HostDataRoot:      requireEnv("WEFT_HOST_DATA_ROOT"),
		LocalDataRoot:     requireEnv("WEFT_LOCAL_DATA_ROOT"),
		CoordinatorAddr:   requireEnv("WEFT_COORDINATOR_ADDR"),
		HeartbeatInterval: 10 * time.Second,
		ShardSecret:       shardSecret,
	}
}
