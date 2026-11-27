package logdb

import (
	"context"

	"github.com/w3ft-net/weft/shard"
)

// Platform is where shards run: what launches them, and what a platform that has
// them keeps beside the launcher. Start takes one; a deployment composes it from
// the launcher it uses (ext/launcher/*) and the core never names a cloud.
type Platform struct {
	// Launcher starts and stops shard instances. Required.
	Launcher ShardLauncher

	// Volumes keeps each shard's data volume: the shard SM asks it for the
	// SUSPENDED stranded check and to retire a volume on archive. Nil: shards
	// have no separate volume.
	Volumes VolumeManager

	// Standby launches the instances of the warm pool. Nil: no warm pool. A
	// Launcher that also takes SetStandbys(Standbys) is handed the pool's service.
	Standby StandbyLauncher

	// Config is the provisioner's configuration on this platform: the defaults
	// that fit it (DefaultProvisionerConfig or DevProvisionerConfig), the archive
	// export, volume sizes and where Parquet and cold reads live. Start sets the
	// per-process fields (secret, holder, leases, retention) on a copy.
	Config ProvisionerConfig
}

// ExportStorage is where the archive export keeps its manifests and removes its
// output: the object store the deployment archives to (ext/storage/s3). Nil: the
// export is disabled and shards archive in process.
type ExportStorage func(ctx context.Context, region string) (ExportManifests, ObjectDeleter, error)

// ExportManifests is what the export keeps its completion manifests in: the
// export host writes them, the coordinator reads them back.
type ExportManifests interface {
	shard.ManifestStore
	shard.ManifestReader
}
