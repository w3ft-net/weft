package shard

import (
	"context"
	"testing"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// TestIngestTsRoundTripIsUTC pins that a log row's timestamp survives a
// store→read round trip as the same absolute instant regardless of the
// process timezone. The logs.ts column is a tz-naive DuckDB TIMESTAMP;
// the query predicate path already formats bounds with explicit .UTC()
// (planner formatTS), so ingest must store UTC wall-clock too or queries
// drift by the local offset on any non-UTC host. Forcing a non-UTC
// time.Local surfaces the skew that a UTC CI/prod box hides.
func TestIngestTsRoundTripIsUTC(t *testing.T) {
	orig := time.Local
	time.Local = time.FixedZone("TEST-0500", -5*60*60)
	t.Cleanup(func() { time.Local = orig })

	s, err := NewDuckDBShard("tz-test", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	inst := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	ns := inst.UnixNano()
	if err := s.Ingest(context.Background(), []*pb.LogRow{{Raw: "x", CustomerId: 10, TsUnixNs: ns}}, nil); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	rows, err := s.Query(context.Background(), "SELECT raw, ts FROM logs WHERE customer_id = 10")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].TsUnixNs != ns {
		t.Fatalf("ts round-trip = %d (%v), want %d (%v) — off by %v",
			rows[0].TsUnixNs, time.Unix(0, rows[0].TsUnixNs).UTC(),
			ns, inst, time.Duration(rows[0].TsUnixNs-ns))
	}
}

// utcBound mirrors planner.formatTS — the exact wall-clock string the
// query planner binds for a ts predicate. Replicated here (the planner
// helper is unexported) so this test guards the storage-vs-predicate
// frame the planner depends on.
func utcBound(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05.999999") }

// TestTsPredicateMatchesUTCBounds is the guard that actually bites if
// storage ever drifts from the planner's UTC frame: a row stored at
// instant I must be selected by UTC-formatted bounds straddling I, even
// under a non-UTC process timezone. (The round-trip test above can't
// catch this — it carries no ts predicate.)
func TestTsPredicateMatchesUTCBounds(t *testing.T) {
	orig := time.Local
	time.Local = time.FixedZone("TEST+0900", 9*60*60)
	t.Cleanup(func() { time.Local = orig })

	s, err := NewDuckDBShard("tz-pred", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	inst := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	if err := s.Ingest(context.Background(), []*pb.LogRow{{Raw: "x", CustomerId: 10, TsUnixNs: inst.UnixNano()}}, nil); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	rows, err := s.Query(context.Background(),
		"SELECT raw, ts FROM logs WHERE ts >= CAST(? AS TIMESTAMP) AND ts <= CAST(? AS TIMESTAMP)",
		utcBound(inst.Add(-time.Hour)), utcBound(inst.Add(time.Hour)))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("row at %v not matched by UTC bounds [11:00,13:00]: got %d rows (storage drifted from the planner's UTC frame)", inst, len(rows))
	}
}
