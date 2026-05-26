package store

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// ShardRecord persists shard registry entries across coordinator restarts.
type ShardRecord struct {
	ID          string `gorm:"primaryKey"`
	WindowStart string `gorm:"not null"` // RFC3339
	WindowEnd   string `gorm:"not null"` // RFC3339
	State       string `gorm:"not null"` // INGESTING/READ_ONLY/ARCHIVED
	Addr        string // gRPC address for live shards
	InstanceID  string // EC2 instance ID
	ParquetPath string // Parquet file path for archived shards
	Tier        string // instance tier (small/medium/large)
	RowCount    int64  `gorm:"default:0"`
	CreatedAt   string `gorm:"not null"` // RFC3339
	EBSVolumeID string // EBS volume holding this shard's DuckDB (empty = not on a dedicated volume yet)
	// HeartbeatToken is a per-shard bearer the shard presents on every
	// Heartbeat RPC. Minted by the provisioner at christen, returned
	// to the shard in the Configure response, and persisted in
	// shard.json. Required: a record with an empty token can no
	// longer authenticate.
	HeartbeatToken string
	// BucketID references buckets.id (surrogate bigint since
	// migration 022). Every shard belongs to exactly one bucket;
	// christen() resolves the target bucket's surrogate via the
	// BucketManager and stamps the int here.
	BucketID int64 `gorm:"not null;column:bucket_id"`
	// IngestFailures, IngestUnhealthy, LastHeartbeatAt, LaunchAttempts,
	// AZ, StripeIndex are populated by the shard FSM (migration 056).
	// IngestFailures and IngestUnhealthy persist the in-memory
	// `Suspended`/IngestFailures pair that previously lost state on
	// restart. LastHeartbeatAt likewise — its prior in-memory zero
	// value made every entry look "fresh" for a tick window post-
	// boot, masking dead shards. LaunchAttempts is the per-shard
	// retry counter across reap ticks. AZ + StripeIndex carry the
	// striping assignment so recovery launches don't have to
	// re-derive them from volume tags.
	IngestFailures  int        `gorm:"not null;default:0;column:ingest_failures"`
	IngestUnhealthy bool       `gorm:"not null;default:false;column:ingest_unhealthy"`
	LastHeartbeatAt *time.Time `gorm:"column:last_heartbeat_at"`
	LaunchAttempts  int        `gorm:"not null;default:0;column:launch_attempts"`
	AZ              string     `gorm:"column:az"`
	StripeIndex     *int       `gorm:"column:stripe_index"`
}

// GenerateShardHeartbeatToken mints a fresh 256-bit random bearer token
// for a newly-christened shard. Callers store the result on the
// ShardRecord and hand the same string back to the shard via the
// Configure RPC.
func GenerateShardHeartbeatToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("heartbeat token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// ConstantTimeTokenEqual compares two heartbeat tokens in constant
// time. Returns false if either side is empty — an empty stored token
// means "no token to authenticate against" and must never match an
// empty presented bearer.
func ConstantTimeTokenEqual(stored, presented string) bool {
	if stored == "" || presented == "" {
		return false
	}
	if len(stored) != len(presented) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(stored), []byte(presented)) == 1
}

// SaveShard upserts a shard record.
func (s *Store) SaveShard(rec *ShardRecord) error {
	return s.db.Save(rec).Error
}

// GetShard returns the record for id. Returns (record, true, nil) when
// found, (zero, false, nil) when absent, and (zero, false, err) on DB
// error. Used by the provisioner's adopt path to preserve persisted
// fields that the heartbeat itself doesn't carry (window, token, apps).
func (s *Store) GetShard(id string) (ShardRecord, bool, error) {
	var rec ShardRecord
	err := s.db.Where("id = ?", id).First(&rec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ShardRecord{}, false, nil
	}
	if err != nil {
		return ShardRecord{}, false, err
	}
	return rec, true, nil
}

// TouchHeartbeat updates only the fields a heartbeat owns: row_count
// and (if suspended) state. Everything else — window bounds, apps,
// tier, etc. — is left untouched so manual DB edits aren't clobbered.
func (s *Store) TouchHeartbeat(id string, rowCount int64) error {
	return s.db.Model(&ShardRecord{}).Where("id = ?", id).
		Update("row_count", rowCount).Error
}

// DeleteShard removes a shard record.
func (s *Store) DeleteShard(id string) error {
	return s.db.Delete(&ShardRecord{}, "id = ?", id).Error
}

// SetShardLaunchResult persists the identifiers a successful launch
// produced (instance, AZ, volume, stripe index). Narrow on purpose —
// Select names the four columns so a zero value (e.g. an empty
// instance) still writes, but nothing else (state, heartbeat, window)
// is touched. The FSM owns these columns; this can't race a concurrent
// state write.
func (s *Store) SetShardLaunchResult(id, instanceID, az, volumeID string, stripeIndex *int) error {
	return s.db.Model(&ShardRecord{}).Where("id = ?", id).
		Select("InstanceID", "AZ", "EBSVolumeID", "StripeIndex").
		Updates(&ShardRecord{
			InstanceID:  instanceID,
			AZ:          az,
			EBSVolumeID: volumeID,
			StripeIndex: stripeIndex,
		}).Error
}

// IncrementShardLaunchAttempts bumps launch_attempts by one. Used by
// the FSM after a failed launch so a later Evaluate can fire
// LaunchFailed once the retry budget is exhausted.
func (s *Store) IncrementShardLaunchAttempts(id string) error {
	return s.db.Model(&ShardRecord{}).Where("id = ?", id).
		UpdateColumn("launch_attempts", gorm.Expr("launch_attempts + 1")).Error
}

// SetShardParquetPath records where an archived shard's data landed in
// S3. The FSM's ARCHIVED-cleanup Evaluate sets this once the export
// succeeds so a later tick (or a restart mid-cleanup) skips re-exporting.
// Narrow (Select forces the single column) so it can't race a state write.
func (s *Store) SetShardParquetPath(id, path string) error {
	return s.db.Model(&ShardRecord{}).Where("id = ?", id).
		Select("ParquetPath").Updates(&ShardRecord{ParquetPath: path}).Error
}

// ClearShardVolume blanks ebs_volume_id after an archived shard's data
// volume is snapshotted and deleted. Select forces the empty string to
// write (a zero-value struct field is otherwise skipped by gorm). Lets
// the next Evaluate tick see "volume already retired" and move on.
func (s *Store) ClearShardVolume(id string) error {
	return s.db.Model(&ShardRecord{}).Where("id = ?", id).
		Select("EBSVolumeID").Updates(&ShardRecord{EBSVolumeID: ""}).Error
}

// ClearShardInstance blanks instance_id after an archived shard's
// instance is terminated. Same narrow-write rationale as ClearShardVolume.
func (s *Store) ClearShardInstance(id string) error {
	return s.db.Model(&ShardRecord{}).Where("id = ?", id).
		Select("InstanceID").Updates(&ShardRecord{InstanceID: ""}).Error
}

// ListShards returns all shard records.
func (s *Store) ListShards() ([]ShardRecord, error) {
	var records []ShardRecord
	err := s.db.Find(&records).Error
	return records, err
}

// ── Shard heartbeats ────────────────────────────────────────

// ShardHeartbeat represents a shard's periodic status report.
type ShardHeartbeat struct {
	ShardID    string
	Addr       string
	InstanceID string
	State      string
	RowCount   int64
	DiskBytes  int64
	BeatAt     string // RFC3339
}

// InsertHeartbeat writes a heartbeat row. Uses raw SQL since the table
// is partitioned and not managed by GORM AutoMigrate.
func (s *Store) InsertHeartbeat(hb *ShardHeartbeat) error {
	return s.db.Exec(`
		INSERT INTO shard_heartbeats (shard_id, addr, instance_id, state, row_count, disk_bytes, beat_at)
		VALUES (?, ?, ?, ?, ?, ?, NOW())`,
		hb.ShardID, hb.Addr, hb.InstanceID, hb.State, hb.RowCount, hb.DiskBytes).Error
}

// LiveShardHeartbeats returns the most recent heartbeat per shard
// where beat_at is within the given duration. A shard with no heartbeat
// in this window is considered dead.
func (s *Store) LiveShardHeartbeats(since string) ([]ShardHeartbeat, error) {
	var results []ShardHeartbeat
	err := s.db.Raw(`
		SELECT DISTINCT ON (shard_id)
			shard_id, addr, instance_id, state, row_count, disk_bytes, beat_at
		FROM shard_heartbeats
		WHERE beat_at > NOW() - ?::interval
		ORDER BY shard_id, beat_at DESC`,
		since).Scan(&results).Error
	return results, err
}
