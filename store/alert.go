package store

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// AlertIncident tracks open/closed alert states for deduplication. Incident
// identity is (watch, condition, tag): in multi-row mode two tags on the
// same watch+condition open and close independently. Tag is "" in single-row
// mode, which keeps the single-row behavior (one open incident per
// watch+condition) as a special case of the general rule.
type AlertIncident struct {
	ID          string     `gorm:"primaryKey"`
	CronWatchID uint       `gorm:"not null"`
	Condition   string     `gorm:"not null"`
	Tag         string     `gorm:"type:text;not null;default:''"`
	OpenedAt    time.Time  `gorm:"not null"`
	ClosedAt    *time.Time
	LastSeenAt  time.Time  `gorm:"not null"`
	NotifiedAt  *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// OpenIncident finds an open (unclosed) incident for this watch + condition
// + tag.
func (s *Store) OpenIncident(watchID uint, condition, tag string) (*AlertIncident, error) {
	var inc AlertIncident
	err := s.db.Where("cron_watch_id = ? AND condition = ? AND tag = ? AND closed_at IS NULL",
		watchID, condition, tag).First(&inc).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &inc, nil
}

// CreateIncident opens a new incident for (watch, condition, tag).
func (s *Store) CreateIncident(watchID uint, condition, tag string) (*AlertIncident, error) {
	now := time.Now()
	inc := AlertIncident{
		ID:          uuid.New().String(),
		CronWatchID: watchID,
		Condition:   condition,
		Tag:         tag,
		OpenedAt:    now,
		LastSeenAt:  now,
		NotifiedAt:  &now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.db.Create(&inc).Error; err != nil {
		return nil, err
	}
	return &inc, nil
}

// CloseIncident marks an incident as resolved.
func (s *Store) CloseIncident(id string) error {
	now := time.Now()
	return s.db.Model(&AlertIncident{}).Where("id = ?", id).Updates(map[string]any{
		"closed_at":  &now,
		"updated_at": now,
	}).Error
}

// UpdateIncidentSeen bumps the LastSeenAt timestamp.
func (s *Store) UpdateIncidentSeen(id string) error {
	now := time.Now()
	return s.db.Model(&AlertIncident{}).Where("id = ?", id).Updates(map[string]any{
		"last_seen_at": now,
		"updated_at":   now,
	}).Error
}

// ListActiveIncidents returns all open incidents for watches owned by a customer.
func (s *Store) ListActiveIncidents(customerID uint) ([]AlertIncident, error) {
	var incidents []AlertIncident
	err := s.db.
		Joins("JOIN cron_watches ON alert_incidents.cron_watch_id = cron_watches.id").
		Where("cron_watches.customer_id = ? AND alert_incidents.closed_at IS NULL", customerID).
		Order("alert_incidents.opened_at DESC").
		Find(&incidents).Error
	return incidents, err
}

// ListIncidentHistory returns recent incidents for a watch.
func (s *Store) ListIncidentHistory(watchID uint, limit int) ([]AlertIncident, error) {
	var incidents []AlertIncident
	err := s.db.Where("cron_watch_id = ?", watchID).
		Order("opened_at DESC").Limit(limit).Find(&incidents).Error
	return incidents, err
}
