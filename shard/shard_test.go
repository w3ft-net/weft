package shard

import (
	"path/filepath"
	"testing"
	"time"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// TestNewDuckDBShard_SeedsRowCountFromExistingTable asserts the recovery
// path's data-preservation contract: when a shard process boots against a
// reattached EBS volume that already has rows on it, RowCount() must
// reflect the on-disk count rather than starting at zero. This is the
// invariant Bug 3 violated — the in-memory counter was only incremented
// by Ingest, so a recovered shard reported empty.
func TestNewDuckDBShard_SeedsRowCountFromExistingTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shard.duckdb")

	// First boot: ingest a fixed number of rows and close.
	first, err := NewDuckDBShard("seed-test", path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	rows := make([]*pb.LogRow, 100)
	base := time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC).UnixNano()
	for i := range rows {
		rows[i] = &pb.LogRow{
			Raw:       "seed",
			Namespace: "test",
			Service:   "shard",
			Kind:      "main",
			Customer:  "kevlar",
			TsUnixNs:  base + int64(i),
		}
	}
	if err := first.Ingest(rows, nil); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if got := first.RowCount(); got != 100 {
		t.Fatalf("post-ingest RowCount: want 100, got %d", got)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Second boot: reopen the same path (simulating a fresh instance with
	// the volume reattached). RowCount must reflect the rows already on
	// disk, before any new ingest.
	second, err := NewDuckDBShard("seed-test", path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()
	if got := second.RowCount(); got != 100 {
		t.Fatalf("RowCount on reopen: want 100 (preserved from disk), got %d", got)
	}
}

// TestNewDuckDBShard_FreshShardStartsAtZero asserts that a brand-new
// shard (no existing logs table contents) still reports RowCount==0 at
// boot — the seed query handles both the fresh and recovery cases.
func TestNewDuckDBShard_FreshShardStartsAtZero(t *testing.T) {
	ds, err := NewDuckDBShard("fresh", "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer ds.Close()
	if got := ds.RowCount(); got != 0 {
		t.Errorf("RowCount on fresh shard: want 0, got %d", got)
	}
}
