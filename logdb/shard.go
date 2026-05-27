package logdb

import (
	"errors"

	"gorm.io/gorm"

	"github.com/kevlarcon/weft/store"
)

// shardRowStore is the shard_records persistence surface the shard SM
// needs (ShardSMDeps.Store). Declared as an interface — not the concrete
// ShardStore — so deps.Store stays nilable for the dev/test paths that
// don't wire persistence, and so tests can substitute a fake.
type shardRowStore interface {
	GetShard(id string) (*Shard, bool, error)
	DeleteShard(id string) error
	SetShardState(id string, state ShardState) error
	SetShardAddr(id, addr string) error
	SetShardLaunchResult(id, instanceID, az, volumeID string, stripeIndex *int) error
	IncrementShardLaunchAttempts(id string) error
	SetShardParquetPath(id, path string) error
	ClearShardVolume(id string) error
	ClearShardInstance(id string) error
}

// ShardStore is the gorm-backed persistence for the unified Shard type.
// It embeds *store.Store — so it inherits the shared DB connection,
// tenant-scoping, and every non-shard-row method (shard_services,
// heartbeats, buckets) — and adds the shard_records row operations that
// used to live on *store.Store as methods over store.ShardRecord. They
// moved here because the row model is now logdb.Shard, and store importing
// logdb would be an import cycle (logdb imports store). The DB schema is
// unchanged; only the Go ownership of the model + its row methods moved.
//
// ShardStore satisfies both RegistryStore and the provisioner's
// ShardOpsStore / ShardSMDeps.Store needs.
type ShardStore struct {
	*store.Store
}

// db is the shared *gorm.DB behind the embedded *store.Store.
func (s ShardStore) db() *gorm.DB { return s.DB() }

// SaveShard upserts a shard row. The caller passes a snapshot copy (see
// ShardRegistry.persistedCopy), so gorm's reflection over the struct
// can't race the registry's live entry.
func (s ShardStore) SaveShard(sh *Shard) error {
	return s.db().Save(sh).Error
}

// GetShard returns the row for id. (row, true, nil) when found,
// (nil, false, nil) when absent, (nil, false, err) on DB error. Used by
// the shard SM's state accessor and the adopt path.
func (s ShardStore) GetShard(id string) (*Shard, bool, error) {
	var sh Shard
	err := s.db().Where("id = ?", id).First(&sh).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &sh, true, nil
}

// ListShards returns every shard row as fresh *Shard objects (loaded
// independently of the registry's live entries).
func (s ShardStore) ListShards() ([]*Shard, error) {
	var shards []*Shard
	err := s.db().Find(&shards).Error
	return shards, err
}

// DeleteShard removes a shard row.
func (s ShardStore) DeleteShard(id string) error {
	return s.db().Delete(&Shard{}, "id = ?", id).Error
}

// TouchHeartbeat updates only row_count — the one field a heartbeat owns.
// Everything else (window bounds, state, services) is left untouched so
// manual DB edits or reconciler-driven changes aren't clobbered.
func (s ShardStore) TouchHeartbeat(id string, rowCount int64) error {
	return s.db().Model(&Shard{}).Where("id = ?", id).
		Update("row_count", rowCount).Error
}

// SetShardLaunchResult persists the identifiers a successful launch
// produced (instance, AZ, volume, stripe index). Narrow on purpose —
// Select names the four columns so a zero value (e.g. an empty instance)
// still writes, but nothing else (state, heartbeat, window) is touched.
func (s ShardStore) SetShardLaunchResult(id, instanceID, az, volumeID string, stripeIndex *int) error {
	return s.db().Model(&Shard{}).Where("id = ?", id).
		Select("InstanceID", "AZ", "EBSVolumeID", "StripeIndex").
		Updates(&Shard{
			InstanceID:  instanceID,
			AZ:          az,
			EBSVolumeID: volumeID,
			StripeIndex: stripeIndex,
		}).Error
}

// SetShardState narrow-writes only the state column — the shard FSM's
// transition mutator. Lifecycle-owned fields (window bounds, services,
// addr) move through SaveShard, so a transition can't trample a
// concurrent SaveShard over them.
func (s ShardStore) SetShardState(id string, state ShardState) error {
	return s.db().Model(&Shard{}).Where("id = ?", id).
		Update("state", string(state)).Error
}

// SetShardAddr persists the live gRPC address a heartbeat reported.
// Narrow (Select forces the single column, so an empty string still
// writes) so the heartbeat path can persist addr without a full SaveShard
// trampling lifecycle-owned fields. Persisting it is what lets
// ReconstructShard eager-dial the shard after a coordinator restart and
// stops RefreshFromStore from clobbering the live addr with a stale row.
func (s ShardStore) SetShardAddr(id, addr string) error {
	return s.db().Model(&Shard{}).Where("id = ?", id).
		Select("Addr").Updates(&Shard{Addr: addr}).Error
}

// IncrementShardLaunchAttempts bumps launch_attempts by one.
func (s ShardStore) IncrementShardLaunchAttempts(id string) error {
	return s.db().Model(&Shard{}).Where("id = ?", id).
		UpdateColumn("launch_attempts", gorm.Expr("launch_attempts + 1")).Error
}

// SetShardParquetPath records where an archived shard's data landed in S3.
// Narrow (Select forces the single column) so it can't race a state write.
func (s ShardStore) SetShardParquetPath(id, path string) error {
	return s.db().Model(&Shard{}).Where("id = ?", id).
		Select("ParquetPath").Updates(&Shard{ParquetPath: path}).Error
}

// ClearShardVolume blanks ebs_volume_id after an archived shard's data
// volume is snapshotted and deleted. Select forces the empty string to
// write (a zero-value struct field is otherwise skipped by gorm).
func (s ShardStore) ClearShardVolume(id string) error {
	return s.db().Model(&Shard{}).Where("id = ?", id).
		Select("EBSVolumeID").Updates(&Shard{EBSVolumeID: ""}).Error
}

// ClearShardInstance blanks instance_id after an archived shard's
// instance is terminated. Same narrow-write rationale as ClearShardVolume.
func (s ShardStore) ClearShardInstance(id string) error {
	return s.db().Model(&Shard{}).Where("id = ?", id).
		Select("InstanceID").Updates(&Shard{InstanceID: ""}).Error
}
