package shard

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
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
//
// args carries positional placeholder values for `?` markers in sql.
// Empty when the planner emitted no placeholders (legacy paths still
// using string-interpolated SQL); the DuckDB driver tolerates 0-arg
// calls on prepared statements with no placeholders.
type Shard interface {
	ID() string
	Query(sql string, args ...any) ([]*pb.ResultRow, error)
	QueryToSink(sql string, args []any, sink RowSink)
	Ingest(rows []*pb.LogRow, rec IngestRecorder) error
	Archive(ctx context.Context, s3Prefix string) (string, error)
	Close() error
}

// IngestRecorder is the surface a Shard.Ingest implementation uses
// to report its own outcome back to the failure-count / suspension
// tracker (the provisioner). The Shard formats the error string from
// (count, threshold) so the caller only needs one log.Printf to
// surface the full state transition.
//
// Nil-safe: callers that don't care about routing failover (tests,
// in-process server paths) pass nil and Ingest behaves like the old
// bare-error form.
type IngestRecorder interface {
	// RecordIngestFailure increments the consecutive-failure counter
	// for shardID and returns the new count plus the threshold at
	// which the recorder considers the shard suspended.
	RecordIngestFailure(shardID string) (count, threshold int)
	// ResetIngestFailures clears the counter on a successful ingest.
	ResetIngestFailures(shardID string)
}

// DuckDBShard is an in-process shard backed by a local DuckDB database.
type DuckDBShard struct {
	id       string
	dbPath   string
	db       *sql.DB
	rowCount atomic.Int64
}

const createTableSQL = `
CREATE TABLE IF NOT EXISTS logs (
    raw            TEXT,
    fields         TEXT[],
    ts             TIMESTAMP,
    level          TEXT,
    namespace      TEXT,
    service        TEXT,
    kind           TEXT,
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
	`ALTER TABLE logs DROP COLUMN IF EXISTS line_num`,
	`ALTER TABLE logs ADD COLUMN IF NOT EXISTS namespace TEXT`,
	`ALTER TABLE logs ADD COLUMN IF NOT EXISTS service TEXT`,
	// Backfill namespace/service from composite app column for existing rows.
	`UPDATE logs SET namespace = split_part(app, '/', 1), service = substr(app, position('/' in app) + 1) WHERE namespace IS NULL AND app IS NOT NULL AND position('/' in app) > 0`,
	// Add the kind column and backfill from the third path segment of app.
	// 2-seg apps (ns/svc) get kind='main'; 3+-seg apps (ns/svc/kind[/…])
	// keep everything past the second slash as kind.
	`ALTER TABLE logs ADD COLUMN IF NOT EXISTS kind TEXT`,
	`UPDATE logs SET kind = CASE
		WHEN app IS NULL THEN NULL
		WHEN length(app) - length(replace(app, '/', '')) < 2 THEN 'main'
		ELSE substr(app, position('/' in substr(app, position('/' in app) + 1)) + position('/' in app) + 1)
	END WHERE kind IS NULL AND app IS NOT NULL`,
	// Service should not carry the kind suffix anymore. Strip everything
	// from the second slash onward so existing service='compute/heartbeats'
	// rows become service='compute' with kind='heartbeats'.
	`UPDATE logs SET service = substr(service, 1, position('/' in service) - 1) WHERE service IS NOT NULL AND position('/' in service) > 0`,
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
	return &DuckDBShard{id: id, dbPath: path, db: db}, nil
}

func (s *DuckDBShard) ID() string { return s.id }

func (s *DuckDBShard) Close() error {
	return s.db.Close()
}

// QueryToSink executes SQL and streams matching rows to the sink, then
// sends a sentinel. If the query fails, the sentinel is still sent so the
// compute job doesn't hang.
func (s *DuckDBShard) QueryToSink(query string, args []any, sink RowSink) {
	defer sink.SendSentinel(s.id)

	rows, err := s.Query(query, args...)
	if err != nil {
		log.Printf("shard %s: QueryToSink error: %v (sql=%s args=%v)", s.id, err, query, args)
		return // sentinel still sent via defer
	}
	for _, r := range rows {
		sink.SendRow(r, s.id)
	}
}

// Ingest batch-inserts rows into the logs table. On error, reports
// the failure to rec (if non-nil) and returns an error message that
// names the shard and the resulting consecutive-failure count so
// callers only need a single log line.
func (s *DuckDBShard) Ingest(rows []*pb.LogRow, rec IngestRecorder) error {
	if len(rows) == 0 {
		return nil
	}

	var sb strings.Builder
	sb.WriteString("INSERT INTO logs (raw, fields, ts, level, namespace, service, kind, app, instance, customer, payload, deployment, env, version, region, cluster, k8s_namespace, pod, container, node) VALUES ")

	args := make([]any, 0, len(rows)*20)
	for i, r := range rows {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString("(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")

		payloadStr := "{}"
		if len(r.PayloadJson) > 0 {
			payloadStr = string(r.PayloadJson)
		}

		ns, svc, kind := SplitApp(r.App)
		args = append(args,
			r.Raw,
			r.Fields,
			time.Unix(0, r.TsUnixNs),
			r.Level,
			ns,
			svc,
			kind,
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
	if err != nil {
		return wrapIngestError(s.id, err, rec)
	}
	s.rowCount.Add(int64(len(rows)))
	if rec != nil {
		rec.ResetIngestFailures(s.id)
	}
	return nil
}

// wrapIngestError reports err to rec (if non-nil) and returns an
// enriched error that names shardID and the resulting consecutive-
// failure count. Centralised so DuckDBShard / GRPCShard / test stubs
// share one format — callers downstream get a single log line.
func wrapIngestError(shardID string, err error, rec IngestRecorder) error {
	if rec == nil {
		return err
	}
	count, threshold := rec.RecordIngestFailure(shardID)
	if count >= threshold {
		return fmt.Errorf("ingest failed on shard %s (%d/%d consecutive, shard suspended): %w",
			shardID, count, threshold, err)
	}
	return fmt.Errorf("ingest failed on shard %s (%d/%d consecutive): %w",
		shardID, count, threshold, err)
}

// SplitApp splits a composite app name into namespace, service, and kind.
// Every row carries a kind: the regular tailed log is "main", telemetry
// streams (heartbeats, metrics, runtime) use their own kind. Wire-format
// apps land in one of three shapes:
//
//	"<ns>"                  → ns only; svc and kind both default to ""/"main"
//	"<ns>/<svc>"            → ns + svc; kind defaults to "main"
//	"<ns>/<svc>/<kind>"     → ns + svc + kind (the canonical shape)
//
// 4+ segments are joined into kind ("<ns>/<svc>/<a>/<b>" → kind="a/b").
// Empty input returns three empty strings.
func SplitApp(app string) (namespace, service, kind string) {
	if app == "" {
		return "", "", ""
	}
	parts := strings.SplitN(app, "/", 3)
	switch len(parts) {
	case 1:
		return parts[0], "", "main"
	case 2:
		return parts[0], parts[1], "main"
	default:
		return parts[0], parts[1], parts[2]
	}
}

// RowCount returns the total number of rows ingested into this shard.
func (s *DuckDBShard) RowCount() int64 {
	return s.rowCount.Load()
}

func (s *DuckDBShard) Archive(_ context.Context, s3Prefix string) (string, error) {
	gran, err := s.pickPartitionGranularity()
	if err != nil {
		return "", fmt.Errorf("partition sizing: %w", err)
	}

	if s3cfg := S3ConfigFromEnv(); s3cfg != nil {
		if err := s3cfg.ConfigureDuckDB(s.db); err != nil {
			return "", err
		}
		return s.exportParquet(fmt.Sprintf("%s/%s", s3Prefix, s.id), gran)
	}

	dir := fmt.Sprintf("%s/%s", s3Prefix, s.id)
	if gran == "" {
		// Small shard — single file, no partition.
		path := dir + ".parquet"
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return "", fmt.Errorf("mkdir for %s: %w", path, err)
		}
		_, err := s.db.Exec(fmt.Sprintf("COPY logs TO '%s' (FORMAT PARQUET)", path))
		return path, err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("mkdir for %s: %w", dir, err)
	}
	return s.exportParquet(dir, gran)
}

const targetFileBytes = 150 * 1024 * 1024 // 150 MB

type partitionGranularity string

const (
	granDay    partitionGranularity = "date_trunc('day', ts)"
	granHour   partitionGranularity = "date_trunc('hour', ts)"
	gran10Min  partitionGranularity = "time_bucket(INTERVAL '10 minutes', ts)"
	gran1Min   partitionGranularity = "time_bucket(INTERVAL '1 minute', ts)"
)

// pickPartitionGranularity determines how to split the archive export.
// Returns "" if the data is small enough for a single file.
func (s *DuckDBShard) pickPartitionGranularity() (partitionGranularity, error) {
	var dbBytes int64
	if s.dbPath != "" && s.dbPath != ":memory:" {
		fi, err := os.Stat(s.dbPath)
		if err == nil {
			dbBytes = fi.Size()
		}
	}
	if dbBytes == 0 {
		// Fallback: query row count and estimate ~200 bytes/row in Parquet.
		var count int64
		if err := s.db.QueryRow("SELECT COUNT(*) FROM logs").Scan(&count); err != nil {
			return "", err
		}
		dbBytes = count * 500 // rough DuckDB row size to approximate
	}

	estimatedParquet := int64(float64(dbBytes) * 0.4)
	if estimatedParquet < 100*1024*1024 {
		return "", nil
	}

	targetFiles := estimatedParquet / targetFileBytes
	if targetFiles < 2 {
		return "", nil
	}

	var tsMin, tsMax time.Time
	err := s.db.QueryRow("SELECT MIN(ts), MAX(ts) FROM logs").Scan(&tsMin, &tsMax)
	if err != nil {
		return "", err
	}
	span := tsMax.Sub(tsMin)
	if span <= 0 {
		return "", nil
	}

	interval := span / time.Duration(targetFiles)
	switch {
	case interval >= 24*time.Hour:
		return granDay, nil
	case interval >= time.Hour:
		return granHour, nil
	case interval >= 10*time.Minute:
		return gran10Min, nil
	default:
		return gran1Min, nil
	}
}

// exportParquet writes partitioned (or single-file) Parquet to the given path.
// If gran is empty, writes a single file at path+".parquet".
// If gran is set, writes hive-partitioned files under path/.
func (s *DuckDBShard) exportParquet(path string, gran partitionGranularity) (string, error) {
	if gran == "" {
		outPath := path + ".parquet"
		_, err := s.db.Exec(fmt.Sprintf("COPY logs TO '%s' (FORMAT PARQUET)", outPath))
		return outPath, err
	}
	// Partitioned export: DuckDB writes hive-style directories.
	sql := fmt.Sprintf(
		"COPY (SELECT *, %s AS _part FROM logs) TO '%s' (FORMAT PARQUET, PARTITION_BY (_part), OVERWRITE_OR_IGNORE)",
		string(gran), path,
	)
	_, err := s.db.Exec(sql)
	if err != nil {
		return "", err
	}
	return path + "/**/*.parquet", nil
}

// ExportToParquet exports the logs table to a single Parquet file (local path).
func (s *DuckDBShard) ExportToParquet(path string) error {
	_, err := s.db.Exec(fmt.Sprintf("COPY logs TO '%s' (FORMAT PARQUET)", path))
	return err
}

// ExportToS3Parquet exports the logs table to a single Parquet file on S3.
func (s *DuckDBShard) ExportToS3Parquet(s3Path string, cfg *S3Config) error {
	if err := cfg.ConfigureDuckDB(s.db); err != nil {
		return err
	}
	_, err := s.db.Exec(fmt.Sprintf("COPY logs TO '%s' (FORMAT PARQUET)", s3Path))
	return err
}

// Query executes a SQL query and returns the matching result rows.
// The SQL is expected to return (raw, ts). args binds positional
// `?` placeholders (planner-supplied customer name, timestamps,
// namespace/env/service names); pass none for legacy fully-baked SQL.
func (s *DuckDBShard) Query(query string, args ...any) ([]*pb.ResultRow, error) {
	return s.QueryContext(context.Background(), query, args...)
}

// QueryContext is Query with a deadline. The DuckDB Go driver honors
// context cancellation at row boundaries, so a context timeout trims
// pathological long-running queries (the main motivation for the
// SHARD_QUERY_TIMEOUT_SECONDS runtime config).
func (s *DuckDBShard) QueryContext(ctx context.Context, query string, args ...any) ([]*pb.ResultRow, error) {
	sqlRows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query shard %s: %w", s.id, err)
	}
	defer sqlRows.Close()
	return ScanResultRows(sqlRows)
}
