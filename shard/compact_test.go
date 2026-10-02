package shard

import (
	"context"
	"os"
	"testing"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// archiveFixture ingests rows then archives to a local temp dir, returning
// the parquet path. Cutoffs in the rewrite tests are built with time.Unix
// (matching how Ingest stores ts), so the ts/cutoff comparison is in the
// same frame regardless of the test machine's timezone.
func archiveFixture(t *testing.T, rows []*pb.LogRow) (string, *DuckDBShard) {
	t.Helper()
	s, err := NewDuckDBShard("compact-test", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(rows, nil); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	path, _, err := s.Archive(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	return path, s
}

// survivorsByCustomer reads a rewritten parquet back through a ColdShard
// and counts surviving rows per customer.
func survivorsByCustomer(t *testing.T, path string, customer int64) int {
	t.Helper()
	cold, err := NewColdShard("verify", nil, path, "", nil)
	if err != nil {
		t.Fatalf("cold shard: %v", err)
	}
	defer cold.Close()
	rows, err := cold.Query("SELECT raw, ts FROM logs WHERE customer_id = ?", customer)
	if err != nil {
		t.Fatalf("verify query (%d): %v", customer, err)
	}
	return len(rows)
}

func TestRewriteRetention_DropsExpiredPreservesUnmapped(t *testing.T) {
	base := int64(1_600_000_000) * int64(time.Second)
	old := base
	recent := base + 100*int64(24*time.Hour)
	rows := []*pb.LogRow{
		{Raw: "acme-old", CustomerId: 5, TsUnixNs: old},
		{Raw: "acme-new", CustomerId: 5, TsUnixNs: recent},
		{Raw: "globex-old", CustomerId: 6, TsUnixNs: old},
		{Raw: "ghost-old", CustomerId: 14, TsUnixNs: old}, // no cutoff → preserved
	}
	src, live := archiveFixture(t, rows)
	defer live.Close()

	// Cutoff at base+50d: drops acme-old and globex-old, keeps acme-new.
	// "ghost" has no cutoff, so all its rows survive.
	cutoff := time.Unix(0, base+50*int64(24*time.Hour))
	dst := t.TempDir() + "/rewritten.parquet"
	res, err := RewriteRetention(src, dst, []CustomerCutoff{
		{CustomerID: 5, Cutoff: cutoff},
		{CustomerID: 6, Cutoff: cutoff},
	}, nil)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	if res.RowsKept != 2 || res.RowsDeleted != 2 {
		t.Fatalf("counts: kept=%d deleted=%d, want 2/2", res.RowsKept, res.RowsDeleted)
	}
	if res.OutputPath != dst {
		t.Fatalf("OutputPath = %q, want %q", res.OutputPath, dst)
	}
	if n := survivorsByCustomer(t, dst, 5); n != 1 {
		t.Errorf("acme survivors = %d, want 1 (only acme-new)", n)
	}
	if n := survivorsByCustomer(t, dst, 6); n != 0 {
		t.Errorf("globex survivors = %d, want 0 (fully expired)", n)
	}
	if n := survivorsByCustomer(t, dst, 14); n != 1 {
		t.Errorf("ghost survivors = %d, want 1 (unmapped, preserved)", n)
	}
}

func TestRewriteRetention_EmptyResultWritesNothing(t *testing.T) {
	base := int64(1_600_000_000) * int64(time.Second)
	rows := []*pb.LogRow{
		{Raw: "globex-old", CustomerId: 6, TsUnixNs: base},
		{Raw: "globex-old2", CustomerId: 6, TsUnixNs: base + int64(time.Hour)},
	}
	src, live := archiveFixture(t, rows)
	defer live.Close()

	cutoff := time.Unix(0, base+50*int64(24*time.Hour)) // after every row
	dst := t.TempDir() + "/empty.parquet"
	res, err := RewriteRetention(src, dst, []CustomerCutoff{{CustomerID: 6, Cutoff: cutoff}}, nil)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	if res.RowsKept != 0 || res.RowsDeleted != 2 {
		t.Fatalf("counts: kept=%d deleted=%d, want 0/2", res.RowsKept, res.RowsDeleted)
	}
	if res.OutputPath != "" {
		t.Errorf("OutputPath = %q, want empty (nothing written)", res.OutputPath)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("dst should not exist on empty result, stat err = %v", err)
	}
}
