package store

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
)

// The shard row model (formerly store.ShardRecord) and its row methods now
// live in the logdb package as the unified logdb.Shard type + ShardStore —
// the row model travels with the runtime object and state machine that
// operate on it (docs/plans/shard-type-collapse.md). store keeps the
// shared connection, migrations, tenant-scoping, shard_services, the
// heartbeat table, and the token helpers below.

// GenerateShardHeartbeatToken mints a fresh 256-bit random bearer token
// for a newly-christened shard. Callers store the result on the shard
// and hand the same string back to the shard via the Configure RPC.
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

// ── Shard heartbeats ────────────────────────────────────────

// ShardHeartbeat represents a shard's periodic status report.
type ShardHeartbeat struct {
	ShardID    string
	Addr       string
	InstanceID string
	State      string
	RowCount   int64
	DiskBytes  int64
	BinarySHA  string // shard binary's compile-time GitSHA; weft-admin reads it for skip-if-current. Not persisted to the heartbeat log table.
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
