package parquet

import (
	"context"
	"time"

	"github.com/w3ft-net/weft/backends/logs/live"
)

// ExportManifest is the completion record an export writes to the archive
// prefix. Attempt lets the coordinator ignore a stale manifest left by an
// earlier attempt.
type ExportManifest struct {
	ShardID     string `json:"shard_id"`
	Attempt     int    `json:"attempt"`
	ParquetPath string `json:"parquet_path"`
	Rows        int64  `json:"rows"`
	DBBytes     int64  `json:"db_bytes"`
	// Partitions keeps the JSON key "customers" the previous build reads and
	// wrote: an export's manifest crosses the coordinator, which may be of
	// either build.
	Partitions []live.PartitionStat `json:"customers"`
	Started    time.Time            `json:"started"`
	Finished   time.Time            `json:"finished"`
}

// ManifestStore persists a manifest at key (as produced by ManifestKey).
type ManifestStore interface {
	Put(ctx context.Context, key string, m ExportManifest) error
}

// ManifestReader is the coordinator's side: it polls for the manifest an export
// wrote. found is false (with a nil error) when no manifest exists yet; an
// unreadable or corrupt manifest is an error, never "not found".
type ManifestReader interface {
	Get(ctx context.Context, key string) (m ExportManifest, found bool, err error)
}
