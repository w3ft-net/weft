package defaults

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/w3ft-net/weft/backends/notice"
)

// NoticeBackend is a notice.NoticeBackend over the install's SQLite database.
type NoticeBackend struct{ db *sql.DB }

var _ notice.Store = (*NoticeBackend)(nil)

const noticeSchema = `
CREATE TABLE IF NOT EXISTS user_notices (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id TEXT NOT NULL,
	notice_key TEXT NOT NULL,
	kind TEXT NOT NULL,
	params TEXT,
	created_at TEXT NOT NULL,
	seen_at TEXT
);
CREATE INDEX IF NOT EXISTS user_notices_pending ON user_notices (user_id) WHERE seen_at IS NULL;
`

// NewNoticeBackend is a store over db, creating its table if need be. db comes from
// sqlitedb.Open and is the caller's to close.
func NewNoticeBackend(db *sql.DB) (*NoticeBackend, error) {
	if _, err := db.Exec(noticeSchema); err != nil {
		return nil, fmt.Errorf("create notice schema: %w", err)
	}
	return &NoticeBackend{db: db}, nil
}

const noticeTimeFmt = "2006-01-02T15:04:05.000000000Z"

func (s *NoticeBackend) Emit(userID, key, kind string, params map[string]any) error {
	switch kind {
	case notice.KindSuccess, notice.KindWarning, notice.KindError:
	default:
		return fmt.Errorf("%w %q (want success/warning/error)", notice.ErrInvalidKind, kind)
	}
	var encoded any
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("encode notice params: %w", err)
		}
		encoded = string(b)
	}
	_, err := s.db.Exec(`INSERT INTO user_notices (user_id, notice_key, kind, params, created_at) VALUES (?, ?, ?, ?, ?)`,
		userID, key, kind, encoded, time.Now().UTC().Format(noticeTimeFmt))
	return err
}

func (s *NoticeBackend) Pending(userID string) ([]notice.Pending, error) {
	rows, err := s.db.Query(`SELECT id, notice_key, kind, params, created_at FROM user_notices
		WHERE user_id = ? AND seen_at IS NULL ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []notice.Pending{}
	for rows.Next() {
		var (
			n       notice.Pending
			raw     sql.NullString
			created string
		)
		if err := rows.Scan(&n.ID, &n.Key, &n.Kind, &raw, &created); err != nil {
			return nil, err
		}
		if raw.Valid && raw.String != "" {
			if err := json.Unmarshal([]byte(raw.String), &n.Params); err != nil {
				return nil, fmt.Errorf("decode params for notice %d: %w", n.ID, err)
			}
		}
		if n.CreatedAt, err = time.Parse(noticeTimeFmt, created); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *NoticeBackend) MarkSeen(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := []any{time.Now().UTC().Format(noticeTimeFmt)}
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := s.db.Exec(`UPDATE user_notices SET seen_at = ? WHERE id IN (`+marks+`) AND seen_at IS NULL`, args...)
	return err
}
