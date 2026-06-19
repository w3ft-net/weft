package shard

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	duckdb "github.com/duckdb/duckdb-go/v2"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
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
	qstats   queryStats
}

// queryStats accumulates per-query execution timings between telemetry samples;
// snapshot() reads and resets it each interval. The slow tail (MaxMs) is the
// signal that matters: when a shard's table outgrows memory_limit and queries
// start spilling to disk, it's the worst-case latency that jumps — exactly the
// quantity we want to correlate against the volume's fill level (see
// runShardStats in cmd/shard).
type queryStats struct {
	mu    sync.Mutex
	count int64
	errs  int64
	rows  int64
	sumMs float64
	maxMs float64
}

func (q *queryStats) record(d time.Duration, rows int, err error) {
	ms := float64(d.Microseconds()) / 1000
	q.mu.Lock()
	q.count++
	q.sumMs += ms
	if ms > q.maxMs {
		q.maxMs = ms
	}
	q.rows += int64(rows)
	if err != nil {
		q.errs++
	}
	q.mu.Unlock()
}

// QueryStatsSnapshot is one telemetry interval's query-latency aggregate.
type QueryStatsSnapshot struct {
	Count  int64   // queries executed in the interval
	Errors int64   // of which failed
	Rows   int64   // total rows returned
	MeanMs float64 // mean execution time (ms)
	MaxMs  float64 // slowest single query (ms) — the spill-sensitive tail
}

func (q *queryStats) snapshot() QueryStatsSnapshot {
	q.mu.Lock()
	defer q.mu.Unlock()
	s := QueryStatsSnapshot{Count: q.count, Errors: q.errs, Rows: q.rows, MaxMs: q.maxMs}
	if q.count > 0 {
		s.MeanMs = q.sumMs / float64(q.count)
	}
	q.count, q.errs, q.rows, q.sumMs, q.maxMs = 0, 0, 0, 0, 0
	return s
}

// QueryStatsSnapshot reads and resets the shard's query-latency accumulator.
// Called once per telemetry interval by the stats emitter.
func (s *DuckDBShard) QueryStatsSnapshot() QueryStatsSnapshot { return s.qstats.snapshot() }

const createTableSQL = `
CREATE TABLE IF NOT EXISTS logs (
    raw            TEXT,
    fields         TEXT[],
    ts             TIMESTAMP,
    level          TEXT,
    namespace      TEXT,
    service        TEXT,
    kind           TEXT,
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
	// The composite `app` column is now redundant with the dedicated
	// namespace/service/kind columns (which the planner queries and which
	// index cleanly). Drop it once the backfills above have salvaged any
	// pre-split rows. On fresh shards the backfills no-op (app never
	// existed; the UPDATE errors are ignored like the other steps) and
	// this DROP is a no-op too.
	`ALTER TABLE logs DROP COLUMN IF EXISTS app`,
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
	ds := &DuckDBShard{id: id, dbPath: path, db: db}
	// Seed rowCount from the on-disk table so a shard recovering off a
	// reattached volume reports its real size in heartbeats. Without this
	// seed, the in-memory counter starts at zero and only increments on
	// post-boot Ingest, so the coordinator believes the recovered shard is
	// empty.
	var existing int64
	if err := db.QueryRow("SELECT COUNT(*) FROM logs").Scan(&existing); err != nil {
		db.Close()
		return nil, fmt.Errorf("seed rowCount from logs: %w", err)
	}
	ds.rowCount.Store(existing)
	return ds, nil
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

// logColumns is the explicit column order the Appender binds to. Naming the
// columns (rather than relying on physical table order) keeps ingest correct on
// migrated shards, whose on-disk column order diverges from createTableSQL once
// ALTER TABLE ADD/DROP has run. The AppendRow argument order below must match
// this list exactly.
var logColumns = []string{
	"raw", "fields", "ts", "level", "namespace", "service", "kind",
	"instance", "customer", "payload", "deployment", "env", "version",
	"region", "cluster", "k8s_namespace", "pod", "container", "node",
}

// Ingest bulk-loads rows into the logs table via DuckDB's Appender — the
// driver's fastest write path: it streams values straight into columnar chunks,
// skipping the SQL parse/bind a multi-row INSERT pays per batch. On error it
// reports the failure to rec (if non-nil) and returns an error naming the shard
// and the resulting consecutive-failure count so callers need a single log line.
func (s *DuckDBShard) Ingest(rows []*pb.LogRow, rec IngestRecorder) error {
	if len(rows) == 0 {
		return nil
	}

	// The Appender works against a single driver connection, so borrow one from
	// the pool for the batch and hand it back on return.
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		return wrapIngestError(s.id, fmt.Errorf("acquire conn: %w", err), rec)
	}
	defer conn.Close()

	err = conn.Raw(func(dc any) error {
		appender, aerr := duckdb.NewAppenderWithColumns(dc.(driver.Conn), "", "", "logs", logColumns)
		if aerr != nil {
			return fmt.Errorf("new appender: %w", aerr)
		}
		for _, r := range rows {
			// payload is a JSON column; setJSON marshals whatever we pass, so a
			// json.RawMessage round-trips the bytes verbatim (a bare string would
			// be double-encoded into a quoted JSON string).
			payload := json.RawMessage("{}")
			if len(r.PayloadJson) > 0 {
				payload = json.RawMessage(r.PayloadJson)
			}
			kind := r.Kind
			if kind == "" {
				kind = "main"
			}
			if rerr := appender.AppendRow(
				r.Raw,
				r.Fields,
				time.Unix(0, r.TsUnixNs),
				r.Level,
				r.Namespace,
				r.Service,
				kind,
				r.Instance,
				r.Customer,
				payload,
				r.Deployment,
				r.Env,
				r.Version,
				r.Region,
				r.Cluster,
				r.K8SNamespace,
				r.Pod,
				r.Container,
				r.Node,
			); rerr != nil {
				appender.Close()
				return fmt.Errorf("append row: %w", rerr)
			}
		}
		// Close flushes the buffered chunks to the table and releases the
		// appender; its error is the batch's commit error.
		return appender.Close()
	})
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
		return fmt.Errorf("ingest failed on shard %s (%d/%d consecutive, shard will degrade out of write routing): %w",
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

// JoinApp rebuilds the composite path from its (namespace, service,
// kind) parts — the inverse of SplitApp. It populates the vestigial
// `app` column so anything still reading it (cold-shard test queries)
// keeps working; routing and the planner key on the dedicated
// namespace/service/kind columns. Empty segments are dropped (so a
// row with no namespace yet renders "<service>", not "/<service>"),
// and a "main" kind is elided, so JoinApp(SplitApp(x)) == x for the
// canonical shapes.
func JoinApp(namespace, service, kind string) string {
	parts := make([]string, 0, 3)
	if namespace != "" {
		parts = append(parts, namespace)
	}
	if service != "" {
		parts = append(parts, service)
	}
	if kind != "" && kind != "main" {
		parts = append(parts, kind)
	}
	return strings.Join(parts, "/")
}

// RowCount returns the total rows currently in the shard's logs table.
// Seeded from SELECT COUNT(*) at boot (so a shard recovering off a
// reattached volume reports its real size, not zero) and incremented on
// every successful Ingest.
func (s *DuckDBShard) RowCount() int64 {
	return s.rowCount.Load()
}

// DiskUsage reports the shard's on-disk footprint on its data volume: the
// .duckdb database file and its co-located spill directory (`<db>.tmp`), which
// DuckDB writes larger-than-memory sorts/aggregations/joins/DISTINCTs to. Both
// share the shard's single EBS volume, so together they are what consumes it —
// and on a small (1 GiB) instance spill, not the data file, is the binding
// disk constraint (see duck-db-tuning.md). Best-effort and never errors so a
// disk metric can't fail a heartbeat; returns zeroes for an in-memory shard.
func (s *DuckDBShard) DiskUsage() DiskUsage {
	var u DiskUsage
	if s.dbPath == "" || s.dbPath == ":memory:" {
		return u
	}
	if fi, err := os.Stat(s.dbPath); err == nil {
		u.DuckDBBytes = fi.Size()
	}
	// Writes land in the WAL first and only fold into the .duckdb file at
	// checkpoint, so between checkpoints the WAL holds real, un-reflected bytes
	// on the same volume — counting only the main file would underreport.
	if fi, err := os.Stat(s.dbPath + ".wal"); err == nil {
		u.WALBytes = fi.Size()
	}
	u.SpillTmpBytes = dirSize(s.dbPath + ".tmp")
	return u
}

// DiskUsage is a shard's data-volume footprint. TotalBytes() is what to compare
// against the volume size for the ≤50%-co-located-spill rule.
type DiskUsage struct {
	DuckDBBytes   int64 // size of the .duckdb file (checkpointed data)
	WALBytes      int64 // size of <db>.wal (writes not yet folded into the main file)
	SpillTmpBytes int64 // total bytes under <db>.tmp (DuckDB's spill directory)
}

// TotalBytes is the combined .duckdb + WAL + spill footprint on the data volume.
func (u DiskUsage) TotalBytes() int64 { return u.DuckDBBytes + u.WALBytes + u.SpillTmpBytes }

// dirSize sums the sizes of all regular files under root, returning 0 when root
// is absent — the common case, since DuckDB only materializes `<db>.tmp` once a
// query actually spills. Best-effort: transient walk errors are skipped rather
// than propagated, so a disk reading never blocks the caller.
func dirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
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
	granDay   partitionGranularity = "date_trunc('day', ts)"
	granHour  partitionGranularity = "date_trunc('hour', ts)"
	gran10Min partitionGranularity = "time_bucket(INTERVAL '10 minutes', ts)"
	gran1Min  partitionGranularity = "time_bucket(INTERVAL '1 minute', ts)"
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
	start := time.Now()
	sqlRows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		s.qstats.record(time.Since(start), 0, err)
		return nil, fmt.Errorf("query shard %s: %w", s.id, err)
	}
	defer sqlRows.Close()
	// Time through row materialization, not just statement prep — scanning a
	// large result set is itself disk/CPU work that grows as the shard does.
	rows, scanErr := ScanResultRows(sqlRows)
	s.qstats.record(time.Since(start), len(rows), scanErr)
	return rows, scanErr
}
