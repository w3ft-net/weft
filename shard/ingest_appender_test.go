package shard

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// TestIngest_AppenderRoundTrip exercises the columns the Appender rewrite is
// most likely to get wrong — the JSON payload (must store the object, not a
// double-encoded string), the TEXT[] fields list, the TIMESTAMP, the kind
// default, and raw text with characters a string-built INSERT would have had to
// escape — and asserts each survives the write→read round trip intact.
func TestIngest_AppenderRoundTrip(t *testing.T) {
	ds, err := NewDuckDBShard("rt", "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer ds.Close()

	// Whole-microsecond timestamp: DuckDB TIMESTAMP is microsecond precision, so
	// pick a value with no sub-microsecond tail to keep the assertion exact.
	tsUs := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC).UnixNano() + 123_000 // .000123s
	rawWithSpecials := "line with 'quotes', \"double\", a comma, and a newline\n🚀"

	row := &pb.LogRow{
		Raw:         rawWithSpecials,
		TsUnixNs:    tsUs,
		Level:       "INFO",
		Namespace:   "test",
		Service:     "shard",
		Kind:        "", // expect defaulted to "main"
		CustomerId:  1,
		PayloadJson: []byte(`{"user":"alice","n":42}`),
		Region:      "us-east-2",
	}
	if err := ds.Ingest(context.Background(), []*pb.LogRow{row}, nil); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if got := ds.RowCount(); got != 1 {
		t.Fatalf("RowCount: want 1, got %d", got)
	}

	// payload must be a queryable JSON object. If it were double-encoded into a
	// quoted JSON string, these extracts would come back NULL/empty.
	var user string
	var n int
	if err := ds.db.QueryRow(`SELECT payload->>'user', CAST(payload->>'n' AS INTEGER) FROM logs`).Scan(&user, &n); err != nil {
		t.Fatalf("scan payload: %v", err)
	}
	if user != "alice" || n != 42 {
		t.Fatalf("payload round-trip: want alice/42, got %q/%d", user, n)
	}

	// fields TEXT[] is raw split the way awk splits $0 — list length and
	// 1-indexed elements, including ones with an embedded quote or comma.
	var flen int
	var f3, f5 string
	if err := ds.db.QueryRow(`SELECT len(fields), fields[3], fields[5] FROM logs`).Scan(&flen, &f3, &f5); err != nil {
		t.Fatalf("scan fields: %v", err)
	}
	if flen != 10 || f3 != "'quotes'," || f5 != "a" {
		t.Fatalf("fields round-trip: want 10/'quotes',/a, got %d/%q/%q", flen, f3, f5)
	}

	// raw verbatim, kind defaulted, ts at microsecond granularity.
	var raw, kind string
	var epochUs int64
	if err := ds.db.QueryRow(`SELECT raw, kind, epoch_us(ts) FROM logs`).Scan(&raw, &kind, &epochUs); err != nil {
		t.Fatalf("scan raw/kind/ts: %v", err)
	}
	if raw != rawWithSpecials {
		t.Fatalf("raw round-trip mismatch:\n want %q\n  got %q", rawWithSpecials, raw)
	}
	if kind != "main" {
		t.Fatalf("kind default: want main, got %q", kind)
	}
	if wantUs := tsUs / 1000; epochUs != wantUs {
		t.Fatalf("ts round-trip: want %d us, got %d us", wantUs, epochUs)
	}
}

// TestIngest_TraceIDRoundTrip exercises all three trace_id write points
// together (createTableSQL, migrationSteps, logColumns/AppendRow) — a row
// carrying TraceId must be both queryable by exact match and absent (NULL,
// not erroring) when unset.
func TestIngest_TraceIDRoundTrip(t *testing.T) {
	ds, err := NewDuckDBShard("trace", "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer ds.Close()

	rows := []*pb.LogRow{
		{Raw: "with trace", CustomerId: 1, TsUnixNs: 1, TraceId: "trace-abc-123"},
		{Raw: "without trace", CustomerId: 1, TsUnixNs: 2},
	}
	if err := ds.Ingest(context.Background(), rows, nil); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	var raw string
	if err := ds.db.QueryRow(`SELECT raw FROM logs WHERE trace_id = ?`, "trace-abc-123").Scan(&raw); err != nil {
		t.Fatalf("query by trace_id: %v", err)
	}
	if raw != "with trace" {
		t.Fatalf("trace_id lookup: want %q, got %q", "with trace", raw)
	}

	var untraced string
	if err := ds.db.QueryRow(`SELECT trace_id FROM logs WHERE raw = 'without trace'`).Scan(&untraced); err != nil {
		t.Fatalf("query untraced row: %v", err)
	}
	if untraced != "" {
		t.Fatalf("untraced row: want empty trace_id, got %q", untraced)
	}
}

// TestIngest_SpanColumnsRoundTrip exercises the same three write points
// (createTableSQL, migrationSteps, logColumns/AppendRow) for span_name/
// span_id/parent_span_id/duration_ns, mirroring TestIngest_TraceIDRoundTrip
// — a row carrying span fields must be queryable by exact match, and a row
// without them must come back NULL/zero, not error.
func TestIngest_SpanColumnsRoundTrip(t *testing.T) {
	ds, err := NewDuckDBShard("span", "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer ds.Close()

	rows := []*pb.LogRow{
		{
			Raw: "with span", CustomerId: 1, TsUnixNs: 1, TraceId: "trace-abc-123",
			SpanName: "shard.Query", SpanId: "s2", ParentSpanId: "s1", DurationNs: 1234000,
		},
		{Raw: "without span", CustomerId: 1, TsUnixNs: 2},
	}
	if err := ds.Ingest(context.Background(), rows, nil); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	var raw string
	if err := ds.db.QueryRow(`SELECT raw FROM logs WHERE span_name = ? AND span_id = ?`, "shard.Query", "s2").Scan(&raw); err != nil {
		t.Fatalf("query by span_name/span_id: %v", err)
	}
	if raw != "with span" {
		t.Fatalf("span lookup: want %q, got %q", "with span", raw)
	}

	var parentID string
	var durationNs int64
	if err := ds.db.QueryRow(`SELECT parent_span_id, duration_ns FROM logs WHERE raw = 'with span'`).Scan(&parentID, &durationNs); err != nil {
		t.Fatalf("query span columns: %v", err)
	}
	if parentID != "s1" || durationNs != 1234000 {
		t.Fatalf("span columns: want parent_span_id=s1 duration_ns=1234000, got parent_span_id=%q duration_ns=%d", parentID, durationNs)
	}

	// Unset int64/string fields append as Go's zero value (0, ""), not
	// SQL NULL — the Appender writes every column's literal Go value,
	// same as trace_id's own "" (not NULL) in TestIngest_TraceIDRoundTrip.
	var spanName string
	var durationOfUnset int64
	if err := ds.db.QueryRow(`SELECT span_name, duration_ns FROM logs WHERE raw = 'without span'`).Scan(&spanName, &durationOfUnset); err != nil {
		t.Fatalf("query unset span row: %v", err)
	}
	if spanName != "" {
		t.Fatalf("unset row: want empty span_name, got %q", spanName)
	}
	if durationOfUnset != 0 {
		t.Fatalf("unset row: want duration_ns=0, got %d", durationOfUnset)
	}
}

// TestIngest_EmptyPayloadDefaults asserts a row with no PayloadJson lands as an
// empty JSON object (parity with the old INSERT path's "{}" default), so
// downstream JSON extracts don't fault on NULL.
func TestIngest_EmptyPayloadDefaults(t *testing.T) {
	ds, err := NewDuckDBShard("empty-payload", "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer ds.Close()

	if err := ds.Ingest(context.Background(), []*pb.LogRow{{Raw: "x", CustomerId: 1, TsUnixNs: 1}}, nil); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	var typ string
	if err := ds.db.QueryRow(`SELECT json_type(payload) FROM logs`).Scan(&typ); err != nil {
		t.Fatalf("scan json_type: %v", err)
	}
	if typ != "OBJECT" {
		t.Fatalf("empty payload: want OBJECT, got %q", typ)
	}
}

// TestIngest_ColumnOrderIndependent proves the Appender binds by column NAME,
// not physical position. A migrated shard's on-disk column order diverges from
// createTableSQL once ALTER TABLE has run; here we force that divergence by
// dropping and re-adding a column (which moves it to the end physically), then
// ingest and confirm each value still lands in its named column. A
// position-based appender would silently shift every value one slot.
func TestIngest_ColumnOrderIndependent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reorder.duckdb")
	ds, err := NewDuckDBShard("reorder", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer ds.Close()

	// Move `level` to the last physical position, diverging from logColumns order.
	if _, err := ds.db.Exec(`ALTER TABLE logs DROP COLUMN level`); err != nil {
		t.Fatalf("drop level: %v", err)
	}
	if _, err := ds.db.Exec(`ALTER TABLE logs ADD COLUMN level TEXT`); err != nil {
		t.Fatalf("re-add level: %v", err)
	}

	row := &pb.LogRow{
		Raw:        "ordered",
		Level:      "WARN",
		Namespace:  "ns",
		Service:    "svc",
		CustomerId: 1,
		Region:     "us-east-2",
		TsUnixNs:   1,
	}
	if err := ds.Ingest(context.Background(), []*pb.LogRow{row}, nil); err != nil {
		t.Fatalf("ingest after reorder: %v", err)
	}

	var level, raw, region string
	if err := ds.db.QueryRow(`SELECT level, raw, region FROM logs`).Scan(&level, &raw, &region); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if level != "WARN" || raw != "ordered" || region != "us-east-2" {
		t.Fatalf("column-order independence: want WARN/ordered/us-east-2, got %q/%q/%q", level, raw, region)
	}
}
