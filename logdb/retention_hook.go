package logdb

import (
	"context"
	"time"

	"github.com/w3ft-net/weft/shard"
)

// Retention is an optional feature that expires archived data. A build without
// one stores no archive stats and runs no janitor.
type Retention interface {
	// ArchiveRecorded is told of each archive's per-partition stats. It is
	// best effort: a failure is logged and does not fail the archive.
	ArchiveRecorded(shardID string, stats []shard.PartitionStat) error
	// CheckDelete is asked before an operator deletes an archived shard
	// without force. An error wrapping ErrShardWithinRetention refuses it.
	CheckDelete(shardID string, bucketID int64, windowEnd time.Time) error
	// Start begins the feature's background work once the provisioner runs.
	Start(ctx context.Context, prov *Provisioner)
}

// ParquetDir is where this deployment's archives are written: a local path or
// an s3:// prefix.
func (p *Provisioner) ParquetDir() string { return p.config.ParquetDir }

// RefreshArchivedPath repoints an archived shard's in-memory entry at a
// rewritten parquet, so new query descriptors read the new file.
func (p *Provisioner) RefreshArchivedPath(shardID, newPath string) {
	p.registry.RefreshArchivedPath(shardID, newPath, p.config.LambdaFunctionURL)
}
