package store

import (
	"time"
)

// DeviceCode represents a pending CLI authorization request.
type DeviceCode struct {
	ID         uint    `gorm:"primaryKey"`
	DeviceCode string  `gorm:"uniqueIndex;not null"` // random, used by CLI to poll
	UserCode   string  `gorm:"uniqueIndex;not null"` // short, displayed to user
	UserID     *string // set when approved
	ExpiresAt  time.Time
	CreatedAt  time.Time
}
