// Package sqlitedb opens the one embedded SQLite database of an install.
// The SQLite stores in package sqlite do not open anything: they take this *sql.DB, so they share
// one file, one connection and, where one domain refers to another, real foreign
// keys.
//
// The driver is modernc.org/sqlite, a pure-Go translation of SQLite, so the
// processes that link this package stay cgo-free.
package sqlitedb

import (
	"database/sql"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Open opens, creating if need be, the database at path; ":memory:" gives a
// throwaway one. It runs in WAL mode with foreign keys enforced, and with one
// connection, which serializes every statement: all the isolation the stores'
// read-then-write steps need.
func Open(path string) (*sql.DB, error) {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}
