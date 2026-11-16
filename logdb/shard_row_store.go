package logdb

// shardRowStore is the shard_records persistence surface the shard SM
// needs (ShardSMDeps.Store). Declared as an interface — not the concrete
// Store — so deps.Store stays nilable for the dev/test paths that
// don't wire persistence, and so tests can substitute a fake.
type shardRowStore interface {
	ExportStore
	UpgradeStore
	GetShard(id string) (*ShardRow, bool, error)
	DeleteShard(id string) error
	SetShardState(id string, state ShardState) error
	SetShardAddr(id, instanceID, addr string) error
	SetShardIngestFailures(id string, n int) error
	SetShardLaunchResult(id, instanceID, az, volumeID string, stripeIndex *int) error
	IncrementShardLaunchAttempts(id string) error
	SetShardParquetPath(id, path string) error
	ClearShardVolume(id string) error
	ClearShardInstance(id string) error
	LatestPreRecoveryState(id string) (ShardState, error)
}
