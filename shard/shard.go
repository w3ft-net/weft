package shard

import (
	"database/sql"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// RowSink receives rows streamed from a shard query. ComputeJob satisfies this.
type RowSink interface {
	SendRow(row *pb.LogRow, shardID string)
	SendSentinel(shardID string)
}

// Shard is the interface the coordinator uses to interact with a shard node.
type Shard interface {
	ID() string
	WindowStart() time.Time
	WindowEnd() time.Time
	Query(sql string) ([]*pb.LogRow, error)
	QueryToSink(sql string, sink RowSink)
	Ingest(rows []*pb.LogRow) error
	Close() error
}

// DuckDBShard is an in-process shard backed by a local DuckDB database.
type DuckDBShard struct {
	id          string
	db          *sql.DB
	windowStart time.Time
	windowEnd   time.Time
	rowCount    atomic.Int64
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
	for _, r := range rows {
		sink.SendRow(r, s.id)
	}
}

// Ingest batch-inserts rows into the logs table.
func (s *DuckDBShard) Ingest(rows []*pb.LogRow) error {
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
		if len(r.PayloadJson) > 0 {
			payloadStr = string(r.PayloadJson)
		}

		args = append(args,
			r.LineNum,
			r.Raw,
			r.Fields,
			time.Unix(0, r.TsUnixNs),
			r.Level,
			r.App,
			r.Instance,
			r.Customer,
			payloadStr,
		)
	}

	_, err := s.db.Exec(sb.String(), args...)
	if err == nil {
		s.rowCount.Add(int64(len(rows)))
	}
	return err
}

// RowCount returns the total number of rows ingested into this shard.
func (s *DuckDBShard) RowCount() int64 {
	return s.rowCount.Load()
}

// ExportToParquet exports the logs table to a Parquet file.
func (s *DuckDBShard) ExportToParquet(path string) error {
	_, err := s.db.Exec(fmt.Sprintf("COPY logs TO '%s' (FORMAT PARQUET)", path))
	return err
}

// Query executes a SQL query and returns the matching rows.
func (s *DuckDBShard) Query(query string) ([]*pb.LogRow, error) {
	sqlRows, err := s.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("query shard %s: %w", s.id, err)
	}
	defer sqlRows.Close()
	return scanRows(sqlRows)
}
