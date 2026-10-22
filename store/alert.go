package store

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// AlertIncident is one continuous stretch of ticks during which a cron's
// alert holds, kept so a notification goes out once per stretch rather than
// once per tick. Identity is (cron, tag): in multi-row mode two tags on the
// same cron open and close independently, and Tag is "" in single-row mode.
// A cron has one alert stage, so its condition text is not part of the key.
// Which targets have been told about the incident lives in
// AlertIncidentDelivery.
type AlertIncident struct {
	ID          string    `gorm:"primaryKey"`
	CronWatchID uint      `gorm:"not null"`
	Tag         string    `gorm:"type:text;not null;default:''"`
	OpenedAt    time.Time `gorm:"not null"`
	ClosedAt    *time.Time
	LastSeenAt  time.Time `gorm:"not null"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// AlertIncidentDelivery records that a notification target (a Slack channel,
// a PagerDuty service, a webhook, an email address) has been told about an
// incident.
type AlertIncidentDelivery struct {
	IncidentID  string    `gorm:"primaryKey"`
	Target      string    `gorm:"primaryKey"`
	DeliveredAt time.Time `gorm:"not null"`
}

// OpenOrTouchIncident returns the open incident for (watch, tag), creating it
// if there is none. A tick that finds an incident already open only advances
// its LastSeenAt.
func (s *Store) OpenOrTouchIncident(watchID uint, tag string) (*AlertIncident, error) {
	now := time.Now()
	var inc AlertIncident
	err := s.db.Where("cron_watch_id = ? AND tag = ? AND closed_at IS NULL", watchID, tag).First(&inc).Error
	if err == nil {
		return &inc, s.db.Model(&inc).Updates(map[string]any{"last_seen_at": now, "updated_at": now}).Error
	}
	if err != gorm.ErrRecordNotFound {
		return nil, err
	}
	inc = AlertIncident{
		ID:          uuid.New().String(),
		CronWatchID: watchID,
		Tag:         tag,
		OpenedAt:    now,
		LastSeenAt:  now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	// A concurrent tick may have opened it first; the unique index on open
	// incidents makes that insert a no-op, and we take the winner's row.
	res := s.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&inc)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return &inc, s.db.Where("cron_watch_id = ? AND tag = ? AND closed_at IS NULL", watchID, tag).First(&inc).Error
	}
	return &inc, nil
}

// CloseIncident closes the open incident for (watch, tag), if any.
func (s *Store) CloseIncident(watchID uint, tag string) error {
	now := time.Now()
	return s.db.Model(&AlertIncident{}).
		Where("cron_watch_id = ? AND tag = ? AND closed_at IS NULL", watchID, tag).
		Updates(map[string]any{"closed_at": now, "updated_at": now}).Error
}

// IncidentDeliveries returns the targets already notified about an incident.
func (s *Store) IncidentDeliveries(incidentID string) (map[string]bool, error) {
	var rows []AlertIncidentDelivery
	if err := s.db.Where("incident_id = ?", incidentID).Find(&rows).Error; err != nil {
		return nil, err
	}
	done := make(map[string]bool, len(rows))
	for _, r := range rows {
		done[r.Target] = true
	}
	return done, nil
}

// RecordDeliveries marks targets as notified about an incident.
func (s *Store) RecordDeliveries(incidentID string, targets []string) error {
	if len(targets) == 0 {
		return nil
	}
	now := time.Now()
	rows := make([]AlertIncidentDelivery, len(targets))
	for i, t := range targets {
		rows[i] = AlertIncidentDelivery{IncidentID: incidentID, Target: t, DeliveredAt: now}
	}
	return s.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&rows).Error
}
