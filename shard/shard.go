package shard

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// RowSink receives result rows streamed from a shard query. ComputeJob satisfies this.
type RowSink interface {
	SendRow(row *pb.ResultRow, shardID string)
	SendSentinel(shardID string)
}

// Shard is the interface the coordinator uses to interact with a shard node.
// Shards are dumb storage — the coordinator manages time windows and routing.
type Shard interface {
	ID() string
	Query(sql string) ([]*pb.ResultRow, error)
	QueryToSink(sql string, sink RowSink)
	Ingest(rows []*pb.LogRow) error
	Close() error
}

// DuckDBShard is an in-process shard backed by a local DuckDB database.
type DuckDBShard struct {
	id       string
	db       *sql.DB
	rowCount atomic.Int64
}

const createTableSQL = `
CREATE TABLE IF NOT EXISTS logs (
    line_num       BIGINT,
    raw            TEXT,
    fields         TEXT[],
    ts             TIMESTAMP,
    level          TEXT,
    app            TEXT,
    instance       TEXT,
    customer       TEXT,
    payload        JSON,
    deployment     TEXT,
    env            TEXT,
    version        TEXT,
    region         TEXT,
    cluster        TEXT,
    k8s_namespace  TEXT,
    pod            TEXT,
    container      TEXT,
    node           TEXT
);
`

// migrationSteps converges existing shards' schemas to the current target.
// Each step is idempotent: rename guarded by column existence, ADD guarded
// by IF NOT EXISTS. Order matters — rename must run before any ADD that
// would conflict with the old name.
var migrationSteps = []string{
	// Rename host_group → deployment if the old column still exists.
	`ALTER TABLE logs RENAME COLUMN host_group TO deployment`,
	// Add the six new dimension columns. ADD COLUMN IF NOT EXISTS makes
	// these idempotent on shards that already carry them.
	`ALTER TABLE logs ADD COLUMN IF NOT EXISTS deployment TEXT`,
	`ALTER TABLE logs ADD COLUMN IF NOT EXISTS env TEXT`,
	`ALTER TABLE logs ADD COLUMN IF NOT EXISTS version TEXT`,
	`ALTER TABLE logs ADD COLUMN IF NOT EXISTS region TEXT`,
	`ALTER TABLE logs ADD COLUMN IF NOT EXISTS cluster TEXT`,
	`ALTER TABLE logs ADD COLUMN IF NOT EXISTS k8s_namespace TEXT`,
	`ALTER TABLE logs ADD COLUMN IF NOT EXISTS pod TEXT`,
	`ALTER TABLE logs ADD COLUMN IF NOT EXISTS container TEXT`,
	`ALTER TABLE logs ADD COLUMN IF NOT EXISTS node TEXT`,
}

// NewDuckDBShard opens or creates a DuckDB database at path and initializes the
// logs table. Use "" or ":memory:" for an in-memory database.
func NewDuckDBShard(id, path string) (*DuckDBShard, error) {
	db, err := sql.Open("duckdb", path)
	if err != nil {
		return nil, fmt.Errorf("open duckdb %q: %w", path, err)
	}
	if _, err := db.Exec(createTableSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("create table: %w", err)
	}
	// Run migrations. Errors are non-fatal — the rename fails on fresh
	// shards that never had host_group, the ADDs fail on fully-migrated
	// shards. Either way the schema converges to the CREATE TABLE shape.
	for _, step := range migrationSteps {
		_, _ = db.Exec(step)
	}
	return &DuckDBShard{id: id, db: db}, nil
}

func (s *DuckDBShard) ID() string { return s.id }

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
		log.Printf("shard %s: QueryToSink error: %v (sql=%s)", s.id, err, query)
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
	sb.WriteString("INSERT INTO logs (line_num, raw, fields, ts, level, app, instance, customer, payload, deployment, env, version, region, cluster, k8s_namespace, pod, container, node) VALUES ")

	args := make([]any, 0, len(rows)*18)
	for i, r := range rows {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString("(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")

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
			r.Deployment,
			r.Env,
			r.Version,
			r.Region,
			r.Cluster,
			r.K8SNamespace,
			r.Pod,
			r.Container,
			r.Node,
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

// ExportToParquet exports the logs table to a Parquet file (local path).
func (s *DuckDBShard) ExportToParquet(path string) error {
	_, err := s.db.Exec(fmt.Sprintf("COPY logs TO '%s' (FORMAT PARQUET)", path))
	return err
}

// ExportToS3Parquet exports the logs table to a Parquet file on S3.
// Configures httpfs on this DuckDB connection, then writes directly.
func (s *DuckDBShard) ExportToS3Parquet(s3Path string, cfg *S3Config) error {
	if err := cfg.ConfigureDuckDB(s.db); err != nil {
		return err
	}
	_, err := s.db.Exec(fmt.Sprintf("COPY logs TO '%s' (FORMAT PARQUET)", s3Path))
	return err
}

// Query executes a SQL query and returns the matching result rows.
// The SQL is expected to return (line_num, raw) — two columns.
func (s *DuckDBShard) Query(query string) ([]*pb.ResultRow, error) {
	return s.QueryContext(context.Background(), query)
}

// QueryContext is Query with a deadline. The DuckDB Go driver honors
// context cancellation at row boundaries, so a context timeout trims
// pathological long-running queries (the main motivation for the
// SHARD_QUERY_TIMEOUT_SECONDS runtime config).
func (s *DuckDBShard) QueryContext(ctx context.Context, query string) ([]*pb.ResultRow, error) {
	sqlRows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query shard %s: %w", s.id, err)
	}
	defer sqlRows.Close()
	return ScanResultRows(sqlRows)
}
