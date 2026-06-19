package shard

import (
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
		Fields:      []string{"a", "b,c", "d"},
		TsUnixNs:    tsUs,
		Level:       "INFO",
		Namespace:   "test",
		Service:     "shard",
		Kind:        "", // expect defaulted to "main"
		Customer:    "kevlar",
		PayloadJson: []byte(`{"user":"alice","n":42}`),
		Region:      "us-east-2",
	}
	if err := ds.Ingest([]*pb.LogRow{row}, nil); err != nil {
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

	// fields TEXT[] — list length and 1-indexed elements (including the one with
	// an embedded comma, which must remain a single element).
	var flen int
	var f1, f2 string
	if err := ds.db.QueryRow(`SELECT len(fields), fields[1], fields[2] FROM logs`).Scan(&flen, &f1, &f2); err != nil {
		t.Fatalf("scan fields: %v", err)
	}
	if flen != 3 || f1 != "a" || f2 != "b,c" {
		t.Fatalf("fields round-trip: want 3/a/b,c, got %d/%q/%q", flen, f1, f2)
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

// TestIngest_EmptyPayloadDefaults asserts a row with no PayloadJson lands as an
// empty JSON object (parity with the old INSERT path's "{}" default), so
// downstream JSON extracts don't fault on NULL.
func TestIngest_EmptyPayloadDefaults(t *testing.T) {
	ds, err := NewDuckDBShard("empty-payload", "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer ds.Close()

	if err := ds.Ingest([]*pb.LogRow{{Raw: "x", Customer: "kevlar", TsUnixNs: 1}}, nil); err != nil {
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
		Raw:       "ordered",
		Level:     "WARN",
		Namespace: "ns",
		Service:   "svc",
		Customer:  "kevlar",
		Region:    "us-east-2",
		TsUnixNs:  1,
	}
	if err := ds.Ingest([]*pb.LogRow{row}, nil); err != nil {
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
