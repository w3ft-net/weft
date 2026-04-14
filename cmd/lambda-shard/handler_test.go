package main

import (
	"bytes"
	"path/filepath"
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
		{LineNum: 1, Raw: "error in vimgym", App: "vimgym", Level: "ERROR", Customer: "kevlar", TsUnixNs: time.Now().UnixNano()},
		{LineNum: 2, Raw: "info in vimgym", App: "vimgym", Level: "INFO", Customer: "kevlar", TsUnixNs: time.Now().UnixNano()},
		{LineNum: 3, Raw: "warn in callbox", App: "callbox", Level: "WARN", Customer: "kevlar", TsUnixNs: time.Now().UnixNano()},
	}
	if err := ds.Ingest(rows); err != nil {
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
		SQL:         "SELECT * FROM logs WHERE customer = 'kevlar'",
		ParquetPath: path,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	rows, err := shard.ReadAllLogRows(&buf)
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
		SQL:         "SELECT * FROM logs WHERE app = 'vimgym' AND customer = 'kevlar'",
		ParquetPath: path,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	rows, err := shard.ReadAllLogRows(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 vimgym rows, got %d", len(rows))
	}
}

func TestLambdaHandler_EmptyResult(t *testing.T) {
	path := testParquet(t)
	var buf bytes.Buffer

	err := HandleQuery(&buf, QueryRequest{
		SQL:         "SELECT * FROM logs WHERE app = 'nonexistent'",
		ParquetPath: path,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	rows, err := shard.ReadAllLogRows(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected 0 rows, got %d", len(rows))
	}
}
