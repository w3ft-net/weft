package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// user_notices is a delivery queue, not a suppression ledger: one row
// per notice *instance*, `seen_at IS NULL` meaning pending delivery.
// Emitters queue; readers drain. See migration 071 for why — briefly,
// a trigger firing outside any request has no response to attach a
// notice to, so it needs somewhere to leave one.
//
// There is deliberately no per-key uniqueness and no "has this user
// seen X" primitive. A notice that shouldn't fire twice is a notice
// whose emitter shouldn't call it twice.

// PendingNotice is one undelivered notice instance. Params carries the
// facts a renderer interpolates; the prose lives in the registry in
// coordinator/notices.go, because the browser and the TUI render
// differently.
type PendingNotice struct {
	ID        int64
	Key       string
	Kind      string
	Params    map[string]any
	CreatedAt time.Time
}

// errInvalidNoticeKind marks a kind that isn't one of the notice_kind
// enum values. Wrapped into the returned error so callers can tell
// registry/schema drift from a database failure.
var errInvalidNoticeKind = errors.New("invalid notice kind")

// EmitNotice queues a notice for a user. It is delivered on the user's
// next drain — the next shell request for the TUI, the next poll for
// the browser — and marked seen at that point.
//
// `kind` must be one of "success", "warning", "error" (the notice_kind
// enum from migration 033). `params` may be nil.
func (s *Store) EmitNotice(userID, noticeKey, kind string, params map[string]any) error {
	if kind != "success" && kind != "warning" && kind != "error" {
		return fmt.Errorf("%w %q (want success/warning/error)", errInvalidNoticeKind, kind)
	}
	var encoded any
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("encode notice params: %w", err)
		}
		encoded = string(b)
	}
	return s.db.Exec(`
		INSERT INTO user_notices (user_id, notice_key, kind, params, created_at)
		VALUES (?, ?, ?::notice_kind, ?::jsonb, NOW())
	`, userID, noticeKey, kind, encoded).Error
}

// PendingNotices returns the user's undelivered notices, oldest first.
// Reading does not deliver — the caller marks what it actually rendered
// via MarkNoticesSeen, so a dropped connection costs one repeat rather
// than the only copy of the notice.
func (s *Store) PendingNotices(userID string) ([]PendingNotice, error) {
	rows, err := s.db.Raw(`
		SELECT id, notice_key, kind::text, params, created_at
		  FROM user_notices
		 WHERE user_id = ? AND seen_at IS NULL
		 ORDER BY id
	`, userID).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PendingNotice
	for rows.Next() {
		var (
			n   PendingNotice
			raw []byte
		)
		if err := rows.Scan(&n.ID, &n.Key, &n.Kind, &raw, &n.CreatedAt); err != nil {
			return nil, err
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &n.Params); err != nil {
				return nil, fmt.Errorf("decode params for notice %d: %w", n.ID, err)
			}
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// MarkNoticesSeen stamps the given instances delivered. Scoped to ids
// rather than (user, key) so draining one instance never burns another
// that happens to share its key. An empty slice is a no-op.
func (s *Store) MarkNoticesSeen(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	return s.db.Exec(`
		UPDATE user_notices SET seen_at = NOW()
		 WHERE id IN ? AND seen_at IS NULL
	`, ids).Error
}
