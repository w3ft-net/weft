package shard

import (
	"context"
	"testing"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// TestDuckDBShard_ArchiveCapturesCustomerStats proves Archive returns
// per-customer row counts and ts spans alongside the parquet path. These
// feed shard_archive_stats, which the retention janitor consults to
// classify archived files without re-scanning S3 (docs/plans/retention-janitor.md).
//
// Timestamp assertions use spans (MaxTs−MinTs), not absolute instants, to
// stay independent of how the DuckDB driver localizes the tz-naive
// TIMESTAMP column on read-back. Row counts are exact.
func TestDuckDBShard_ArchiveCapturesCustomerStats(t *testing.T) {
	s, err := NewDuckDBShard("stats-test", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Whole-second timestamps round-trip cleanly through DuckDB's
	// microsecond TIMESTAMP. acme spans 2h across 3 rows; globex has 2
	// rows at one instant (zero span).
	base := int64(1_700_000_000) * int64(time.Second)
	rows := []*pb.LogRow{
		{Raw: "a1", Customer: "acme", TsUnixNs: base},
		{Raw: "a2", Customer: "acme", TsUnixNs: base + int64(time.Hour)},
		{Raw: "a3", Customer: "acme", TsUnixNs: base + 2*int64(time.Hour)},
		{Raw: "g1", Customer: "globex", TsUnixNs: base + int64(30*time.Minute)},
		{Raw: "g2", Customer: "globex", TsUnixNs: base + int64(30*time.Minute)},
	}
	if err := s.Ingest(rows, nil); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	path, stats, err := s.Archive(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if path == "" {
		t.Fatal("archive returned empty path")
	}

	byCustomer := make(map[string]CustomerStat, len(stats))
	for _, st := range stats {
		byCustomer[st.Customer] = st
	}
	if len(byCustomer) != 2 {
		t.Fatalf("stats: got %d customers, want 2: %+v", len(byCustomer), stats)
	}

	acme := byCustomer["acme"]
	if acme.RowCount != 3 {
		t.Errorf("acme RowCount = %d, want 3", acme.RowCount)
	}
	if span := acme.MaxTs.Sub(acme.MinTs); span != 2*time.Hour {
		t.Errorf("acme span = %v, want 2h", span)
	}

	globex := byCustomer["globex"]
	if globex.RowCount != 2 {
		t.Errorf("globex RowCount = %d, want 2", globex.RowCount)
	}
	if !globex.MaxTs.Equal(globex.MinTs) {
		t.Errorf("globex span: min %v != max %v, want zero span", globex.MinTs, globex.MaxTs)
	}
}
