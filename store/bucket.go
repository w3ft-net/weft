package store

import (
	"errors"

	"gorm.io/gorm"
)

// Bucket persists an app-bucket's configuration. ID is a surrogate
// bigint assigned by Postgres (migration 022); Label is the stable
// human-readable name (unique) that code looks up by when it needs
// "the default bucket" or "the dedicated-vimgym bucket" without
// caring about the numeric key.
type Bucket struct {
	ID                   int64  `gorm:"primaryKey;column:id"`
	Label                string `gorm:"not null;uniqueIndex;column:label"`
	Apps                 string `gorm:"not null;column:apps"` // JSON array; '[]' = catch-all
	TargetIngestingCount int    `gorm:"not null;column:target_ingesting_count"`
	Tier                 string `gorm:"not null;column:tier"`
	CreatedAt            string `gorm:"not null;column:created_at"`
	WindowDuration       string `gorm:"not null;column:window_duration"`
	RetentionPeriod      string `gorm:"not null;column:retention_period"`
}

func (Bucket) TableName() string { return "buckets" }

// ListBuckets returns every bucket. Order is not guaranteed; callers
// that need determinism should sort.
func (s *Store) ListBuckets() ([]Bucket, error) {
	var out []Bucket
	err := s.db.Find(&out).Error
	return out, err
}

// GetBucket returns the bucket with the given surrogate id.
func (s *Store) GetBucket(id int64) (Bucket, bool, error) {
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

// GetBucketByLabel resolves a human-readable bucket name to the full
// record. The canonical day-1 label is "default"; dedicated buckets
// use "dedicated-<app>". Unique index ensures at most one row matches.
func (s *Store) GetBucketByLabel(label string) (Bucket, bool, error) {
	var b Bucket
	err := s.db.Where("label = ?", label).First(&b).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Bucket{}, false, nil
	}
	if err != nil {
		return Bucket{}, false, err
	}
	return b, true, nil
}

// SaveBucket upserts a bucket. For new buckets (ID == 0) GORM
// INSERTs and assigns the surrogate id back to the struct; for
// existing (ID > 0) it UPDATEs in place.
func (s *Store) SaveBucket(b *Bucket) error {
	return s.db.Save(b).Error
}

// DeleteBucket removes a bucket by surrogate id. Fails when
// shard_records still reference it (FK is RESTRICT-by-default).
func (s *Store) DeleteBucket(id int64) error {
	return s.db.Delete(&Bucket{}, "id = ?", id).Error
}
