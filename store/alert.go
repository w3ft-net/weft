package store

import (
	"time"
)

// AlertIncident tracks open/closed alert states for deduplication. Incident
// identity is (watch, condition, tag): in multi-row mode two tags on the
// same watch+condition open and close independently. Tag is "" in single-row
// mode, which keeps the single-row behavior (one open incident per
// watch+condition) as a special case of the general rule.
type AlertIncident struct {
	ID          string    `gorm:"primaryKey"`
	CronWatchID uint      `gorm:"not null"`
	Condition   string    `gorm:"not null"`
	Tag         string    `gorm:"type:text;not null;default:''"`
	OpenedAt    time.Time `gorm:"not null"`
	ClosedAt    *time.Time
	LastSeenAt  time.Time `gorm:"not null"`
	NotifiedAt  *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
