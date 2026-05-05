package shard

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

var (
	coldT0 = time.Date(2026, 3, 28, 10, 0, 0, 0, time.UTC)
	coldT1 = time.Date(2026, 3, 28, 11, 0, 0, 0, time.UTC)
)

// exportToParquet creates a DuckDB shard, ingests rows, exports to Parquet,
// and returns the Parquet path. The DuckDB is closed after export.
func exportToParquet(t *testing.T, rows []*pb.LogRow) string {
	t.Helper()
	ds, err := NewDuckDBShard("export", "")
	if err != nil {
		t.Fatalf("create shard: %v", err)
	}
	defer ds.Close()

	if err := ds.Ingest(rows); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	path := filepath.Join(t.TempDir(), "test.parquet")
	if err := ds.ExportToParquet(path); err != nil {
		t.Fatalf("export: %v", err)
	}
	return path
}

func testRows() []*pb.LogRow {
	return []*pb.LogRow{
		{LineNum: 1, Raw: "error in vimgym", App: "vimgym", Level: "ERROR", Customer: "kevlar", Instance: "i-001", TsUnixNs: coldT0.Add(1 * time.Second).UnixNano()},
		{LineNum: 2, Raw: "info in vimgym", App: "vimgym", Level: "INFO", Customer: "kevlar", Instance: "i-001", TsUnixNs: coldT0.Add(2 * time.Second).UnixNano()},
		{LineNum: 3, Raw: "error in callbox", App: "callbox", Level: "ERROR", Customer: "kevlar", Instance: "i-002", TsUnixNs: coldT0.Add(3 * time.Second).UnixNano()},
		{LineNum: 4, Raw: "info in callbox", App: "callbox", Level: "INFO", Customer: "kevlar", Instance: "i-002", TsUnixNs: coldT0.Add(4 * time.Second).UnixNano()},
		{LineNum: 5, Raw: "other customer", App: "vimgym", Level: "INFO", Customer: "acme", Instance: "i-999", TsUnixNs: coldT0.Add(5 * time.Second).UnixNano()},
	}
}

func TestColdShard_QueryParquet(t *testing.T) {
	path := exportToParquet(t, testRows())

	cs, err := NewColdShard("cold-0", []string{"vimgym", "callbox"}, path, "", nil)
	if err != nil {
		t.Fatalf("create cold shard: %v", err)
	}
	defer cs.Close()

	rows, err := cs.Query("SELECT line_num, raw FROM logs WHERE level = 'ERROR' AND customer = 'kevlar'")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 ERROR rows, got %d", len(rows))
	}
	for _, r := range rows {
		if !strings.Contains(strings.ToLower(r.Raw), "error") {
			t.Errorf("expected raw to contain 'error', got %q", r.Raw)
		}
	}
}

func TestColdShard_FiltersByApp(t *testing.T) {
	path := exportToParquet(t, testRows())

	cs, err := NewColdShard("cold-0", []string{"vimgym", "callbox"}, path, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	rows, err := cs.Query("SELECT line_num, raw FROM logs WHERE app = 'vimgym' AND customer = 'kevlar'")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 vimgym/kevlar rows, got %d", len(rows))
	}
	for _, r := range rows {
		if !strings.Contains(r.Raw, "vimgym") {
			t.Errorf("expected raw to contain 'vimgym', got %q", r.Raw)
		}
	}
}

func TestColdShard_ReadOnly(t *testing.T) {
	path := exportToParquet(t, testRows())

	cs, err := NewColdShard("cold-0", []string{"vimgym"}, path, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	err = cs.Ingest([]*pb.LogRow{{Raw: "should fail"}})
	if err == nil {
		t.Fatal("expected error on Ingest to cold shard")
	}
}

func TestColdShard_TimeWindow(t *testing.T) {
	path := exportToParquet(t, testRows())

	cs, err := NewColdShard("cold-test-id", []string{"vimgym"}, path, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	if cs.ID() != "cold-test-id" {
		t.Errorf("expected cold-test-id, got %s", cs.ID())
	}
}

func TestColdShard_QueryToSink(t *testing.T) {
	path := exportToParquet(t, testRows())

	cs, err := NewColdShard("cold-0", []string{"vimgym", "callbox"}, path, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	sink := &testSink{}
	cs.QueryToSink("SELECT line_num, raw FROM logs WHERE customer = 'kevlar'", sink)

	if !sink.sentinelReceived {
		t.Error("expected sentinel")
	}
	if len(sink.rows) != 4 {
		t.Fatalf("expected 4 kevlar rows via sink, got %d", len(sink.rows))
	}
}

func TestColdShard_EmptyResult(t *testing.T) {
	path := exportToParquet(t, testRows())

	cs, err := NewColdShard("cold-0", []string{"vimgym"}, path, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	rows, err := cs.Query("SELECT line_num, raw FROM logs WHERE app = 'nonexistent'")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected 0 rows, got %d", len(rows))
	}
}

// testSink implements RowSink for testing QueryToSink.
type testSink struct {
	rows             []*pb.ResultRow
	sentinelReceived bool
}

func (s *testSink) SendRow(row *pb.ResultRow, shardID string) {
	s.rows = append(s.rows, row)
}
func (s *testSink) SendSentinel(shardID string) {
	s.sentinelReceived = true
}

// Benchmarks

func BenchmarkColdShard_Query_1K(b *testing.B)   { benchColdQuery(b, 1_000) }
func BenchmarkColdShard_Query_10K(b *testing.B)  { benchColdQuery(b, 10_000) }
func BenchmarkColdShard_Query_100K(b *testing.B) { benchColdQuery(b, 100_000) }

func benchColdQuery(b *testing.B, n int) {
	rows := make([]*pb.LogRow, n)
	base := coldT0
	for i := range rows {
		rows[i] = &pb.LogRow{
			LineNum:  int64(i),
			Raw:      "bench log line with some content for realistic sizing",
			App:      "vimgym",
			Level:    "INFO",
			Customer: "kevlar",
			Instance: "i-001",
			TsUnixNs: base.Add(time.Duration(i) * time.Millisecond).UnixNano(),
		}
	}

	ds, err := NewDuckDBShard("bench", "")
	if err != nil {
		b.Fatal(err)
	}
	if err := ds.Ingest(rows); err != nil {
		b.Fatal(err)
	}

	path := filepath.Join(b.TempDir(), "bench.parquet")
	if err := ds.ExportToParquet(path); err != nil {
		b.Fatal(err)
	}
	ds.Close()

	cs, err := NewColdShard("bench-cold", []string{"vimgym"}, path, "", nil)
	if err != nil {
		b.Fatal(err)
	}
	defer cs.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cs.Query("SELECT line_num, raw FROM logs WHERE level = 'ERROR' AND customer = 'kevlar'")
	}
}
