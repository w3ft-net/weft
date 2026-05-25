package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
)

func testParquet(t *testing.T) string {
	t.Helper()
	ds, err := shard.NewDuckDBShard("test", "")
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()

	rows := []*pb.LogRow{
		{Raw: "error in vimgym", Namespace: "vimgym", Level: "ERROR", Customer: "kevlar", TsUnixNs: time.Now().UnixNano()},
		{Raw: "info in vimgym", Namespace: "vimgym", Level: "INFO", Customer: "kevlar", TsUnixNs: time.Now().UnixNano()},
		{Raw: "warn in callbox", Namespace: "callbox", Level: "WARN", Customer: "kevlar", TsUnixNs: time.Now().UnixNano()},
	}
	if err := ds.Ingest(rows, nil); err != nil {
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

	err := HandleQuery(&buf, QueryRequest{
		SQL:         "SELECT raw, ts FROM logs WHERE customer = 'kevlar'",
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

	err := HandleQuery(&buf, QueryRequest{
		SQL:         "SELECT raw, ts FROM logs WHERE app = 'vimgym' AND customer = 'kevlar'",
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
		SQL:         "SELECT raw, ts FROM logs WHERE customer = 'kevlar'",
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
		SQL:         "SELECT raw, ts FROM logs WHERE customer = 'kevlar'",
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
		SQL:         "SELECT raw, ts FROM logs WHERE customer = 'kevlar'",
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

	err := HandleQuery(&buf, QueryRequest{
		SQL:         "SELECT raw, ts FROM logs WHERE app = 'nonexistent'",
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
