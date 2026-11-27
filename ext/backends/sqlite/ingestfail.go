package sqlite

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/w3ft-net/weft/backends/ingestfail"
	"github.com/w3ft-net/weft/shape"
)

// IngestFailBackend is an ingestfail.IngestFailBackend over the install's SQLite database.
type IngestFailBackend struct{ db *sql.DB }

var _ ingestfail.Store = (*IngestFailBackend)(nil)

const ingestFailSchema = `
CREATE TABLE IF NOT EXISTS ingest_failures (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	occurred_at TEXT NOT NULL,
	shard_id TEXT NOT NULL,
	message TEXT NOT NULL,
	row_count INTEGER NOT NULL,
	services TEXT NOT NULL,
	dlq_path TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS ingest_failures_at ON ingest_failures (occurred_at);
`

// NewIngestFailBackend is a store over db, creating its table if need be. db comes from
// sqlitedb.Open and is the caller's to close.
func NewIngestFailBackend(db *sql.DB) (*IngestFailBackend, error) {
	if _, err := db.Exec(ingestFailSchema); err != nil {
		return nil, fmt.Errorf("create ingest failure schema: %w", err)
	}
	return &IngestFailBackend{db: db}, nil
}

// Times are stored as fixed-width UTC text, so they order as text.
const ingestFailTimeFmt = "2006-01-02T15:04:05.000000000Z"

func (s *IngestFailBackend) Record(f ingestfail.Failure) error {
	if f.At.IsZero() {
		f.At = time.Now()
	}
	services, err := json.Marshal(f.Services)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO ingest_failures (occurred_at, shard_id, message, row_count, services, dlq_path)
		VALUES (?, ?, ?, ?, ?, ?)`, f.At.UTC().Format(ingestFailTimeFmt), f.ShardID, f.Message, f.Rows, string(services), f.DLQPath)
	return err
}

func (s *IngestFailBackend) AffectedServices(_ shape.Added, since time.Time) ([]string, error) {
	rows, err := s.db.Query(`SELECT services FROM ingest_failures WHERE occurred_at >= ?`, since.UTC().Format(ingestFailTimeFmt))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	out := []string{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var services []string
		if err := json.Unmarshal([]byte(raw), &services); err != nil {
			return nil, err
		}
		for _, svc := range services {
			if !seen[svc] {
				seen[svc] = true
				out = append(out, svc)
			}
		}
	}
	sort.Strings(out)
	return out, rows.Err()
}
