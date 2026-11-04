package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	weftsdk "github.com/w3ft-net/weft-go"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"

	"github.com/graybeardcorp/weft/shard"
)

// QueryRequest is the JSON request body for a cold shard query.
// Args carries positional placeholder values for `?` markers in SQL;
// empty for legacy callers still using string-interpolated SQL. Keep
// in sync with shard.LambdaQueryRequest.
type QueryRequest struct {
	SQL         string   `json:"sql"`
	ParquetPath string   `json:"parquet_path"`
	Args        []string `json:"args,omitempty"`
	// TraceId is the ambient trace ID (if any) ferried explicitly in the
	// body — this process can't read a caller's Go context.Context.
	// Keep in sync with shard.LambdaQueryRequest.
	TraceId string `json:"trace_id,omitempty"`
	// DeadlineUnixMs is when the coordinator stops waiting for this query;
	// zero means no deadline. A Function URL doesn't reliably tell the
	// function its caller hung up, so without this an abandoned query runs
	// (and bills) to the Lambda's own timeout. Keep in sync with
	// shard.LambdaQueryRequest.
	DeadlineUnixMs int64 `json:"deadline_unix_ms,omitempty"`
}

// queryContext bounds ctx by the request's deadline. The caller cancels it
// once the rows are drained.
func (req QueryRequest) queryContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if req.DeadlineUnixMs == 0 {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, time.UnixMilli(req.DeadlineUnixMs))
}

// coldQuery is one query in flight: the connection it runs on and the rows
// it is returning. Profiling is on for the connection so the query can say
// what it scanned.
type coldQuery struct {
	db          *sql.DB
	conn        *sql.Conn
	rows        *sql.Rows
	profilePath string
	threads     int64
}

// Close releases the query's resources. Close it before reading Stats only
// if the rows were not drained.
func (q *coldQuery) Close() {
	if q.rows != nil {
		q.rows.Close()
	}
	q.conn.Close()
	q.db.Close()
	os.Remove(q.profilePath)
}

// Stats is what the query scanned. Call it once the rows are drained: DuckDB
// writes the profile as the query finishes.
func (q *coldQuery) Stats() shard.ScanStats {
	b, err := os.ReadFile(q.profilePath)
	if err != nil || len(b) == 0 {
		// A bare count(*) answers from metadata and leaves no profile.
		return shard.ScanStats{}
	}
	var p struct {
		RowsScanned int64 `json:"cumulative_rows_scanned"`
		BytesRead   int64 `json:"total_bytes_read"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return shard.ScanStats{}
	}
	// The profile counts a scan once per thread that worked on it: N busy
	// threads report N times the rows read (cmd/lambda-shard/rows_read_test.go).
	// A scan too small to keep them busy reports less than that, so the
	// division rounds up and undercounts only scans too small to matter.
	threads := max(q.threads, 1)
	return shard.ScanStats{RowsScanned: (p.RowsScanned + threads - 1) / threads, BytesRead: p.BytesRead}
}

// runQuery opens an in-memory DuckDB, configures S3, and executes the
// rewritten query. Every failure here is pre-stream — a missing parquet
// snapshot or bad SQL surfaces from the query before any bytes are
// written — so the caller can still return an honest non-2xx status.
// On success the caller owns Close; ctx must stay live until the rows are
// drained, since cancelling it interrupts DuckDB.
func runQuery(ctx context.Context, req QueryRequest, s3cfg *shard.S3Config) (*coldQuery, error) {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, fmt.Errorf("open duckdb: %w", err)
	}

	if s3cfg != nil {
		if err := s3cfg.ConfigureDuckDB(db); err != nil {
			db.Close()
			return nil, fmt.Errorf("configure s3: %w", err)
		}
	}

	// Profiling is per connection, so the query runs on one of its own.
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("open connection: %w", err)
	}
	profile, err := os.CreateTemp("", "profile-*.json")
	if err != nil {
		conn.Close()
		db.Close()
		return nil, fmt.Errorf("profile file: %w", err)
	}
	profile.Close()
	q := &coldQuery{db: db, conn: conn, profilePath: profile.Name()}
	fail := func(err error) (*coldQuery, error) {
		q.Close()
		return nil, err
	}
	for _, stmt := range []string{"PRAGMA enable_profiling='json'", fmt.Sprintf("PRAGMA profiling_output='%s'", q.profilePath)} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return fail(fmt.Errorf("profiling: %w", err))
		}
	}
	if err := conn.QueryRowContext(ctx, "SELECT current_setting('threads')::BIGINT").Scan(&q.threads); err != nil {
		return fail(fmt.Errorf("threads: %w", err))
	}

	// Rewrite FROM logs → FROM read_parquet('path').
	// Glob paths (partitioned archives) enable hive_partitioning for file pruning.
	sqlText := req.SQL
	if strings.Contains(req.ParquetPath, "*") {
		sqlText = shard.PruneColdPartitions(sqlText, req.Args, req.ParquetPath)
	}
	query := strings.Replace(sqlText, "FROM logs", "FROM "+shard.ParquetReadExpr(req.ParquetPath), 1)

	args := make([]any, len(req.Args))
	for i, a := range req.Args {
		args[i] = a
	}
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return fail(fmt.Errorf("query: %w", err))
	}
	q.rows = rows
	return q, nil
}

// streamRows scans rows one at a time and writes each as a
// length-prefixed protobuf message, flushing after every row so the
// coordinator receives results as they are produced rather than after
// the whole result set is materialized. A non-nil error means the
// stream is truncated (no end sentinel): callers past the HTTP header
// can only log it — the coordinator's stream-read-error path then
// surfaces the truncation on its side. On success the caller ends the
// stream with finishStream.
func streamRows(w io.Writer, rows *sql.Rows) (int, error) {
	flush := func() {}
	if f, ok := w.(http.Flusher); ok {
		flush = f.Flush
	}

	cols, err := rows.Columns()
	if err != nil {
		return 0, fmt.Errorf("columns: %w", err)
	}
	extraCols := cols[2:]

	count := 0
	for rows.Next() {
		row, err := shard.ScanResultRow(rows, extraCols)
		if err != nil {
			return count, err
		}
		if err := shard.WriteResultRow(w, row); err != nil {
			return count, fmt.Errorf("write row: %w", err)
		}
		count++
		flush()
	}
	if err := rows.Err(); err != nil {
		return count, fmt.Errorf("rows: %w", err)
	}
	return count, nil
}

// finishStream ends the stream with the sentinel, then the scan stats. The
// rows must be drained: DuckDB writes its profile as the query finishes.
func finishStream(w io.Writer, q *coldQuery) (shard.ScanStats, error) {
	if err := shard.WriteStreamEnd(w); err != nil {
		return shard.ScanStats{}, err
	}
	stats := q.Stats()
	return stats, shard.WriteScanStats(w, stats)
}

// HandleQuery executes a SQL query against a Parquet file and streams
// length-prefixed protobuf ResultRow messages to the writer, one row at
// a time. Testable without the Lambda runtime.
func HandleQuery(ctx context.Context, w io.Writer, req QueryRequest, s3cfg *shard.S3Config) error {
	ctx, cancel := req.queryContext(ctx)
	defer cancel()
	q, err := runQuery(ctx, req, s3cfg)
	if err != nil {
		return err
	}
	defer q.Close()
	if _, err := streamRows(w, q.rows); err != nil {
		return err
	}
	_, err = finishStream(w, q)
	return err
}

// ServeHTTP is the HTTP handler for the query endpoint.
// Used by both the Lambda function URL handler and the test httptest.Server.
func ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	var req QueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("bad request: %v", err), http.StatusBadRequest)
		return
	}

	if req.SQL == "" || req.ParquetPath == "" {
		http.Error(w, "sql and parquet_path required", http.StatusBadRequest)
		return
	}

	// The trace ID arrives in the body, not the request's context.
	logCtx := r.Context()
	if req.TraceId != "" {
		logCtx = weftsdk.ContextWithTraceID(logCtx, req.TraceId)
	}

	s3cfg := shard.S3ConfigFromEnv()
	start := time.Now()
	ctx, cancel := req.queryContext(r.Context())
	defer cancel()

	// Execute before writing the status. A failed query (missing
	// parquet, bad SQL) surfaces from db.Query() with no bytes written,
	// so it becomes an honest 500 the coordinator logs as
	// "lambda returned 500" — instead of the old path, which flushed a
	// 200 header first and smuggled the error into the body as text
	// where the coordinator parsed it as a corrupt stream and silently
	// dropped the shard's rows.
	q, err := runQuery(ctx, req, s3cfg)
	if err != nil {
		http.Error(w, fmt.Sprintf("query failed: %v", err), http.StatusInternalServerError)
		return
	}
	defer q.Close()

	w.Header().Set("Content-Type", "application/x-protobuf-stream")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// Past the header now: a mid-stream failure (e.g. an S3 read
	// interrupted after the parquet opened) can't change the status.
	// We stop without the end sentinel; the coordinator's
	// queryToSinkRemote logs the truncated stream on its side.
	count, err := streamRows(w, q.rows)
	if err != nil {
		weftsdk.Infof(logCtx, "lambda-shard: mid-stream failure after header sent: %v", err)
		return
	}
	stats, err := finishStream(w, q)
	if err != nil {
		weftsdk.Infof(logCtx, "lambda-shard: ending the stream failed: %v", err)
		return
	}
	// Unconditional completion log, matching the caller-side
	// (shard.ColdShard.queryToSinkRemote) contract — no shard id is
	// available on this side of the wire, so parquet path identifies
	// which archive was read.
	weftsdk.Infof(logCtx, "lambda-shard: query completed in %s parquet=%s (%d rows, rows_scanned=%d bytes_read=%d)", time.Since(start), req.ParquetPath, count, stats.RowsScanned, stats.BytesRead)
}
