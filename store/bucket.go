package store

import (
	"errors"

	"gorm.io/gorm"
)

// Bucket persists an app-bucket's configuration: which apps it owns,
// how many INGESTING shards it should maintain, and which tier (ASG /
// instance class) those shards run on. Hydrated by the provisioner at
// startup; mutated when an app is split into a dedicated bucket or
// when an operator updates the desired count.
type Bucket struct {
	ID                   string `gorm:"primaryKey;column:id"`
	Apps                 string `gorm:"not null;column:apps"`                   // JSON array; '[]' = catch-all
	TargetIngestingCount int    `gorm:"not null;column:target_ingesting_count"`
	Tier                 string `gorm:"not null;column:tier"`
	CreatedAt            string `gorm:"not null;column:created_at"`
	// WindowDuration and RetentionPeriod are stored as Go duration
	// literals ("168h", "30m") so time.ParseDuration round-trips them.
	// Empty strings are treated as "fall back to provisioner config"
	// at the Go layer.
	WindowDuration  string `gorm:"not null;column:window_duration"`
	RetentionPeriod string `gorm:"not null;column:retention_period"`
}

// TableName pins the GORM mapping. Without this GORM would pluralise
// to "buckets" — which happens to be what we want, but be explicit.
func (Bucket) TableName() string { return "buckets" }

// ListBuckets returns every bucket. Order is not guaranteed; callers
// that need determinism should sort by ID.
func (s *Store) ListBuckets() ([]Bucket, error) {
	var out []Bucket
	err := s.db.Find(&out).Error
	return out, err
}

// GetBucket returns the bucket with the given id, or (zero, false, nil)
// when absent.
func (s *Store) GetBucket(id string) (Bucket, bool, error) {
	var b Bucket
	err := s.db.Where("id = ?", id).First(&b).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Bucket{}, false, nil
	}
	if err != nil {
		return Bucket{}, false, err
	}
	return b, true, nil
}

// SaveBucket upserts a bucket record.
func (s *Store) SaveBucket(b *Bucket) error {
	return s.db.Save(b).Error
}

// DeleteBucket removes a bucket. Fails when shard_records still
// reference it (FK is RESTRICT-by-default in the migration).
func (s *Store) DeleteBucket(id string) error {
	return s.db.Delete(&Bucket{}, "id = ?", id).Error
}
