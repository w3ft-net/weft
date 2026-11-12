// Package local is the open build's coordination: the one coordinator is the
// leader of everything, and its cron claims live in an embedded SQLite file.
//
// It ships no scaling mechanism. Leases are not state: every acquire is
// granted, so a restarted process leads at once and a lease never has to
// expire. Claims are state, because a coordinator that restarts inside a
// cron's slot must not fire the slot again; each is one INSERT whose rows
// affected is the answer.
//
// The driver is modernc.org/sqlite (BSD-3-Clause), a pure-Go translation of
// SQLite, so the processes that link this package stay cgo-free.
package local

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/graybeardcorp/weft/coordination"
)

// Local is a coordination.Coordination over one SQLite file.
type Local struct {
	db *sql.DB
}

var _ coordination.Coordination = (*Local)(nil)

// Open opens, creating if need be, the claims file at path. The caller decides
// where it lives. One writer connection serializes statements, which is all
// the atomicity a claim needs; the file is in WAL mode so a reader (a backup,
// sqlite3 on the command line) never blocks it.
func Open(path string) (*Local, error) {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("open claims %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS cron_claims (
		key        TEXT PRIMARY KEY,
		holder     TEXT NOT NULL,
		claimed_at INTEGER NOT NULL
	) WITHOUT ROWID`); err != nil {
		db.Close()
		return nil, fmt.Errorf("create claims table in %s: %w", path, err)
	}
	return &Local{db: db}, nil
}

// Close closes the file.
func (l *Local) Close() error { return l.db.Close() }

// AcquireLease grants every lease: this process is the only coordinator.
func (*Local) AcquireLease(string, string, time.Duration) (bool, error) { return true, nil }

// ReleaseLease does nothing; there is no lease to give up.
func (*Local) ReleaseLease(string, string) error { return nil }

// HeldLeases is empty: the process holds everything and keeps no record.
func (*Local) HeldLeases(context.Context, string) ([]string, error) { return nil, nil }

// ClaimCronSlot records the claim. Exactly one caller per key ever sees a row
// inserted, in this process or after a restart.
func (l *Local) ClaimCronSlot(key, holder string) (bool, error) {
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
func (l *Local) DeleteCronClaimsBefore(cutoff time.Time) (int64, error) {
	res, err := l.db.Exec(`DELETE FROM cron_claims WHERE claimed_at < ?`, cutoff.UnixNano())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
