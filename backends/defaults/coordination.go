package defaults

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/w3ft-net/weft/backends/coordination"
)

// CoordinationBackend is a coordination.Coordination over the install's SQLite database.
type CoordinationBackend struct {
	db *sql.DB
}

var _ coordination.Coordination = (*CoordinationBackend)(nil)

// NewCoordinationBackend is a coordination over db, creating its table if need be. db comes from
// sqlitedb.Open and is the caller's to close.
func NewCoordinationBackend(db *sql.DB) (*CoordinationBackend, error) {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS cron_claims (
		key        TEXT PRIMARY KEY,
		holder     TEXT NOT NULL,
		claimed_at INTEGER NOT NULL
	) WITHOUT ROWID`); err != nil {
		return nil, fmt.Errorf("create claims table: %w", err)
	}
	return &CoordinationBackend{db: db}, nil
}

// AcquireLease grants every lease: this process is the only coordinator.
func (*CoordinationBackend) AcquireLease(string, string, time.Duration) (bool, error) {
	return true, nil
}

// ReleaseLease does nothing; there is no lease to give up.
func (*CoordinationBackend) ReleaseLease(string, string) error { return nil }

// HeldLeases is empty: the process holds everything and keeps no record.
func (*CoordinationBackend) HeldLeases(context.Context, string) ([]string, error) { return nil, nil }

// ClaimCronSlot records the claim. Exactly one caller per key ever sees a row
// inserted, in this process or after a restart.
func (l *CoordinationBackend) ClaimCronSlot(key, holder string) (bool, error) {
	res, err := l.db.Exec(
		`INSERT INTO cron_claims (key, holder, claimed_at) VALUES (?, ?, ?) ON CONFLICT (key) DO NOTHING`,
		key, holder, time.Now().UnixNano())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DeleteCronClaimsBefore removes claims older than cutoff.
func (l *CoordinationBackend) DeleteCronClaimsBefore(cutoff time.Time) (int64, error) {
	res, err := l.db.Exec(`DELETE FROM cron_claims WHERE claimed_at < ?`, cutoff.UnixNano())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
