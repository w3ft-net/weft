package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
)

func testParquet(t *testing.T) string {
	t.Helper()
	ds, err := shard.NewDuckDBShard("test", "")
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()

	rows := []*pb.LogRow{
		{Raw: "error in vimgym", Namespace: "vimgym", Level: "ERROR", CustomerId: 1, TsUnixNs: time.Now().UnixNano()},
		{Raw: "info in vimgym", Namespace: "vimgym", Level: "INFO", CustomerId: 1, TsUnixNs: time.Now().UnixNano()},
		{Raw: "warn in callbox", Namespace: "callbox", Level: "WARN", CustomerId: 1, TsUnixNs: time.Now().UnixNano()},
	}
	if err := ds.Ingest(context.Background(), rows, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "test.parquet")
	if err := ds.ExportToParquet(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLambdaHandler_QueryParquet(t *testing.T) {
	path := testParquet(t)
	var buf bytes.Buffer

	err := HandleQuery(context.Background(), &buf, QueryRequest{
		SQL:         "SELECT raw, ts FROM logs WHERE customer_id = 1",
		ParquetPath: path,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	rows, err := shard.ReadAllResultRows(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}
}

func TestLambdaHandler_FiltersByApp(t *testing.T) {
	path := testParquet(t)
	var buf bytes.Buffer

	err := HandleQuery(context.Background(), &buf, QueryRequest{
		SQL:         "SELECT raw, ts FROM logs WHERE namespace = 'vimgym' AND customer_id = 1",
		ParquetPath: path,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	rows, err := shard.ReadAllResultRows(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 vimgym rows, got %d", len(rows))
	}
}

// TestServeHTTP_QueryFailureReturnsErrorStatus is the observability
// contract: when the query fails (e.g. the parquet snapshot is missing
// from S3), the handler must return a non-2xx status so the coordinator
// logs "lambda returned 5xx" instead of silently dropping the shard's
// rows. The pre-fix handler flushed a 200 header before running the
// query, so failures were smuggled into the body as text and the
// client saw 200 + zero rows with no log line anywhere.
func TestServeHTTP_QueryFailureReturnsErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(ServeHTTP))
	defer srv.Close()

	body, _ := json.Marshal(QueryRequest{
		SQL:         "SELECT raw, ts FROM logs WHERE customer_id = 1",
		ParquetPath: "/nonexistent/missing-shard.parquet",
	})
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		t.Fatalf("query against missing parquet returned 200 — failure is "+
			"invisible to the coordinator; want a 5xx status (got %d)", resp.StatusCode)
	}
	if resp.StatusCode < 500 {
		t.Fatalf("expected a 5xx server error, got %d", resp.StatusCode)
	}

	rows, _ := shard.ReadAllResultRows(resp.Body)
	if len(rows) != 0 {
		t.Fatalf("error response should carry no result rows, got %d", len(rows))
	}
}

// TestServeHTTP_SuccessStreamsRows guards the happy path through the
// HTTP surface (not just HandleQuery): 200 + parseable rows.
func TestServeHTTP_SuccessStreamsRows(t *testing.T) {
	path := testParquet(t)
	srv := httptest.NewServer(http.HandlerFunc(ServeHTTP))
	defer srv.Close()

	body, _ := json.Marshal(QueryRequest{
		SQL:         "SELECT raw, ts FROM logs WHERE customer_id = 1",
		ParquetPath: path,
	})
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	rows, err := shard.ReadAllResultRows(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}
	if !strings.Contains(rows[0].Raw, "vimgym") && !strings.Contains(rows[0].Raw, "callbox") {
		t.Fatalf("unexpected row content: %q", rows[0].Raw)
	}
}

// TestServeHTTP_StreamsIncrementally guards against the buffer-the-
// whole-result-then-write regression: a streamed response flushed per
// row uses chunked transfer encoding with no Content-Length. A single
// buffered Write would let net/http set a Content-Length instead.
func TestServeHTTP_StreamsIncrementally(t *testing.T) {
	path := testParquet(t)
	srv := httptest.NewServer(http.HandlerFunc(ServeHTTP))
	defer srv.Close()

	body, _ := json.Marshal(QueryRequest{
		SQL:         "SELECT raw, ts FROM logs WHERE customer_id = 1",
		ParquetPath: path,
	})
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		t.Fatalf("response has Content-Length %q — result was buffered, not streamed", cl)
	}
	chunked := false
	for _, te := range resp.TransferEncoding {
		if te == "chunked" {
			chunked = true
		}
	}
	if !chunked {
		t.Fatalf("expected chunked transfer encoding (streamed), got %v", resp.TransferEncoding)
	}
	rows, err := shard.ReadAllResultRows(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}
}

func TestLambdaHandler_EmptyResult(t *testing.T) {
	path := testParquet(t)
	var buf bytes.Buffer

	err := HandleQuery(context.Background(), &buf, QueryRequest{
		SQL:         "SELECT raw, ts FROM logs WHERE namespace = 'nonexistent'",
		ParquetPath: path,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	rows, err := shard.ReadAllResultRows(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected 0 rows, got %d", len(rows))
	}
}

// The coordinator pushes queries down whose rows carry more than raw and ts:
// a count aggregate for `wc -l`, event_id and line_no for dedup, meta-vars for
// jq. The hot shard scans all of them; the cold path has to as well, or every
// such query against archived data comes back empty.
func TestLambdaHandler_ScansExtraColumnsLikeAHotShard(t *testing.T) {
	path := testParquet(t)

	var counted bytes.Buffer
	if err := HandleQuery(context.Background(), &counted, QueryRequest{
		SQL:         "SELECT '' AS raw, min(ts) AS ts, count(*) AS count FROM logs WHERE customer_id = 1 HAVING count(*) > 0",
		ParquetPath: path,
	}, nil); err != nil {
		t.Fatalf("count query: %v", err)
	}
	rows, err := shard.ReadAllResultRows(&counted)
	if err != nil {
		t.Fatalf("read count rows: %v", err)
	}
	if len(rows) != 1 || rows[0].MetaVars["count"] != "3" {
		t.Fatalf("count query rows = %v, want one row with count=3", rows)
	}

	var withIDs bytes.Buffer
	if err := HandleQuery(context.Background(), &withIDs, QueryRequest{
		SQL:         "SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE customer_id = 1",
		ParquetPath: path,
	}, nil); err != nil {
		t.Fatalf("event_id query: %v", err)
	}
	rows, err = shard.ReadAllResultRows(&withIDs)
	if err != nil {
		t.Fatalf("read event_id rows: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("event_id query rows = %v, want 3 rows", rows)
	}
}

// A query the coordinator has given up on must stop billing: the request
// carries the coordinator's deadline and the handler interrupts DuckDB there,
// instead of running to the Lambda's own timeout.
func TestHandleQuery_StopsAtTheCoordinatorsDeadline(t *testing.T) {
	path := testParquet(t)
	deadline := time.Now().Add(300 * time.Millisecond)

	start := time.Now()
	err := HandleQuery(context.Background(), &bytes.Buffer{}, QueryRequest{
		// A cross join this large would run for minutes.
		SQL:            "SELECT 'x' AS raw, now() AS ts, count(*) AS n FROM logs, range(100000000000)",
		ParquetPath:    path,
		DeadlineUnixMs: deadline.UnixMilli(),
	}, nil)
	if err == nil {
		t.Fatal("query ran to completion past its deadline")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("query kept running %s after a deadline %s out", took, time.Until(deadline)+took)
	}
}

func TestHandleQuery_NoDeadlineRunsToCompletion(t *testing.T) {
	var buf bytes.Buffer
	err := HandleQuery(context.Background(), &buf, QueryRequest{
		SQL:         "SELECT raw, ts FROM logs WHERE customer_id = 1",
		ParquetPath: testParquet(t),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
}

// The stream ends with what the query scanned. DuckDB's profile counts a scan
// once per busy thread; the handler divides that back out, so the answer is the
// same on a laptop and in the Lambda. The file is big enough to keep every
// thread busy.
func TestHandleQuery_ReportsRowsScannedInATrailer(t *testing.T) {
	const total = 1_000_000
	path := filepath.Join(t.TempDir(), "big.parquet")
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf("COPY (SELECT TIMESTAMP '2026-01-01' + to_seconds(i) AS ts, 'line ' || i AS raw, 'x' AS event_id, 0 AS line_no, 1 AS customer_id FROM range(%d) t(i)) TO '%s' (FORMAT parquet, ROW_GROUP_SIZE 10000)", total, path)); err != nil {
		t.Fatal(err)
	}
	db.Close()

	var buf bytes.Buffer
	err = HandleQuery(context.Background(), &buf, QueryRequest{
		SQL:         "SELECT raw, ts FROM logs WHERE customer_id = 1 AND regexp_matches(raw, 'line 99999')",
		ParquetPath: path,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	rows, err := shard.ReadAllResultRows(&buf)
	if err != nil || len(rows) != 11 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	stats, ok, err := shard.ReadScanStats(&buf)
	if err != nil || !ok {
		t.Fatalf("no trailer: ok=%v err=%v", ok, err)
	}
	if stats.RowsScanned != total {
		t.Errorf("RowsScanned = %d, want %d", stats.RowsScanned, total)
	}
}

// A scan too small to keep DuckDB's threads busy still reads as scanned.
func TestHandleQuery_ATinyScanIsNotZero(t *testing.T) {
	var buf bytes.Buffer
	if err := HandleQuery(context.Background(), &buf, QueryRequest{
		SQL:         "SELECT raw, ts FROM logs WHERE customer_id = 1 AND regexp_matches(raw, 'error')",
		ParquetPath: testParquet(t),
	}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := shard.ReadAllResultRows(&buf); err != nil {
		t.Fatal(err)
	}
	stats, ok, _ := shard.ReadScanStats(&buf)
	if !ok || stats.RowsScanned < 1 {
		t.Errorf("stats = %+v ok=%v, want some rows scanned", stats, ok)
	}
}
