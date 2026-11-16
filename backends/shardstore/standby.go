package shardstore

import "time"

// Standby is one warm-pool instance (table shard_spares, migration 104): a
// booted shard instance with no volume, waiting to be claimed by an
// upgrade, a recovery, or a mint.
type Standby struct {
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

func (*Standby) TableName() string { return "shard_spares" }

// Claimed reports whether a path holds this standby.
func (sp *Standby) Claimed() bool { return sp.ClaimedBy != nil }

// HeartbeatSince reports whether the standby has heartbeat at or after t.
func (sp *Standby) HeartbeatSince(t time.Time) bool {
	return sp.LastHeartbeatAt != nil && !sp.LastHeartbeatAt.Before(t)
}

// StandbyStore is the shard_spares persistence the pool needs. The hosted
// build satisfies it over Postgres (logdb/shardpg).
type StandbyStore interface {
	ListStandbys() ([]Standby, error)
	InsertStandby(sp *Standby) error
	DeleteStandby(instanceID string) error
	// ClaimStandby takes an unclaimed standby matching tier, AZ and image for
	// shardID, preferring one that has heartbeat, and returns it; nil when
	// none matches. A standby shardID already holds is returned as is, so a
	// path that crashed after claiming gets the same standby back.
	ClaimStandby(shardID, tier, az, imageID string, at time.Time) (*Standby, error)
	// StandbyClaimedBy returns the standby shardID holds, or nil.
	StandbyClaimedBy(shardID string) (*Standby, error)
	// ObserveStandbyHeartbeat records a standby's heartbeat. Any coordinator may
	// receive it; the row is the shared record.
	ObserveStandbyHeartbeat(instanceID, addr, binarySHA string, at time.Time) error
}
