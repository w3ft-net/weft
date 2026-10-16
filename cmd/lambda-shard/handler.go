package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	weftsdk "github.com/w3ft-net/weft-go"
	"io"
	"net/http"
	"strings"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"

	"github.com/graybeardcorp/weft/internal/weftlog"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
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
}

// runQuery opens an in-memory DuckDB, configures S3, and executes the
// rewritten query. Every failure here is pre-stream — a missing parquet
// snapshot or bad SQL surfaces from db.Query() before any bytes are
// written — so the caller can still return an honest non-2xx status.
// On success the caller owns closing both the *sql.DB and *sql.Rows.
func runQuery(req QueryRequest, s3cfg *shard.S3Config) (*sql.DB, *sql.Rows, error) {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, nil, fmt.Errorf("open duckdb: %w", err)
	}

	if s3cfg != nil {
		if err := s3cfg.ConfigureDuckDB(db); err != nil {
			db.Close()
			return nil, nil, fmt.Errorf("configure s3: %w", err)
		}
	}

	// Rewrite FROM logs → FROM read_parquet('path').
	// Glob paths (partitioned archives) enable hive_partitioning for file pruning.
	var readExpr string
	if strings.Contains(req.ParquetPath, "*") {
		readExpr = fmt.Sprintf("read_parquet('%s', hive_partitioning=true)", req.ParquetPath)
	} else {
		readExpr = fmt.Sprintf("read_parquet('%s')", req.ParquetPath)
	}
	query := strings.Replace(req.SQL, "FROM logs", "FROM "+readExpr, 1)

	args := make([]any, len(req.Args))
	for i, a := range req.Args {
		args[i] = a
	}
	rows, err := db.Query(query, args...)
	if err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("query: %w", err)
	}
	return db, rows, nil
}

// streamRows scans rows one at a time and writes each as a
// length-prefixed protobuf message, flushing after every row so the
// coordinator receives results as they are produced rather than after
// the whole result set is materialized. A non-nil error means the
// stream is truncated (no end sentinel): callers past the HTTP header
// can only log it — the coordinator's stream-read-error path then
// surfaces the truncation on its side.
func streamRows(w io.Writer, rows *sql.Rows) (int, error) {
	flush := func() {}
	if f, ok := w.(http.Flusher); ok {
		flush = f.Flush
	}

	count := 0
	for rows.Next() {
		var raw string
		var ts time.Time
		if err := rows.Scan(&raw, &ts); err != nil {
			return count, fmt.Errorf("scan row: %w", err)
		}
		if err := shard.WriteResultRow(w, &pb.ResultRow{Raw: raw, TsUnixNs: ts.UnixNano()}); err != nil {
			return count, fmt.Errorf("write row: %w", err)
		}
		count++
		flush()
	}
	if err := rows.Err(); err != nil {
		return count, fmt.Errorf("rows: %w", err)
	}
	return count, shard.WriteStreamEnd(w)
}

// HandleQuery executes a SQL query against a Parquet file and streams
// length-prefixed protobuf ResultRow messages to the writer, one row at
// a time. Testable without the Lambda runtime.
func HandleQuery(w io.Writer, req QueryRequest, s3cfg *shard.S3Config) error {
	db, rows, err := runQuery(req, s3cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	defer rows.Close()
	_, err = streamRows(w, rows)
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

	// Execute before writing the status. A failed query (missing
	// parquet, bad SQL) surfaces from db.Query() with no bytes written,
	// so it becomes an honest 500 the coordinator logs as
	// "lambda returned 500" — instead of the old path, which flushed a
	// 200 header first and smuggled the error into the body as text
	// where the coordinator parsed it as a corrupt stream and silently
	// dropped the shard's rows.
	db, rows, err := runQuery(req, s3cfg)
	if err != nil {
		http.Error(w, fmt.Sprintf("query failed: %v", err), http.StatusInternalServerError)
		return
	}
	defer db.Close()
	defer rows.Close()

	w.Header().Set("Content-Type", "application/x-protobuf-stream")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// Past the header now: a mid-stream failure (e.g. an S3 read
	// interrupted after the parquet opened) can't change the status.
	// We stop without the end sentinel; the coordinator's
	// queryToSinkRemote logs the truncated stream on its side.
	count, err := streamRows(w, rows)
	if err != nil {
		weftlog.Infof(logCtx, "lambda-shard: mid-stream failure after header sent: %v", err)
		return
	}
	// Unconditional completion log, matching the caller-side
	// (shard.ColdShard.queryToSinkRemote) contract — no shard id is
	// available on this side of the wire, so parquet path identifies
	// which archive was read.
	weftlog.Infof(logCtx, "lambda-shard: query completed in %s parquet=%s (%d rows)", time.Since(start), req.ParquetPath, count)
}
