package logdb

import (
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Spare is one warm-pool instance (table shard_spares, migration 104): a
// booted shard instance with no volume, waiting to be claimed by an
// upgrade, a recovery, or a mint. See docs/plans/shard-seamless-upgrade.md.
type Spare struct {
	InstanceID      string     `gorm:"primaryKey;column:instance_id"`
	Tier            string     `gorm:"not null;column:tier"`
	AZ              string     `gorm:"not null;column:az"`
	ImageID         string     `gorm:"not null;default:'';column:image_id"`
	Addr            string     `gorm:"not null;default:'';column:addr"`
	BinarySHA       string     `gorm:"not null;default:'';column:binary_sha"`
	LaunchedAt      time.Time  `gorm:"not null;column:launched_at"`
	LastHeartbeatAt *time.Time `gorm:"column:last_heartbeat_at"`
	ClaimedBy       *string    `gorm:"column:claimed_by"`
	ClaimedAt       *time.Time `gorm:"column:claimed_at"`
}

func (*Spare) TableName() string { return "shard_spares" }

// claimed reports whether a path holds this spare.
func (sp *Spare) claimed() bool { return sp.ClaimedBy != nil }

// heartbeatSince reports whether the spare has heartbeat at or after t.
func (sp *Spare) heartbeatSince(t time.Time) bool {
	return sp.LastHeartbeatAt != nil && !sp.LastHeartbeatAt.Before(t)
}

// SpareStore is the shard_spares persistence the pool needs. ShardStore
// satisfies it over Postgres.
type SpareStore interface {
	ListSpares() ([]Spare, error)
	InsertSpare(sp *Spare) error
	DeleteSpare(instanceID string) error
	// ClaimSpare takes an unclaimed spare matching tier, AZ and image for
	// shardID, preferring one that has heartbeat, and returns it; nil when
	// none matches. A spare shardID already holds is returned as is, so a
	// path that crashed after claiming gets the same spare back.
	ClaimSpare(shardID, tier, az, imageID string, at time.Time) (*Spare, error)
	// SpareClaimedBy returns the spare shardID holds, or nil.
	SpareClaimedBy(shardID string) (*Spare, error)
	// ObserveSpareHeartbeat records a spare's heartbeat. Any coordinator may
	// receive it; the row is the shared record.
	ObserveSpareHeartbeat(instanceID, addr, binarySHA string, at time.Time) error
}

var _ SpareStore = ShardStore{}

func (s ShardStore) ListSpares() ([]Spare, error) {
	var out []Spare
	err := s.db().Order("launched_at").Find(&out).Error
	return out, err
}

func (s ShardStore) InsertSpare(sp *Spare) error {
	return s.db().Clauses(clause.OnConflict{DoNothing: true}).Create(sp).Error
}

func (s ShardStore) DeleteSpare(instanceID string) error {
	return s.db().Delete(&Spare{}, "instance_id = ?", instanceID).Error
}

func (s ShardStore) SpareClaimedBy(shardID string) (*Spare, error) {
	var sp Spare
	err := s.db().Where("claimed_by = ?", shardID).Order("claimed_at").First(&sp).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sp, nil
}

// ClaimSpare is one conditional UPDATE: SKIP LOCKED lets concurrent
// claimants each take a different row instead of queueing on the same one,
// and the claimed_by IS NULL predicate in the subquery means a row can be
// claimed once.
func (s ShardStore) ClaimSpare(shardID, tier, az, imageID string, at time.Time) (*Spare, error) {
	if held, err := s.SpareClaimedBy(shardID); err != nil || held != nil {
		return held, err
	}
	var out []Spare
	err := s.db().Raw(`
		UPDATE shard_spares SET claimed_by = ?, claimed_at = ?
		WHERE instance_id = (
			SELECT instance_id FROM shard_spares
			WHERE claimed_by IS NULL AND tier = ? AND az = ? AND image_id = ?
			ORDER BY (last_heartbeat_at IS NULL), launched_at
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING *`, shardID, at, tier, az, imageID).Scan(&out).Error
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return &out[0], nil
}

func (s ShardStore) ObserveSpareHeartbeat(instanceID, addr, binarySHA string, at time.Time) error {
	return s.db().Model(&Spare{}).Where("instance_id = ?", instanceID).
		Updates(map[string]any{"addr": addr, "binary_sha": binarySHA, "last_heartbeat_at": at}).Error
}

// IngestionErrorCount is how many batches ingest has failed since the table
// began: a monotonic counter, so a test's before/after difference is what
// the test cost. Every DLQ'd batch has a row.
func (s ShardStore) IngestionErrorCount() (int64, error) {
	var n int64
	err := s.db().Raw("SELECT count(*) FROM ingestion_errors").Scan(&n).Error
	return n, err
}
