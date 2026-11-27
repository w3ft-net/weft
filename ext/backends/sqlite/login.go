package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/w3ft-net/weft/backends/login"
)

// LoginBackend is a login.Store over the install's SQLite database.
type LoginBackend struct {
	db  *sql.DB
	now func() time.Time
}

var _ login.Store = (*LoginBackend)(nil)

const loginSchema = `
CREATE TABLE IF NOT EXISTS login_emails (
	email TEXT PRIMARY KEY,
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS login_links (
	token_hash TEXT PRIMARY KEY,
	email TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	used_at TEXT
);
`

// loginTimeFmt is fixed-width, so the times compare as text.
const loginTimeFmt = "2006-01-02T15:04:05.000000000Z"

// NewLoginBackend is a store over db, creating its tables if need be. db comes
// from sqlitedb.Open and is the caller's to close.
func NewLoginBackend(db *sql.DB) (*LoginBackend, error) {
	if _, err := db.Exec(loginSchema); err != nil {
		return nil, fmt.Errorf("create login schema: %w", err)
	}
	return &LoginBackend{db: db, now: time.Now}, nil
}

func (s *LoginBackend) Allowed(email string) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM login_emails WHERE email = ?`, login.Normalize(email)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// IssueLink also drops the links that can no longer be used, so the table holds
// only what is live.
func (s *LoginBackend) IssueLink(email string, ttl time.Duration) (string, error) {
	raw, err := login.NewRaw()
	if err != nil {
		return "", err
	}
	now := s.now().UTC()
	if _, err := s.db.Exec(`DELETE FROM login_links WHERE used_at IS NOT NULL OR expires_at <= ?`,
		now.Format(loginTimeFmt)); err != nil {
		return "", err
	}
	if ttl < 0 {
		ttl = 0
	}
	// A link that is not good for any time is stored already expired, so its
	// consume is refused like any other expired one.
	if _, err := s.db.Exec(`INSERT INTO login_links (token_hash, email, expires_at) VALUES (?, ?, ?)`,
		login.Hash(raw), login.Normalize(email), now.Add(ttl).Format(loginTimeFmt)); err != nil {
		return "", err
	}
	return raw, nil
}

// ConsumeLink marks the link used and reads its address in one statement, so two
// callers with one token cannot both succeed.
func (s *LoginBackend) ConsumeLink(raw string) (string, bool, error) {
	now := s.now().UTC().Format(loginTimeFmt)
	var email string
	err := s.db.QueryRow(`UPDATE login_links SET used_at = ?
		WHERE token_hash = ? AND used_at IS NULL AND expires_at > ? RETURNING email`,
		now, login.Hash(raw), now).Scan(&email)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return email, true, nil
}
