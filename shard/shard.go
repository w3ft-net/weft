package shard

import (
	"cmp"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	weftsdk "github.com/w3ft-net/weft-go"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	duckdb "github.com/duckdb/duckdb-go/v2"
	"github.com/google/uuid"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// RowSink receives result rows streamed from a shard query. compute.Job satisfies this.
type RowSink interface {
	SendRow(row *pb.ResultRow, shardID string)
	// Fail reports that the shard's rows for this query are missing or
	// incomplete; the query then has no answer. Called before the shard's
	// SendSentinel.
	Fail(shardID string, err error)
	SendSentinel(shardID string)
}

// FieldQuerier is a shard that understands a query's field form: fieldSQL
// returns just some of each row's fields as field columns (see
// ScanResultRows), and runs in place of sql. Compute asks a shard that has it
// and plain-queries one that doesn't (a cold shard, a shard from before it),
// so sql must be correct on its own.
type FieldQuerier interface {
	QueryFieldsToSink(ctx context.Context, sql, fieldSQL string, args []any, sink RowSink)
}

// TableQuerier is a shard that can be told which table holds every row a query
// reads (planner.QueryPlan.Table). A shard with a table per log source reads
// just that table; one without ignores the name and runs the SQL as written.
// fieldSQL, when set, runs in place of sql as in FieldQuerier. Compute asks a
// shard that has it when the plan names a table and otherwise queries as
// before, so sql must be correct on its own.
type TableQuerier interface {
	QueryTableToSink(ctx context.Context, table, sql, fieldSQL string, args []any, sink RowSink)
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
	Query(ctx context.Context, sql string, args ...any) ([]*pb.ResultRow, error)
	// QueryToSink streams matching rows to sink. ctx carries the
	// ambient trace ID (if any) — see weftsdk.ContextWithTraceID —
	// down to whichever wire boundary this implementation crosses
	// (gRPC metadata for a live shard, a JSON body field for a
	// cold-shard Lambda invocation).
	QueryToSink(ctx context.Context, sql string, args []any, sink RowSink)
	Ingest(ctx context.Context, rows []*pb.LogRow, rec IngestRecorder) error
	// Archive exports the shard's data to Parquet and returns the path
	// written plus per-customer stats (row counts and ts spans) captured
	// during export. The coordinator persists the stats to
	// shard_archive_stats for the retention janitor. See
	// docs/plans/retention-janitor.md.
	Archive(ctx context.Context, s3Prefix string) (string, []CustomerStat, error)
	Close() error
}

// CustomerStat summarizes one customer's rows within an archived shard:
// the timestamp span and row count. Captured during Archive (while the
// DuckDB handle is local on the shard node) and persisted to
// shard_archive_stats so the retention janitor can classify archived
// parquet (delete / keep / rewrite) without re-scanning S3.
type CustomerStat struct {
	CustomerID int64     `json:"customer_id"`
	MinTs      time.Time `json:"min_ts"`
	MaxTs      time.Time `json:"max_ts"`
	RowCount   int64     `json:"row_count"`
}

// CustomerStatsToProto / CustomerStatsFromProto bridge the Go struct and
// the ArchiveResponse wire form. Both return nil for an empty slice so
// the proto field stays absent rather than an empty list.
func CustomerStatsToProto(cs []CustomerStat) []*pb.CustomerArchiveStat {
	if len(cs) == 0 {
		return nil
	}
	out := make([]*pb.CustomerArchiveStat, len(cs))
	for i, c := range cs {
		out[i] = &pb.CustomerArchiveStat{
			CustomerId:  c.CustomerID,
			MinTsUnixNs: c.MinTs.UnixNano(),
			MaxTsUnixNs: c.MaxTs.UnixNano(),
			RowCount:    c.RowCount,
		}
	}
	return out
}

func CustomerStatsFromProto(ps []*pb.CustomerArchiveStat) []CustomerStat {
	if len(ps) == 0 {
		return nil
	}
	out := make([]CustomerStat, len(ps))
	for i, p := range ps {
		out[i] = CustomerStat{
			CustomerID: p.CustomerId,
			MinTs:      time.Unix(0, p.MinTsUnixNs).UTC(),
			MaxTs:      time.Unix(0, p.MaxTsUnixNs).UTC(),
			RowCount:   p.RowCount,
		}
	}
	return out
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

	// facts learns which awk fields hold only numbers; see fieldfacts.go.
	facts *FieldClassifier
	// factsMu orders saving the facts against dropping them; factsForgotten
	// is set once the classifier has stopped and nothing more may be saved.
	factsMu        sync.Mutex
	factsForgotten bool

	// archiveMu guards the single in-flight archive attempt (see beginArchive).
	archiveMu      sync.Mutex
	archiveRunning bool
	archiveAttempt int64
	archiveStarted time.Time

	// openTiming is where NewDuckDBShard spent its time, for OpenTiming.
	openTiming string

	// perKey is set when the database keeps a table per key (see layout.go);
	// tables are the keyed tables that exist, guarded by tablesMu.
	perKey   bool
	tablesMu sync.RWMutex
	tables   map[string]struct{}
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
    customer_id    BIGINT,
    payload        JSON,
    deployment     TEXT,
    env            TEXT,
    version        TEXT,
    region         TEXT,
    cluster        TEXT,
    k8s_namespace  TEXT,
    pod            TEXT,
    container      TEXT,
    node           TEXT,
    trace_id       TEXT,
    span_name      TEXT,
    span_id        TEXT,
    parent_span_id TEXT,
    duration_ns    BIGINT,
    event_id       UUID,
    line_no        INTEGER
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
	// No backfill: unlike namespace/service/kind, there's no prior
	// composite encoding to recover a trace_id from. Existing rows get NULL.
	`ALTER TABLE logs ADD COLUMN IF NOT EXISTS trace_id TEXT`,
	// Same reasoning as trace_id: no prior composite to backfill from —
	// weft-go's span.go is the only source, and it's new. See
	// docs/plans/span-display.md.
	`ALTER TABLE logs ADD COLUMN IF NOT EXISTS span_name TEXT`,
	`ALTER TABLE logs ADD COLUMN IF NOT EXISTS span_id TEXT`,
	`ALTER TABLE logs ADD COLUMN IF NOT EXISTS parent_span_id TEXT`,
	`ALTER TABLE logs ADD COLUMN IF NOT EXISTS duration_ns BIGINT`,
	// Multi-line text events are stored one row per line; event_id/line_no
	// group them. NULL on one-line events and on rows ingested before this
	// existed. See docs/plans/line-storage.md.
	`ALTER TABLE logs ADD COLUMN IF NOT EXISTS event_id UUID`,
	`ALTER TABLE logs ADD COLUMN IF NOT EXISTS line_no INTEGER`,
}

// legacyColumns are columns an older build's schema carried that the
// migrations remove or rename; any of them means the shard needs migrating.
var legacyColumns = []string{"host_group", "app", "line_num"}

// schemaIsCurrent reports whether the logs table already has every column the
// code writes and none of the legacy ones, so the migrations would change
// nothing. It only reads the catalog, where running the migrations takes a
// write for every statement even when each is a no-op. Any doubt (a failed
// read) says it is not current, and the migrations run.
func schemaIsCurrent(db *sql.DB) bool {
	rows, err := db.Query(`SELECT name FROM pragma_table_info('logs')`)
	if err != nil {
		return false
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var name string
		if rows.Scan(&name) != nil {
			return false
		}
		have[name] = true
	}
	if rows.Err() != nil {
		return false
	}
	for _, c := range logColumns {
		if !have[c] {
			return false
		}
	}
	for _, c := range legacyColumns {
		if have[c] {
			return false
		}
	}
	return true
}

// NewDuckDBShard opens or creates a DuckDB database at path and initializes the
// logs table. Use "" or ":memory:" for an in-memory database. The database
// keeps the layout it already has; see Option for how a new one is laid out.
func NewDuckDBShard(id, path string, opts ...Option) (*DuckDBShard, error) {
	var cfg openOptions
	for _, o := range opts {
		o(&cfg)
	}
	t0 := time.Now()
	db, err := sql.Open("duckdb", path)
	if err != nil {
		return nil, fmt.Errorf("open duckdb %q: %w", path, err)
	}
	// The duckdb driver opens the database file inside sql.Open, so t0 is taken
	// before it; the Ping confirms the connection.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open duckdb %q: %w", path, err)
	}
	connected := time.Now()
	if _, err := db.Exec(createTableSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("create table: %w", err)
	}
	created := time.Now()
	// Run migrations. Errors are non-fatal — the rename fails on fresh
	// shards that never had host_group, the ADDs fail on fully-migrated
	// shards. Either way the schema converges to the CREATE TABLE shape.
	migrate := "skipped"
	if !schemaIsCurrent(db) {
		for _, step := range migrationSteps {
			_, _ = db.Exec(step)
		}
		migrate = ""
	}
	migrated := time.Now()
	ds := &DuckDBShard{id: id, dbPath: path, db: db}
	if err := ds.initLayout(cfg.perKey); err != nil {
		db.Close()
		return nil, fmt.Errorf("init layout: %w", err)
	}
	if ds.perKey {
		if err := ds.syncTableColumns(); err != nil {
			db.Close()
			return nil, fmt.Errorf("sync table columns: %w", err)
		}
	}
	// Seed rowCount from the on-disk table so a shard recovering off a
	// reattached volume reports its real size in heartbeats. Without this
	// seed, the in-memory counter starts at zero and only increments on
	// post-boot Ingest, so the coordinator believes the recovered shard is
	// empty.
	var existing int64
	if err := db.QueryRow("SELECT COUNT(*) FROM " + ds.source()).Scan(&existing); err != nil {
		db.Close()
		return nil, fmt.Errorf("seed rowCount from logs: %w", err)
	}
	ds.rowCount.Store(existing)
	ds.facts = NewFieldClassifier()
	if _, err := db.Exec(factsStateSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("create field_facts_state: %w", err)
	}
	ds.restoreFieldFacts(existing)
	ds.facts.SetPersist(ds.persistFieldFacts, ds.forgetFieldFacts)
	ds.openTiming = fmt.Sprintf("open=%s create=%s migrate=%s count=%s",
		connected.Sub(t0).Round(time.Microsecond), created.Sub(connected).Round(time.Microsecond),
		cmp.Or(migrate, migrated.Sub(created).Round(time.Microsecond).String()), time.Since(migrated).Round(time.Microsecond))
	return ds, nil
}

// OpenTiming reports how long each step of NewDuckDBShard took. A spare opens
// its database inside an upgrade's hold window, so this says what a slow open
// spent it on.
func (s *DuckDBShard) OpenTiming() string { return s.openTiming }

func (s *DuckDBShard) ID() string { return s.id }

func (s *DuckDBShard) Close() error {
	s.facts.Stop()
	return s.db.Close()
}

// Checkpoint folds the WAL into the database file, so a volume unmounted
// after Close carries a database the next instance opens without replay.
func (s *DuckDBShard) Checkpoint(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "CHECKPOINT")
	return err
}

// QueryToSink executes SQL and streams matching rows to the sink, then
// sends a sentinel. If the query fails, the sentinel is still sent so the
// compute job doesn't hang.
func (s *DuckDBShard) QueryToSink(ctx context.Context, query string, args []any, sink RowSink) {
	s.QueryTableToSink(ctx, "", query, "", args, sink)
}

// QueryTableToSink is QueryToSink for a query whose table the planner named;
// see QueryContextTable. fieldSQL, when set, runs in place of query.
func (s *DuckDBShard) QueryTableToSink(ctx context.Context, table, query, fieldSQL string, args []any, sink RowSink) {
	defer sink.SendSentinel(s.id)

	if fieldSQL != "" {
		query = fieldSQL
	}
	rows, err := s.QueryContextTable(ctx, table, query, args...)
	if err != nil {
		weftsdk.Errorf(ctx, "shard %s: QueryToSink error: %v (sql=%s args=%v)", s.id, err, query, args)
		sink.Fail(s.id, err)
		return // sentinel still sent via defer
	}
	for _, r := range rows {
		sink.SendRow(r, s.id)
	}
}

// QueryFieldsToSink is QueryToSink for a query's field form: fieldSQL runs in
// place of sql, which only shards without this method need.
func (s *DuckDBShard) QueryFieldsToSink(ctx context.Context, _, fieldSQL string, args []any, sink RowSink) {
	s.QueryToSink(ctx, fieldSQL, args, sink)
}

// logColumns is the explicit column order the Appender binds to. Naming the
// columns (rather than relying on physical table order) keeps ingest correct on
// migrated shards, whose on-disk column order diverges from createTableSQL once
// ALTER TABLE ADD/DROP has run. The AppendRow argument order below must match
// this list exactly.
var logColumns = []string{
	"raw", "fields", "ts", "level", "namespace", "service", "kind",
	"instance", "customer_id", "payload", "deployment", "env", "version",
	"region", "cluster", "k8s_namespace", "pod", "container", "node",
	"trace_id", "span_name", "span_id", "parent_span_id", "duration_ns",
	"event_id", "line_no",
}

// awkFields splits a line into the fields column the way awk's default
// field separator splits $0, so the planner can lower awk's $N to
// fields[N]: on runs of spaces, tabs, newlines and carriage returns, as
// the awk compute runs (warp's) does. Rows ingested before this held a
// schema-dependent split; queries over them may disagree with awk.
func awkFields(raw string) []string {
	return strings.FieldsFunc(raw, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
}

// Ingest bulk-loads rows into the logs table via DuckDB's Appender — the
// driver's fastest write path: it streams values straight into columnar chunks,
// skipping the SQL parse/bind a multi-row INSERT pays per batch. On error it
// reports the failure to rec (if non-nil) and returns an error naming the shard
// and the resulting consecutive-failure count so callers need a single log line.
func (s *DuckDBShard) Ingest(ctx context.Context, rows []*pb.LogRow, rec IngestRecorder) error {
	if len(rows) == 0 {
		return nil
	}

	var err error
	if s.perKey {
		err = s.ingestPerKey(ctx, rows)
	} else {
		err = s.ingestTable(ctx, "logs", rows)
	}
	if err != nil {
		return wrapIngestError(s.id, err, rec)
	}
	s.rowCount.Add(int64(len(rows)))
	s.facts.Observe(rows)
	if rec != nil {
		rec.ResetIngestFailures(s.id)
	}
	return nil
}

// ingestTable appends rows to one table on a connection of its own.
func (s *DuckDBShard) ingestTable(ctx context.Context, table string, rows []*pb.LogRow) error {
	// The Appender works against a single driver connection, so borrow one from
	// the pool for the batch and hand it back on return.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	return conn.Raw(func(dc any) error {
		return appendRows(dc.(driver.Conn), table, rows)
	})
}

// ingestPerKey appends each row to the table of its key. The whole batch is
// one transaction: a caller that retries a failed batch must not find its good
// rows already stored.
func (s *DuckDBShard) ingestPerKey(ctx context.Context, rows []*pb.LogRow) error {
	var order []tableKey
	groups := map[tableKey][]*pb.LogRow{}
	for _, r := range rows {
		k := rowKey(r)
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], r)
	}
	if err := s.ensureTables(ctx, order); err != nil {
		return err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN TRANSACTION"); err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	err = conn.Raw(func(dc any) error {
		for _, k := range order {
			if err := appendRows(dc.(driver.Conn), k.table(), groups[k]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// appendRows streams rows into table through DuckDB's Appender on dc.
func appendRows(dc driver.Conn, table string, rows []*pb.LogRow) error {
	appender, aerr := duckdb.NewAppenderWithColumns(dc, "", "", table, logColumns)
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
		// A one-line event or JSON row carries no event_id: both
		// columns are NULL, not 0.
		var eventID, lineNo any
		if r.EventId != "" {
			id, perr := uuid.Parse(r.EventId)
			if perr != nil {
				appender.Close()
				return fmt.Errorf("event_id %q: %w", r.EventId, perr)
			}
			eventID, lineNo = duckdb.UUID(id), r.LineNo
		}
		if rerr := appender.AppendRow(
			r.Raw,
			awkFields(r.Raw),
			time.Unix(0, r.TsUnixNs),
			r.Level,
			r.Namespace,
			r.Service,
			kind,
			r.Instance,
			r.CustomerId,
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
			r.TraceId,
			r.SpanName,
			r.SpanId,
			r.ParentSpanId,
			r.DurationNs,
			eventID,
			lineNo,
		); rerr != nil {
			appender.Close()
			return fmt.Errorf("append row: %w", rerr)
		}
	}
	// Close flushes the buffered chunks to the table and releases the
	// appender; its error is the batch's commit error.
	return appender.Close()
}

// factsStateSQL keeps the field classifier's results in the shard's own
// database, so they travel with the volume through a roll.
const factsStateSQL = `CREATE TABLE IF NOT EXISTS field_facts_state (
	id INTEGER PRIMARY KEY, state VARCHAR NOT NULL)`

// persistFieldFacts keeps the classifier's results in the database; the
// classifier calls it only when a result changes.
func (s *DuckDBShard) persistFieldFacts(b []byte) error {
	s.factsMu.Lock()
	defer s.factsMu.Unlock()
	if s.factsForgotten {
		return nil // the classifier stopped; what it would save is incomplete
	}
	_, err := s.db.Exec(`INSERT OR REPLACE INTO field_facts_state (id, state) VALUES (1, ?)`, string(b))
	return err
}

// forgetFieldFacts drops the saved results once the classifier has stopped
// (it missed rows), and keeps a late save from bringing them back. The next
// open finds rows and nothing saved, and leaves the facts off.
func (s *DuckDBShard) forgetFieldFacts() {
	s.factsMu.Lock()
	defer s.factsMu.Unlock()
	s.factsForgotten = true
	if _, err := s.db.Exec(`DELETE FROM field_facts_state`); err != nil {
		weftsdk.Warnf(context.Background(), "shard %s: drop saved field facts: %v", s.id, err)
	}
}

// FlushFieldFacts waits until every row ingested so far is classified and
// any resulting change is saved. A roll calls it once ingest has stopped.
func (s *DuckDBShard) FlushFieldFacts() { s.facts.Drain() }

// restoreFieldFacts brings back the results persistFieldFacts kept. A shard
// with rows but nothing kept (one that predates this) gets no facts: its rows
// are never rescanned.
func (s *DuckDBShard) restoreFieldFacts(rows int64) {
	if rows == 0 {
		return
	}
	var state string
	if err := s.db.QueryRow(`SELECT state FROM field_facts_state WHERE id = 1`).Scan(&state); err != nil {
		s.facts.Disable(fmt.Sprintf("no saved facts for the %d rows already here", rows))
		return
	}
	if err := s.facts.Restore([]byte(state)); err != nil {
		s.facts.Disable(err.Error())
	}
}

// FieldFacts is what the shard's classifier has learned that the coordinator
// has not yet acknowledged.
func (s *DuckDBShard) FieldFacts() []FieldFact { return s.facts.Pending() }

// AckFieldFacts records that the coordinator holds every fact up to epoch.
func (s *DuckDBShard) AckFieldFacts(epoch uint64) { s.facts.Ack(epoch) }

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

// Archive exports the shard to Parquet and returns the path and per-customer
// stats. It honors ctx (a cancelled attempt stops the DuckDB statement), runs
// at most one attempt at a time (ErrArchiveInFlight otherwise), and logs each
// phase with the shard's native memory state so a stuck export is diagnosable
// from the shard log alone.
func (s *DuckDBShard) Archive(ctx context.Context, s3Prefix string) (string, []CustomerStat, error) {
	return s.archive(ctx, s3Prefix, "", false)
}

// archive is Archive with a suffix on the output name: "" writes
// <prefix>/<id>[.parquet], the in-process layout; the ephemeral export passes
// ExportOutputSuffix(attempt) so each attempt gets a location of its own. keyed
// splits a partitioned archive by log key too (see exportParquet).
func (s *DuckDBShard) archive(ctx context.Context, s3Prefix, outputSuffix string, keyed bool) (string, []CustomerStat, error) {
	attempt, end, err := s.beginArchive()
	if err != nil {
		return "", nil, err
	}
	defer end()

	start := time.Now()
	fail := func(phase string, err error) (string, []CustomerStat, error) {
		weftsdk.Errorf(ctx, "archive attempt=%d failed phase=%s after=%s err=%v %s",
			attempt, phase, time.Since(start).Round(time.Millisecond), err, s.nativeMemoryDiag(context.WithoutCancel(ctx)))
		return "", nil, err
	}
	weftsdk.Infof(ctx, "archive attempt=%d start shard=%s prefix=%s %s", attempt, s.id, s3Prefix, s.nativeMemoryDiag(ctx))

	// Capture per-customer stats first, off the same local handle that's
	// about to be exported. Cheaper here (one GROUP BY over local DuckDB)
	// than a re-scan of the written parquet over S3/Lambda later.
	statsStart := time.Now()
	stopSampling := s.sampleArchiveMemory(attempt, "stats", archiveSampleInterval)
	stats, err := s.customerStats(ctx)
	stopSampling()
	if err != nil {
		return fail("stats", fmt.Errorf("customer stats: %w", err))
	}
	weftsdk.Infof(ctx, "archive attempt=%d stats customers=%d took=%s %s",
		attempt, len(stats), time.Since(statsStart).Round(time.Millisecond), s.nativeMemoryDiag(ctx))

	stopSampling = s.sampleArchiveMemory(attempt, "copy", archiveSampleInterval)
	path, rows, err := s.exportArchive(ctx, attempt, s3Prefix, outputSuffix, keyed)
	stopSampling()
	if err != nil {
		return fail("export", err)
	}
	weftsdk.Infof(ctx, "archive attempt=%d done path=%s rows=%d total=%s %s",
		attempt, path, rows, time.Since(start).Round(time.Millisecond), s.nativeMemoryDiag(ctx))
	return path, stats, nil
}

// customerStats summarizes each customer's row count and ts span in the
// logs table. Drives the retention janitor's classify-without-scanning
// pass (see docs/plans/retention-janitor.md).
func (s *DuckDBShard) customerStats(ctx context.Context) ([]CustomerStat, error) {
	return scanCustomerStats(ctx, s.db, `
		SELECT COALESCE(customer_id, 0), MIN(ts), MAX(ts), COUNT(*)
		FROM `+s.source()+` GROUP BY 1`)
}

// scanCustomerStats runs a (customer, min_ts, max_ts, count) aggregation
// and scans it into CustomerStat rows. The query must project exactly
// those four columns in that order; COALESCE the customer so the scan
// target is always a plain string (the column is nullable).
func scanCustomerStats(ctx context.Context, db *sql.DB, query string) ([]CustomerStat, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CustomerStat
	for rows.Next() {
		var c CustomerStat
		if err := rows.Scan(&c.CustomerID, &c.MinTs, &c.MaxTs, &c.RowCount); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// exportArchive writes the logs table to Parquet (single file or
// time-partitioned, sized by pickPartitionGranularity) and returns the
// path. S3 when S3ConfigFromEnv is set, otherwise a local directory.
// It also returns the number of rows the COPY wrote. attempt tags the phase
// log lines (see Archive).
func (s *DuckDBShard) exportArchive(ctx context.Context, attempt int64, s3Prefix, outputSuffix string, keyed bool) (path string, rows int64, err error) {
	plan, err := s.planPartitions(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("partition sizing: %w", err)
	}
	weftsdk.Infof(ctx, "archive attempt=%d sizing db_bytes=%d est_parquet_bytes=%d target_files=%d bucket_minutes=%d",
		attempt, plan.dbBytes, plan.estParquetBytes, plan.targetFiles, plan.minutes)

	copyStart := time.Now()
	weftsdk.Infof(ctx, "archive attempt=%d copy start %s", attempt, s.nativeMemoryDiag(ctx))
	defer func() {
		if err == nil {
			weftsdk.Infof(ctx, "archive attempt=%d copy done rows=%d took=%s", attempt, rows, time.Since(copyStart).Round(time.Millisecond))
		}
	}()

	if s3cfg := S3ConfigFromEnv(); s3cfg != nil {
		if err := s3cfg.ConfigureDuckDB(s.db); err != nil {
			return "", 0, err
		}
		return s.exportParquet(ctx, fmt.Sprintf("%s/%s%s", s3Prefix, s.id, outputSuffix), plan.minutes, keyed)
	}

	dir := fmt.Sprintf("%s/%s%s", s3Prefix, s.id, outputSuffix)
	if plan.minutes == 0 {
		// Small shard — single file, no partition.
		path := dir + ".parquet"
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return "", 0, fmt.Errorf("mkdir for %s: %w", path, err)
		}
		res, err := s.db.ExecContext(ctx, fmt.Sprintf("COPY logs TO '%s' (FORMAT PARQUET)", path))
		if err != nil {
			return "", 0, err
		}
		n, _ := res.RowsAffected()
		return path, n, nil
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", 0, fmt.Errorf("mkdir for %s: %w", dir, err)
	}
	return s.exportParquet(ctx, dir, plan.minutes, keyed)
}

// partitionPlan is the archive export's sizing decision: the inputs
// (dbBytes, the estimated Parquet size and target file count derived from it)
// and the resulting bucket width. minutes is 0 when the data fits a single
// file.
type partitionPlan struct {
	minutes         int
	dbBytes         int64
	estParquetBytes int64
	targetFiles     int64
}

// planPartitions determines how to split the archive export.
func (s *DuckDBShard) planPartitions(ctx context.Context) (partitionPlan, error) {
	var p partitionPlan
	if s.dbPath != "" && s.dbPath != ":memory:" {
		fi, err := os.Stat(s.dbPath)
		if err == nil {
			p.dbBytes = fi.Size()
		}
	}
	if p.dbBytes == 0 {
		// Fallback: query row count and estimate ~200 bytes/row in Parquet.
		var count int64
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+s.source()).Scan(&count); err != nil {
			return p, err
		}
		p.dbBytes = count * 500 // rough DuckDB row size to approximate
	}

	p.estParquetBytes = int64(float64(p.dbBytes) * parquetToDBRatio)
	p.targetFiles = p.estParquetBytes / targetFileBytes
	if p.estParquetBytes < 100*1024*1024 || p.targetFiles < 2 {
		return p, nil // fits a single file
	}

	var tsMin, tsMax time.Time
	if err := s.db.QueryRowContext(ctx, "SELECT MIN(ts), MAX(ts) FROM "+s.source()).Scan(&tsMin, &tsMax); err != nil {
		return p, err
	}
	p.minutes = choosePartitionMinutes(p.estParquetBytes, tsMax.Sub(tsMin))
	return p, nil
}

// exportParquet writes partitioned (or single-file) Parquet to the given path
// and returns it with the number of rows written.
// If minutes is 0, writes a single file at path+".parquet".
// Otherwise writes hive-partitioned files, one directory per minutes-wide time
// bucket, under path/m<minutes>/ (see PartitionMinutes). With keyed, each
// bucket is also split by (customer, namespace, service, kind) and the files
// go under path/k<minutes>/ (see IsKeyed). A single file is not split.
func (s *DuckDBShard) exportParquet(ctx context.Context, path string, minutes int, keyed bool) (string, int64, error) {
	if minutes == 0 {
		outPath := path + ".parquet"
		res, err := s.db.ExecContext(ctx, fmt.Sprintf("COPY %s TO '%s' (FORMAT PARQUET)", s.source(), outPath))
		if err != nil {
			return outPath, 0, err
		}
		n, _ := res.RowsAffected()
		return outPath, n, nil
	}
	layout, partitionBy, order := "m", "_part", ""
	if keyed {
		layout, partitionBy = "k", keyedPartitionBy
		// Sorted, so each partition arrives whole. DuckDB keeps at most 100
		// partition files open and starts a new file each time it reopens one,
		// so rows left in stored order (every batch touches every partition)
		// become many small files per partition: 1,905 files for 308
		// partitions in a 6M-row trial, 326 sorted. A cold read pays for each
		// file. The files are not each one time-ordered run (every writer
		// thread flushes its own), only each run is.
		order = " ORDER BY " + keyedPartitionBy + ", ts"
	}
	dir := fmt.Sprintf("%s/%s%d", path, layout, minutes)
	sql := fmt.Sprintf(
		"COPY (SELECT *, %s AS _part FROM %s%s) TO '%s' (FORMAT PARQUET, PARTITION_BY (%s), OVERWRITE_OR_IGNORE)",
		partitionExpr(minutes, "ts"), s.source(), order, dir, partitionBy,
	)
	res, err := s.db.ExecContext(ctx, sql)
	if err != nil {
		return "", 0, err
	}
	n, _ := res.RowsAffected()
	return dir + "/**/*.parquet", n, nil
}

// ExportToParquet exports the logs table to a single Parquet file (local path).
func (s *DuckDBShard) ExportToParquet(path string) error {
	_, err := s.db.Exec(fmt.Sprintf("COPY %s TO '%s' (FORMAT PARQUET)", s.source(), path))
	return err
}

// ExportToS3Parquet exports the logs table to a single Parquet file on S3.
func (s *DuckDBShard) ExportToS3Parquet(s3Path string, cfg *S3Config) error {
	if err := cfg.ConfigureDuckDB(s.db); err != nil {
		return err
	}
	_, err := s.db.Exec(fmt.Sprintf("COPY %s TO '%s' (FORMAT PARQUET)", s.source(), s3Path))
	return err
}

// Query executes a SQL query and returns the matching result rows.
// The SQL is expected to return (raw, ts). args binds positional
// `?` placeholders (planner-supplied customer id, timestamps,
// namespace/env/service names); pass none for legacy fully-baked SQL.
func (s *DuckDBShard) Query(ctx context.Context, query string, args ...any) ([]*pb.ResultRow, error) {
	return s.QueryContext(ctx, query, args...)
}

// QueryContext is Query with a deadline. The DuckDB Go driver honors
// context cancellation at row boundaries, so a context timeout trims
// pathological long-running queries (the main motivation for the
// SHARD_QUERY_TIMEOUT_SECONDS runtime config).
func (s *DuckDBShard) QueryContext(ctx context.Context, query string, args ...any) ([]*pb.ResultRow, error) {
	return s.QueryContextTable(ctx, "", query, args...)
}

// QueryContextTable is QueryContext for a query that reads one log source,
// whose table the planner named (logtable.Name). A shard that keeps a table per
// key reads only that table; one that does not runs the query as written, so
// the hint is safe to send to any shard. See resolveSource.
func (s *DuckDBShard) QueryContextTable(ctx context.Context, table, query string, args ...any) ([]*pb.ResultRow, error) {
	query = s.resolveSource(query, table)
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
