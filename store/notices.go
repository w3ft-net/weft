package store

import (
	"errors"
	"fmt"
)

// HasSeenNotice reports whether the given user has already been
// shown the notice keyed by `noticeKey`. Returns false (and no error)
// when no row exists.
func (s *Store) HasSeenNotice(userID, noticeKey string) (bool, error) {
	var count int64
	err := s.db.Table("user_notices").
		Where("user_id = ? AND notice_key = ?", userID, noticeKey).
		Count(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// MarkNoticeSeen records that a user has been shown a notice.
// Idempotent — re-marking an already-seen notice doesn't error and
// doesn't move the seen_at timestamp.
//
// `kind` must be one of "success", "warning", "error" (matching the
// notice_kind enum in migration 033). Other values fail with a clear
// error to surface registry/db drift.
func (s *Store) MarkNoticeSeen(userID, noticeKey, kind string) error {
	if kind != "success" && kind != "warning" && kind != "error" {
		return fmt.Errorf("invalid notice kind %q (want success/warning/error)", kind)
	}
	return s.db.Exec(`
		INSERT INTO user_notices (user_id, notice_key, kind, seen_at)
		VALUES (?, ?, ?::notice_kind, NOW())
		ON CONFLICT (user_id, notice_key) DO NOTHING
	`, userID, noticeKey, kind).Error
}

// errInvalidNoticeKind is exported via errors.Is for callers that
// want to distinguish kind-validation failures from DB errors.
var errInvalidNoticeKind = errors.New("invalid notice kind")
