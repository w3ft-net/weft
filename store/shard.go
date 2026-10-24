package store

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"time"
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
	BinarySHA  string // shard binary's compile-time GitSHA; weft-admin reads it for skip-if-current. Not persisted to the heartbeat table.
	// Age is how long before it was read this beat was recorded, by the
	// database's own clock. Zero for a beat just received over RPC. Carried as
	// an age rather than a timestamp so replaying it needs no agreement
	// between the coordinator's clock and the database's.
	Age time.Duration
	// Spare marks a warm-pool instance's beat (it has no volume and no shard
	// id). Spare beats refresh the pool's shard_spares row and are never
	// recorded here or dispatched as a shard's.
	Spare bool
}

// RecordHeartbeat upserts the latest beat for this source. The table is a
// "who is alive right now" view, not a log: one row per (shard_id,
// instance_id), where an unclaimed instance beats with an empty shard_id.
// Raw SQL because the table isn't managed by GORM AutoMigrate.
func (s *Store) RecordHeartbeat(hb *ShardHeartbeat) error {
	return s.db.Exec(`
		INSERT INTO shard_heartbeats (shard_id, instance_id, addr, state, row_count, disk_bytes, beat_at)
		VALUES (?, ?, ?, ?, ?, ?, NOW())
		ON CONFLICT (shard_id, instance_id) DO UPDATE SET
			addr = EXCLUDED.addr, state = EXCLUDED.state, row_count = EXCLUDED.row_count,
			disk_bytes = EXCLUDED.disk_bytes, beat_at = EXCLUDED.beat_at`,
		hb.ShardID, hb.InstanceID, hb.Addr, hb.State, hb.RowCount, hb.DiskBytes).Error
}

// LiveShardHeartbeats returns the beats within the given duration: the newest
// per claimed shard (a replaced instance's old row must not resurface its dead
// addr), and every unclaimed instance's own beat. A shard with no beat in this
// window is considered dead.
func (s *Store) LiveShardHeartbeats(since string) ([]ShardHeartbeat, error) {
	var rows []struct {
		ShardHeartbeat
		AgeSeconds float64
	}
	err := s.db.Raw(`
		(SELECT DISTINCT ON (shard_id)
			shard_id, addr, instance_id, state, row_count, disk_bytes,
			EXTRACT(EPOCH FROM NOW() - beat_at)::float8 AS age_seconds
		FROM shard_heartbeats
		WHERE shard_id <> '' AND beat_at > NOW() - ?::interval
		ORDER BY shard_id, beat_at DESC)
		UNION ALL
		(SELECT shard_id, addr, instance_id, state, row_count, disk_bytes,
			EXTRACT(EPOCH FROM NOW() - beat_at)::float8 AS age_seconds
		FROM shard_heartbeats
		WHERE shard_id = '' AND beat_at > NOW() - ?::interval)`,
		since, since).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	results := make([]ShardHeartbeat, len(rows))
	for i, r := range rows {
		results[i] = r.ShardHeartbeat
		results[i].Age = time.Duration(r.AgeSeconds * float64(time.Second))
	}
	return results, nil
}

// PruneShardHeartbeats deletes rows not refreshed within maxAge: replaced
// instances, unclaimed instances that have since been christened, and shards
// that are gone.
func (s *Store) PruneShardHeartbeats(maxAge time.Duration) error {
	return s.db.Exec(`DELETE FROM shard_heartbeats WHERE beat_at < NOW() - make_interval(secs => ?)`,
		maxAge.Seconds()).Error
}
