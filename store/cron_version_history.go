package store

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
)

// CronWatchVersion is one entry in a /cron.d watch's history,
// covering both the live row (HEAD) and history rows uniformly.
type CronWatchVersion struct {
	ContentHash       string
	Schedule          string
	Pipeline          string
	Message           string
	AuthoredAt        time.Time
	AuthoredByUserID  *string
	DisplacedByUserID *string // nil for HEAD
	IsHead            bool
}

type cronWatchHistoryRow struct {
	ID               uint64    `gorm:"column:id"`
	WatchID          uint64    `gorm:"column:watch_id"`
	CustomerID       uint      `gorm:"column:customer_id"`
	Name             string    `gorm:"column:name"`
	Schedule         string    `gorm:"column:schedule"`
	Pipeline         string    `gorm:"column:pipeline"`
	ContentHash      string    `gorm:"column:content_hash"`
	Message          string    `gorm:"column:message"`
	CapturedAt       time.Time `gorm:"column:captured_at"`
	CapturedByUserID *string   `gorm:"column:captured_by_user_id"`
	AuthoredByUserID *string   `gorm:"column:authored_by_user_id"`
	PredecessorID    *uint64   `gorm:"column:predecessor_id"`
}

func (cronWatchHistoryRow) TableName() string { return "cron_watch_history" }

// LogCronWatch returns the version chain newest-first for the named
// watch, including the live row as the first entry.
func (s *Store) LogCronWatch(customerID uint, name string) ([]CronWatchVersion, error) {
	w, err := s.GetCronWatch(customerID, name)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}

	out := []CronWatchVersion{{
		ContentHash:      w.ContentHash,
		Schedule:         w.Schedule,
		Pipeline:         w.Pipeline,
		Message:          w.Message,
		AuthoredAt:       w.UpdatedAt,
		AuthoredByUserID: w.CreatedByUserID,
		IsHead:           true,
	}}

	var rows []cronWatchHistoryRow
	err = s.db.Where("watch_id = ?", w.ID).
		Order("captured_at DESC, id DESC").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out = append(out, CronWatchVersion{
			ContentHash:       r.ContentHash,
			Schedule:          r.Schedule,
			Pipeline:          r.Pipeline,
			Message:           r.Message,
			AuthoredAt:        r.CapturedAt,
			AuthoredByUserID:  r.AuthoredByUserID,
			DisplacedByUserID: r.CapturedByUserID,
			IsHead:            false,
		})
	}
	return out, nil
}

// ResolveCronWatchRef parses a ref ("HEAD", "HEAD^N", or hash
// prefix) and returns the matching version.
func (s *Store) ResolveCronWatchRef(customerID uint, name, ref string) (*CronWatchVersion, error) {
	if m := headCaretRefRe.FindStringSubmatch(ref); m != nil {
		offset := 0
		if m[1] != "" {
			if m[2] == "" {
				offset = 1
			} else {
				n, err := strconv.Atoi(m[2])
				if err != nil || n < 0 {
					return nil, fmt.Errorf("%w: %q", ErrInvalidRef, ref)
				}
				offset = n
			}
		}
		versions, err := s.LogCronWatch(customerID, name)
		if err != nil {
			return nil, err
		}
		if offset >= len(versions) {
			return nil, fmt.Errorf("%w: HEAD^%d (only %d versions exist)",
				ErrRefNotFound, offset, len(versions))
		}
		v := versions[offset]
		return &v, nil
	}
	if len(ref) < 4 {
		return nil, fmt.Errorf("%w: hash prefix must be at least 4 characters", ErrInvalidRef)
	}
	if !isHexString(ref) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidRef, ref)
	}
	versions, err := s.LogCronWatch(customerID, name)
	if err != nil {
		return nil, err
	}
	var matches []CronWatchVersion
	for _, v := range versions {
		if strings.HasPrefix(v.ContentHash, ref) {
			matches = append(matches, v)
		}
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrRefNotFound, ref)
	}
	if len(matches) > 1 {
		cands := make([]string, len(matches))
		for i, m := range matches {
			cands[i] = m.ContentHash[:7]
		}
		return nil, fmt.Errorf("%w: %s matches %s",
			ErrAmbiguousHash, ref, strings.Join(cands, ", "))
	}
	return &matches[0], nil
}

// CronWatchContent returns the (schedule, pipeline) at the given ref.
func (s *Store) CronWatchContent(customerID uint, name, ref string) (schedule, pipeline string, err error) {
	v, err := s.ResolveCronWatchRef(customerID, name, ref)
	if err != nil {
		return "", "", err
	}
	return v.Schedule, v.Pipeline, nil
}

// CheckoutCronWatch makes the ref's version the new HEAD.
func (s *Store) CheckoutCronWatch(customerID uint, userID, name, ref string) error {
	target, err := s.ResolveCronWatchRef(customerID, name, ref)
	if err != nil {
		return err
	}
	if target.IsHead {
		return nil
	}
	compiled, err := CompileWatchPipeline(target.Pipeline)
	if err != nil {
		return err
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		// Always set, even for empty — see SaveShellFunction.
		if err := tx.Exec("SELECT set_config('weft.acting_user_id', ?, true)", userID).Error; err != nil {
			return err
		}
		updates := map[string]any{
			"schedule":        target.Schedule,
			"pipeline":        target.Pipeline,
			"content_hash":    target.ContentHash,
			"message":         target.Message,
			"alert_key":       compiled.AlertKey,
			"alert_tag":       compiled.AlertTag,
			"alert_condition": compiled.AlertCondition,
			"alert_threshold": compiled.AlertThreshold,
			"updated_at":      time.Now(),
		}
		if target.AuthoredByUserID != nil {
			updates["created_by_user_id"] = *target.AuthoredByUserID
		}
		return tx.Model(&CronWatch{}).
			Where("customer_id = ? AND name = ?", customerID, name).
			Updates(updates).Error
	})
}

// AmendCronWatchMessage sets the commit message on the version at
// `ref`. For HEAD this UPDATEs cron_watches.message — the trigger
// is scoped to UPDATE OF schedule, pipeline so a message-only edit
// doesn't capture a new history row. For history refs, it UPDATEs
// the matching cron_watch_history row directly.
func (s *Store) AmendCronWatchMessage(customerID uint, name, ref, message string) error {
	v, err := s.ResolveCronWatchRef(customerID, name, ref)
	if err != nil {
		return err
	}
	if v.IsHead {
		return s.db.Model(&CronWatch{}).
			Where("customer_id = ? AND name = ?", customerID, name).
			Update("message", message).Error
	}
	w, err := s.GetCronWatch(customerID, name)
	if err != nil {
		return err
	}
	return s.db.Model(&cronWatchHistoryRow{}).
		Where("watch_id = ? AND content_hash = ? AND captured_at = ?",
			w.ID, v.ContentHash, v.AuthoredAt).
		Update("message", message).Error
}
