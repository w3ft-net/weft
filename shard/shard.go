package shard

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
)

// RowSink receives rows streamed from a shard query. ComputeJob satisfies this.
type RowSink interface {
	SendRow(row *LogRow, shardID string)
	SendSentinel(shardID string)
}

// Shard is the interface the coordinator uses to interact with a shard node.
// Backed by DuckDBShard in-process today; will be a gRPC client later.
type Shard interface {
	ID() string
	WindowStart() time.Time
	WindowEnd() time.Time
	Query(sql string) ([]LogRow, error)
	// QueryToSink executes SQL and streams matching rows to the sink,
	// followed by a sentinel. Intended to run in a goroutine.
	QueryToSink(sql string, sink RowSink)
	Ingest(rows []LogRow) error
	Close() error
}

// DuckDBShard is an in-process shard backed by a local DuckDB database.
type DuckDBShard struct {
	id          string
	db          *sql.DB
	windowStart time.Time
	windowEnd   time.Time
}

const createTableSQL = `
CREATE TABLE IF NOT EXISTS logs (
    line_num    BIGINT,
    raw         TEXT,
    fields      TEXT[],
    ts          TIMESTAMP,
    level       TEXT,
    app         TEXT,
    instance    TEXT,
    customer    TEXT,
    payload     JSON
);
`

// NewDuckDBShard opens or creates a DuckDB database at path and initializes the
// logs table. Use "" or ":memory:" for an in-memory database.
func NewDuckDBShard(id, path string, start, end time.Time) (*DuckDBShard, error) {
	db, err := sql.Open("duckdb", path)
	if err != nil {
		return nil, fmt.Errorf("open duckdb %q: %w", path, err)
	}
	if _, err := db.Exec(createTableSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("create table: %w", err)
	}
	return &DuckDBShard{
		id:          id,
		db:          db,
		windowStart: start,
		windowEnd:   end,
	}, nil
}

func (s *DuckDBShard) ID() string           { return s.id }
func (s *DuckDBShard) WindowStart() time.Time { return s.windowStart }
func (s *DuckDBShard) WindowEnd() time.Time   { return s.windowEnd }

func (s *DuckDBShard) Close() error {
	return s.db.Close()
}

// QueryToSink executes SQL and streams matching rows to the sink, then
// sends a sentinel. If the query fails, the sentinel is still sent so the
// compute job doesn't hang.
func (s *DuckDBShard) QueryToSink(query string, sink RowSink) {
	defer sink.SendSentinel(s.id)

	rows, err := s.Query(query)
	if err != nil {
		return // sentinel still sent via defer
	}
	for i := range rows {
		sink.SendRow(&rows[i], s.id)
	}
}

// Ingest batch-inserts rows into the logs table.
func (s *DuckDBShard) Ingest(rows []LogRow) error {
	if len(rows) == 0 {
		return nil
	}

	var sb strings.Builder
	sb.WriteString("INSERT INTO logs (line_num, raw, fields, ts, level, app, instance, customer, payload) VALUES ")

	args := make([]any, 0, len(rows)*9)
	for i, r := range rows {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString("(?, ?, ?, ?, ?, ?, ?, ?, ?)")

		payloadStr := "{}"
		if len(r.Payload) > 0 {
			payloadStr = string(r.Payload)
		}

		args = append(args,
			r.LineNum,
			r.Raw,
			r.Fields,
			r.Ts,
			r.Level,
			r.App,
			r.Instance,
			r.Customer,
			payloadStr,
		)
	}

	_, err := s.db.Exec(sb.String(), args...)
	return err
}

// Query executes a SQL query and returns the matching rows.
func (s *DuckDBShard) Query(query string) ([]LogRow, error) {
	sqlRows, err := s.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("query shard %s: %w", s.id, err)
	}
	defer sqlRows.Close()

	var results []LogRow
	for sqlRows.Next() {
		var r LogRow
		var rawFields any
		var rawPayload any

		err := sqlRows.Scan(
			&r.LineNum,
			&r.Raw,
			&rawFields,
			&r.Ts,
			&r.Level,
			&r.App,
			&r.Instance,
			&r.Customer,
			&rawPayload,
		)
		if err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		// DuckDB returns TEXT[] as []interface{} — convert to []string.
		if arr, ok := rawFields.([]any); ok {
			for _, v := range arr {
				if sv, ok := v.(string); ok {
					r.Fields = append(r.Fields, sv)
				}
			}
		}
		// DuckDB returns JSON as map[string]interface{} — marshal back to bytes.
		if rawPayload != nil {
			b, err := json.Marshal(rawPayload)
			if err == nil {
				r.Payload = json.RawMessage(b)
			}
		}
		results = append(results, r)
	}
	return results, sqlRows.Err()
}
