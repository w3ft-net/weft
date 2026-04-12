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
