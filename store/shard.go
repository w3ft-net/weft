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
	Addr        string                   // gRPC address for live shards
	InstanceID  string                   // EC2 instance ID
	ParquetPath string                   // Parquet file path for archived shards
	Tier        string                   // instance tier (small/medium/large)
	RowCount    int64  `gorm:"default:0"`
	CreatedAt   string `gorm:"not null"` // RFC3339
	EBSVolumeID string                   // EBS volume holding this shard's DuckDB (empty = not on a dedicated volume yet)
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
	IngestFailures   int       `gorm:"not null;default:0;column:ingest_failures"`
	IngestUnhealthy  bool      `gorm:"not null;default:false;column:ingest_unhealthy"`
	LastHeartbeatAt  *time.Time `gorm:"column:last_heartbeat_at"`
	LaunchAttempts   int       `gorm:"not null;default:0;column:launch_attempts"`
	AZ               string    `gorm:"column:az"`
	StripeIndex      *int      `gorm:"column:stripe_index"`
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

// ListShards returns all shard records.
func (s *Store) ListShards() ([]ShardRecord, error) {
	var records []ShardRecord
	err := s.db.Find(&records).Error
	return records, err
}

// ── LAUNCHING lifecycle ──────────────────────────────────────

// ClaimLaunchingForTier atomically claims the oldest LAUNCHING row for
// the given tier that has no instance_id yet, stamping instance_id
// onto it. Used by the lifecycle hook when an ASG-launched instance
// enters Pending:Wait: the claim lets the hook know which reconciler-
// minted shard identity this instance will carry, so the EBS volume
// gets tagged with a real shard_id instead of a placeholder.
//
// The SELECT FOR UPDATE SKIP LOCKED + UPDATE pair runs inside a
// transaction so two concurrent hooks (e.g., a scale-up batch that
// brought up two instances at once) can't both claim the same row;
// the second hook sees SKIP LOCKED bypass the locked row and either
// claims the next oldest or returns (zero, false).
//
// Returns (record, true, nil) on successful claim, (zero, false, nil)
// when no unclaimed LAUNCHING row exists for this tier.
func (s *Store) ClaimLaunchingForTier(tier, instanceID string) (ShardRecord, bool, error) {
	var claimed ShardRecord
	found := false
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var candidate ShardRecord
		err := tx.Raw(`
			SELECT *
			  FROM shard_records
			 WHERE state = 'LAUNCHING'
			   AND tier = ?
			   AND (instance_id IS NULL OR instance_id = '')
			 ORDER BY created_at ASC
			 LIMIT 1
			 FOR UPDATE SKIP LOCKED`, tier).Scan(&candidate).Error
		if err != nil {
			return err
		}
		if candidate.ID == "" {
			return nil
		}
		if err := tx.Model(&ShardRecord{}).Where("id = ?", candidate.ID).
			Update("instance_id", instanceID).Error; err != nil {
			return err
		}
		candidate.InstanceID = instanceID
		claimed = candidate
		found = true
		return nil
	})
	if err != nil {
		return ShardRecord{}, false, err
	}
	return claimed, found, nil
}

// FindLaunchingByInstance returns the LAUNCHING row stamped with this
// instance_id by the lifecycle hook. Called from the christen path
// when an ASG-launched instance first heartbeats: the row carries the
// reconciler's intent (shard id, bucket, window, token) that the
// christen needs to send in the Configure RPC.
func (s *Store) FindLaunchingByInstance(instanceID string) (ShardRecord, bool, error) {
	if instanceID == "" {
		return ShardRecord{}, false, nil
	}
	var rec ShardRecord
	err := s.db.Where("state = ? AND instance_id = ?", "LAUNCHING", instanceID).
		First(&rec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ShardRecord{}, false, nil
	}
	if err != nil {
		return ShardRecord{}, false, err
	}
	return rec, true, nil
}

// ListStaleLaunching returns LAUNCHING rows whose created_at is older
// than olderThan. The reaper partitions the result by whether
// instance_id is empty: empty means the ASG never brought an instance
// up for this row (capacity exhausted, launch failed); populated
// means the lifecycle hook claimed the row but the instance never
// heartbeated (bad AMI, userdata failure).
func (s *Store) ListStaleLaunching(olderThan time.Duration) ([]ShardRecord, error) {
	threshold := time.Now().UTC().Add(-olderThan).Format(time.RFC3339)
	var out []ShardRecord
	err := s.db.Where("state = ? AND created_at < ?", "LAUNCHING", threshold).
		Find(&out).Error
	return out, err
}

// CountShardsInBucket returns the number of shard_records rows for
// this bucket in any of the given states. The reconciler passes
// ['LAUNCHING','INGESTING'] to compute "effective capacity" — the
// count that must be used when deciding whether to scale up, since
// ignoring LAUNCHING would ratchet the ASG under any launch latency.
func (s *Store) CountShardsInBucket(bucketID int64, states []string) (int, error) {
	if len(states) == 0 {
		return 0, nil
	}
	var n int64
	err := s.db.Model(&ShardRecord{}).
		Where("bucket_id = ? AND state IN ?", bucketID, states).
		Count(&n).Error
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// CountShardsInBucketForFutureWindows returns the count of LAUNCHING
// rows in this bucket whose window_start is strictly greater than
// currentWindowStart. The reconciler's pre-warm path uses this to
// know whether next-window LAUNCHING rows already exist (so it
// doesn't re-emit), and the tier-level desired sync uses it to add
// the pre-warmed LAUNCHING rows as extra capacity beyond bucket
// targets.
func (s *Store) CountShardsInBucketForFutureWindows(bucketID int64, currentWindowStart time.Time) (int, error) {
	var n int64
	threshold := currentWindowStart.UTC().Format(time.RFC3339)
	err := s.db.Model(&ShardRecord{}).
		Where("bucket_id = ? AND state = ? AND window_start > ?", bucketID, "LAUNCHING", threshold).
		Count(&n).Error
	if err != nil {
		return 0, err
	}
	return int(n), nil
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
