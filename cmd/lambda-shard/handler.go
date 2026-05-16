package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	_ "github.com/duckdb/duckdb-go/v2"

	"github.com/kevlarcon/weft/shard"
)

// QueryRequest is the JSON request body for a cold shard query.
// Args carries positional placeholder values for `?` markers in SQL;
// empty for legacy callers still using string-interpolated SQL. Keep
// in sync with shard.LambdaQueryRequest.
type QueryRequest struct {
	SQL         string   `json:"sql"`
	ParquetPath string   `json:"parquet_path"`
	Args        []string `json:"args,omitempty"`
}

// HandleQuery executes a SQL query against a Parquet file and streams
// length-prefixed protobuf LogRow messages to the writer.
// This function is testable without the Lambda runtime.
func HandleQuery(w io.Writer, req QueryRequest, s3cfg *shard.S3Config) error {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return fmt.Errorf("open duckdb: %w", err)
	}
	defer db.Close()

	// Configure S3 if needed.
	if s3cfg != nil {
		if err := s3cfg.ConfigureDuckDB(db); err != nil {
			return fmt.Errorf("configure s3: %w", err)
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
		return fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	// Scan and stream rows.
	results, err := shard.ScanResultRows(rows)
	if err != nil {
		return fmt.Errorf("scan: %w", err)
	}

	for _, row := range results {
		if err := shard.WriteResultRow(w, row); err != nil {
			return fmt.Errorf("write row: %w", err)
		}
	}
	return shard.WriteStreamEnd(w)
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

	s3cfg := shard.S3ConfigFromEnv()

	// Run the query into a buffer before writing the status. HandleQuery
	// already materializes every result row (ScanResultRows) before it
	// emits, so buffering does not change the memory profile — but it
	// lets us return an honest HTTP status. The old path flushed a 200
	// header first, so a failed query (missing parquet, bad SQL) was
	// smuggled into the body as text: the coordinator saw 200, failed to
	// parse the protobuf stream, and silently dropped the shard's rows
	// with no log line anywhere. A real 5xx makes the existing
	// "lambda returned %d" client log fire.
	var buf bytes.Buffer
	if err := HandleQuery(&buf, req, s3cfg); err != nil {
		http.Error(w, fmt.Sprintf("query failed: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-protobuf-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}
