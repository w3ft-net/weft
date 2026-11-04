package shard

import (
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

	_ "github.com/duckdb/duckdb-go/v2"

	weftsdk "github.com/w3ft-net/weft-go"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// lambdaHandler is a test HTTP handler that mimics the Lambda query handler.
func lambdaHandler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t.Helper()

		var req struct {
			SQL         string `json:"sql"`
			ParquetPath string `json:"parquet_path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}

		db, err := sql.Open("duckdb", "")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer db.Close()

		query := strings.Replace(req.SQL, "FROM logs", fmt.Sprintf("FROM read_parquet('%s')", req.ParquetPath), 1)
		rows, err := db.Query(query)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()

		results, err := ScanResultRows(rows)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}

		w.Header().Set("Content-Type", "application/x-protobuf-stream")
		for _, row := range results {
			WriteResultRow(w, row)
		}
		WriteStreamEnd(w)
	}
}

func TestColdShard_RemoteQuery(t *testing.T) {
	parquetPath := exportToParquet(t, testRows())

	srv := httptest.NewServer(lambdaHandler(t))
	defer srv.Close()

	cs, err := NewColdShard("remote-test", []string{"vimgym", "callbox"}, parquetPath, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	rows, err := cs.Query(context.Background(), "SELECT raw, ts FROM logs WHERE customer_id = 1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("expected 4 kevlar rows, got %d", len(rows))
	}
}

func TestColdShard_RemoteQueryToSink(t *testing.T) {
	parquetPath := exportToParquet(t, testRows())

	srv := httptest.NewServer(lambdaHandler(t))
	defer srv.Close()

	cs, err := NewColdShard("remote-test", []string{"vimgym"}, parquetPath, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	sink := &testSink{}
	cs.QueryToSink(context.Background(), "SELECT raw, ts FROM logs WHERE namespace = 'vimgym' AND customer_id = 1", nil, sink)

	if !sink.sentinelReceived {
		t.Error("expected sentinel")
	}
	if len(sink.rows) != 2 {
		t.Fatalf("expected 2 vimgym/kevlar rows via sink, got %d", len(sink.rows))
	}
}

// TestColdShard_RemoteQueryToSink_CarriesTraceID proves the mechanism
// this whole dogfooding plan depends on: an ambient trace ID set via
// weftsdk.ContextWithTraceID survives all the way into the JSON body
// of the request sent to a cold-shard Lambda — the Lambda is a
// separate AWS-managed process that can't read Go context values at
// all, so the body field is the only channel.
func TestColdShard_RemoteQueryToSink_CarriesTraceID(t *testing.T) {
	parquetPath := exportToParquet(t, testRows())

	var gotTraceID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req LambdaQueryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		gotTraceID = req.TraceId
		w.Header().Set("Content-Type", "application/x-protobuf-stream")
		WriteStreamEnd(w)
	}))
	defer srv.Close()

	cs, err := NewColdShard("remote-trace-test", []string{"vimgym"}, parquetPath, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	ctx := weftsdk.ContextWithTraceID(context.Background(), "test-trace-123")
	sink := &testSink{}
	cs.QueryToSink(ctx, "SELECT raw, ts FROM logs WHERE namespace = 'vimgym' AND customer_id = 1", nil, sink)

	if gotTraceID != "test-trace-123" {
		t.Fatalf("expected lambda request body to carry trace_id=test-trace-123, got %q", gotTraceID)
	}
}

func TestColdShard_RemoteReadOnly(t *testing.T) {
	cs, err := NewColdShard("remote-test", nil, "", "http://unused", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	err = cs.Ingest(context.Background(), []*pb.LogRow{{Raw: "nope", TsUnixNs: time.Now().UnixNano()}}, nil)
	if err == nil {
		t.Fatal("expected error on Ingest")
	}
}

func TestColdShard_RemoteFiltersByApp(t *testing.T) {
	parquetPath := exportToParquet(t, testRows())

	srv := httptest.NewServer(lambdaHandler(t))
	defer srv.Close()

	cs, err := NewColdShard("remote-test", []string{"callbox"}, parquetPath, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	rows, err := cs.Query(context.Background(), "SELECT raw, ts FROM logs WHERE namespace = 'callbox' AND customer_id = 1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 callbox rows, got %d", len(rows))
	}
}

func BenchmarkColdShard_RemoteQuery(b *testing.B) {
	ds, err := NewDuckDBShard("bench", "")
	if err != nil {
		b.Fatal(err)
	}
	rows := make([]*pb.LogRow, 10_000)
	for i := range rows {
		rows[i] = &pb.LogRow{
			Raw: "bench log line", Namespace: "vimgym",
			Level: "INFO", CustomerId: 1,
			TsUnixNs: coldT0.Add(time.Duration(i) * time.Millisecond).UnixNano(),
		}
	}
	ds.Ingest(context.Background(), rows, nil)
	path := filepath.Join(b.TempDir(), "bench.parquet")
	ds.ExportToParquet(path)
	ds.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			SQL         string `json:"sql"`
			ParquetPath string `json:"parquet_path"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		db, _ := sql.Open("duckdb", "")
		defer db.Close()
		query := strings.Replace(req.SQL, "FROM logs", fmt.Sprintf("FROM read_parquet('%s')", req.ParquetPath), 1)
		sqlRows, _ := db.Query(query)
		defer sqlRows.Close()
		results, _ := ScanResultRows(sqlRows)
		for _, row := range results {
			WriteResultRow(w, row)
		}
		WriteStreamEnd(w)
	}))
	defer srv.Close()

	cs, err := NewColdShard("bench", []string{"vimgym"}, path, srv.URL, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer cs.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cs.Query(context.Background(), "SELECT raw, ts FROM logs WHERE level = 'ERROR' AND customer_id = 1")
	}
}

// The Lambda is a separate process that can't see the coordinator's context,
// and a Function URL doesn't reliably surface a dropped connection, so the
// query's deadline travels in the body.
func TestColdShard_RemoteQueryToSink_CarriesDeadline(t *testing.T) {
	parquetPath := exportToParquet(t, testRows())

	got := make(chan LambdaQueryRequest, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req LambdaQueryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		got <- req
		w.Header().Set("Content-Type", "application/x-protobuf-stream")
		WriteStreamEnd(w)
	}))
	defer srv.Close()

	cs, err := NewColdShard("remote-deadline-test", []string{"vimgym"}, parquetPath, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	const q = "SELECT raw, ts FROM logs WHERE namespace = 'vimgym' AND customer_id = 1"

	deadline := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	cs.QueryToSink(ctx, q, nil, &testSink{})
	if req := <-got; req.DeadlineUnixMs != deadline.UnixMilli() {
		t.Errorf("deadline_unix_ms = %d, want %d", req.DeadlineUnixMs, deadline.UnixMilli())
	}

	cs.QueryToSink(context.Background(), q, nil, &testSink{})
	if req := <-got; req.DeadlineUnixMs != 0 {
		t.Errorf("deadline_unix_ms = %d for a query with no deadline, want 0", req.DeadlineUnixMs)
	}

	if _, err := cs.Query(ctx, q); err != nil {
		t.Fatal(err)
	}
	if req := <-got; req.DeadlineUnixMs != deadline.UnixMilli() {
		t.Errorf("Query: deadline_unix_ms = %d, want %d", req.DeadlineUnixMs, deadline.UnixMilli())
	}
}
