package main

import (
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
type QueryRequest struct {
	SQL         string `json:"sql"`
	ParquetPath string `json:"parquet_path"`
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
	query := strings.Replace(req.SQL, "FROM logs", fmt.Sprintf("FROM read_parquet('%s')", req.ParquetPath), 1)

	rows, err := db.Query(query)
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	// Scan and stream rows.
	results, err := shard.ScanRows(rows)
	if err != nil {
		return fmt.Errorf("scan: %w", err)
	}

	for _, row := range results {
		if err := shard.WriteLogRow(w, row); err != nil {
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

	w.Header().Set("Content-Type", "application/x-protobuf-stream")
	w.WriteHeader(http.StatusOK)

	// Flush header immediately for streaming.
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	if err := HandleQuery(w, req, s3cfg); err != nil {
		// Can't send HTTP error after headers are written — log it.
		fmt.Fprintf(w, "\nerror: %v\n", err)
	}
}
