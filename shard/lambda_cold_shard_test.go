package shard

import (
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

	pb "github.com/kevlarcon/weft/proto/weftpb"
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

func TestLambdaColdShard_Query(t *testing.T) {
	parquetPath := exportToParquet(t, testRows())

	srv := httptest.NewServer(lambdaHandler(t))
	defer srv.Close()

	shard := NewLambdaColdShard("lambda-test", []string{"vimgym", "callbox"}, srv.URL, parquetPath)

	rows, err := shard.Query("SELECT line_num, raw FROM logs WHERE customer = 'kevlar'")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 { // 5 total minus 1 acme customer
		t.Fatalf("expected 4 kevlar rows, got %d", len(rows))
	}
}

func TestLambdaColdShard_QueryToSink(t *testing.T) {
	parquetPath := exportToParquet(t, testRows())

	srv := httptest.NewServer(lambdaHandler(t))
	defer srv.Close()

	shard := NewLambdaColdShard("lambda-test", []string{"vimgym"}, srv.URL, parquetPath)

	sink := &testSink{}
	shard.QueryToSink("SELECT line_num, raw FROM logs WHERE app = 'vimgym' AND customer = 'kevlar'", sink)

	if !sink.sentinelReceived {
		t.Error("expected sentinel")
	}
	if len(sink.rows) != 2 {
		t.Fatalf("expected 2 vimgym/kevlar rows via sink, got %d", len(sink.rows))
	}
}

func TestLambdaColdShard_ReadOnly(t *testing.T) {
	shard := NewLambdaColdShard("lambda-test", nil, "http://unused", "")
	err := shard.Ingest([]*pb.LogRow{{Raw: "nope"}})
	if err == nil {
		t.Fatal("expected error on Ingest")
	}
}

func TestLambdaColdShard_FiltersByApp(t *testing.T) {
	parquetPath := exportToParquet(t, testRows())

	srv := httptest.NewServer(lambdaHandler(t))
	defer srv.Close()

	shard := NewLambdaColdShard("lambda-test", []string{"callbox"}, srv.URL, parquetPath)

	rows, err := shard.Query("SELECT line_num, raw FROM logs WHERE app = 'callbox' AND customer = 'kevlar'")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 callbox rows, got %d", len(rows))
	}
}

func BenchmarkLambdaColdShard_Query(b *testing.B) {
	// Build a Parquet file with 10K rows.
	ds, err := NewDuckDBShard("bench", "")
	if err != nil {
		b.Fatal(err)
	}
	rows := make([]*pb.LogRow, 10_000)
	for i := range rows {
		rows[i] = &pb.LogRow{
			LineNum: int64(i), Raw: "bench log line", App: "vimgym",
			Level: "INFO", Customer: "kevlar",
			TsUnixNs: coldT0.Add(time.Duration(i) * time.Millisecond).UnixNano(),
		}
	}
	ds.Ingest(rows)
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

	shard := NewLambdaColdShard("bench", []string{"vimgym"}, srv.URL, path)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		shard.Query("SELECT line_num, raw FROM logs WHERE level = 'ERROR' AND customer = 'kevlar'")
	}
}
