package shard

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// TestDiskUsage_InMemoryIsZero asserts an in-memory shard reports no footprint
// rather than faulting on a nonexistent path — the blank/dev path must not
// poison a disk metric.
func TestDiskUsage_InMemoryIsZero(t *testing.T) {
	ds, err := NewDuckDBShard("mem", "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer ds.Close()
	if u := ds.DiskUsage(); u.TotalBytes() != 0 {
		t.Fatalf("in-memory DiskUsage: want 0, got %+v", u)
	}
}

// TestDiskUsage_CountsFileAndWAL asserts a file-backed shard reports a non-zero
// footprint after ingest. The data may sit in the WAL rather than the
// checkpointed .duckdb file, so the test asserts on TotalBytes (.duckdb + WAL +
// spill) — the figure the heartbeat and dashboards actually report — which is
// the whole point of counting the WAL.
func TestDiskUsage_CountsFileAndWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shard.duckdb")
	ds, err := NewDuckDBShard("disk", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer ds.Close()

	rows := make([]*pb.LogRow, 500)
	for i := range rows {
		rows[i] = &pb.LogRow{Raw: "a reasonably sized log line for footprint", CustomerId: 1, Namespace: "ns", Service: "svc", TsUnixNs: int64(i + 1)}
	}
	if err := ds.Ingest(context.Background(), rows, nil); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	u := ds.DiskUsage()
	if u.TotalBytes() <= 0 {
		t.Fatalf("post-ingest DiskUsage.TotalBytes: want > 0, got %+v", u)
	}
	// The main file should exist on disk and match the reported DuckDBBytes.
	if fi, serr := os.Stat(path); serr != nil {
		t.Fatalf("stat db: %v", serr)
	} else if fi.Size() != u.DuckDBBytes {
		t.Fatalf("DuckDBBytes %d != stat size %d", u.DuckDBBytes, fi.Size())
	}
}

// TestQueryStats_RecordsAndResets asserts the query-latency accumulator counts
// queries (and errors) executed through QueryContext, exposes a non-zero max,
// and zeroes on snapshot so each telemetry interval reports only its own window.
func TestQueryStats_RecordsAndResets(t *testing.T) {
	ds, err := NewDuckDBShard("qstats", "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer ds.Close()

	// ScanResultRows projects (raw, ts), so shape the queries to match.
	if _, err := ds.Query(context.Background(), "SELECT 'hello', now()"); err != nil {
		t.Fatalf("good query: %v", err)
	}
	if _, err := ds.Query(context.Background(), "SELECT raw, ts FROM no_such_table"); err == nil {
		t.Fatal("expected error from bad query")
	}

	s := ds.QueryStatsSnapshot()
	if s.Count != 2 {
		t.Fatalf("Count: want 2, got %d", s.Count)
	}
	if s.Errors != 1 {
		t.Fatalf("Errors: want 1, got %d", s.Errors)
	}
	if s.MaxMs < 0 || s.MeanMs < 0 {
		t.Fatalf("latency must be non-negative, got mean=%v max=%v", s.MeanMs, s.MaxMs)
	}

	// Snapshot must have reset the accumulator.
	if again := ds.QueryStatsSnapshot(); again.Count != 0 {
		t.Fatalf("snapshot did not reset: Count=%d", again.Count)
	}
}

// TestDirSize_MissingRootIsZero guards the common case: no query has spilled, so
// <db>.tmp doesn't exist — dirSize must return 0, not error.
func TestDirSize_MissingRootIsZero(t *testing.T) {
	if got := dirSize(filepath.Join(t.TempDir(), "does-not-exist.tmp")); got != 0 {
		t.Fatalf("dirSize(missing): want 0, got %d", got)
	}
}
