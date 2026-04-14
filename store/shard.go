package store

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
}

// SaveShard upserts a shard record.
func (s *Store) SaveShard(rec *ShardRecord) error {
	return s.db.Save(rec).Error
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
	ShardID     string
	Addr        string
	InstanceID  string
	Apps        string // JSON array
	WindowStart string // RFC3339
	WindowEnd   string // RFC3339
	State       string
	RowCount    int64
	DiskBytes   int64
	BeatAt      string // RFC3339
}

// InsertHeartbeat writes a heartbeat row. Uses raw SQL since the table
// is partitioned and not managed by GORM AutoMigrate.
func (s *Store) InsertHeartbeat(hb *ShardHeartbeat) error {
	return s.db.Exec(`
		INSERT INTO shard_heartbeats (shard_id, addr, instance_id, apps, window_start, window_end, state, row_count, disk_bytes, beat_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NOW())`,
		hb.ShardID, hb.Addr, hb.InstanceID, hb.Apps, hb.WindowStart, hb.WindowEnd, hb.State, hb.RowCount, hb.DiskBytes).Error
}

// LiveShardHeartbeats returns the most recent heartbeat per shard
// where beat_at is within the given duration. A shard with no heartbeat
// in this window is considered dead.
func (s *Store) LiveShardHeartbeats(since string) ([]ShardHeartbeat, error) {
	var results []ShardHeartbeat
	err := s.db.Raw(`
		SELECT DISTINCT ON (shard_id)
			shard_id, addr, instance_id, apps, window_start, window_end, state, row_count, disk_bytes, beat_at
		FROM shard_heartbeats
		WHERE beat_at > NOW() - ?::interval
		ORDER BY shard_id, beat_at DESC`,
		since).Scan(&results).Error
	return results, err
}
