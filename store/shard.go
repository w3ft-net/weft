package store

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// ShardRecord persists shard registry entries across coordinator restarts.
type ShardRecord struct {
	ID          string `gorm:"primaryKey"`
	Apps        string `gorm:"not null"` // JSON array: ["vimgym","callbox"]
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
	// Heartbeat RPC. Minted by the provisioner at christen, returned to
	// the shard in the Configure response, and persisted in shard.json.
	// NULL for shards created before migration 015 — the Heartbeat
	// handler accepts them during the transition window and logs a
	// warning so the fleet can roll naturally.
	HeartbeatToken string
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
// time. Returns false if either side is empty so callers don't need a
// separate check — "empty vs empty equals true" would be a trap since
// the Heartbeat handler treats empty-token as a grandfather fallback.
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

// ListShardsByState returns shard records with the given state.
func (s *Store) ListShardsByState(state string) ([]ShardRecord, error) {
	var records []ShardRecord
	err := s.db.Where("state = ?", state).Find(&records).Error
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
