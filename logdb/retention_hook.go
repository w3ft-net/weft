package logdb

import (
	"context"
	"time"

	"github.com/graybeardcorp/weft/shard"
)

// Retention is an optional feature that expires archived data. A build without
// one stores no archive stats and runs no janitor; the hosted build installs
// tenancy/hosted/retention.
type Retention interface {
	// ArchiveRecorded is told of each archive's per-partition stats. It is
	// best effort: a failure is logged and does not fail the archive.
	ArchiveRecorded(shardID string, stats []shard.CustomerStat) error
	// CheckDelete is asked before an operator deletes an archived shard
	// without force. An error wrapping ErrShardWithinRetention refuses it.
	CheckDelete(shardID string, bucketID int64, windowEnd time.Time) error
	// Start begins the feature's background work once the provisioner runs.
	Start(ctx context.Context, prov *Provisioner)
}

// ShardStore is the shard_records store this provisioner persists to, for a
// Retention that reads and rewrites archived shards.
func (p *Provisioner) ShardStore() ShardStore { return ShardStore{p.store} }

// RefreshArchivedPath repoints an archived shard's in-memory entry at a
// rewritten parquet, so new query descriptors read the new file.
func (p *Provisioner) RefreshArchivedPath(shardID, newPath string) {
	p.registry.RefreshArchivedPath(shardID, newPath, p.config.LambdaFunctionURL)
}
