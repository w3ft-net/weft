package shard

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

const testEventID = "01900000-0000-7000-8000-000000000001"

// TestIngest_EventLineColumnsRoundTrip: split rows land with event_id and
// line_no; a one-line event lands with both NULL, not zero values.
func TestIngest_EventLineColumnsRoundTrip(t *testing.T) {
	ds, err := NewDuckDBShard("lines", "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer ds.Close()

	rows := []*pb.LogRow{
		{Raw: "Traceback:", CustomerId: 1, TsUnixNs: 1000, EventId: testEventID, LineNo: 0},
		{Raw: "ValueError", CustomerId: 1, TsUnixNs: 1000, EventId: testEventID, LineNo: 1},
		{Raw: "plain", CustomerId: 1, TsUnixNs: 2000},
	}
	if err := ds.Ingest(rows, nil); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	got, err := ds.db.Query(`SELECT raw, CAST(event_id AS TEXT), line_no FROM logs ORDER BY ts, line_no`)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()
	want := []struct {
		raw     string
		eventID sql.NullString
		lineNo  sql.NullInt32
	}{
		{"Traceback:", sql.NullString{String: testEventID, Valid: true}, sql.NullInt32{Int32: 0, Valid: true}},
		{"ValueError", sql.NullString{String: testEventID, Valid: true}, sql.NullInt32{Int32: 1, Valid: true}},
		{"plain", sql.NullString{}, sql.NullInt32{}},
	}
	for i, w := range want {
		if !got.Next() {
			t.Fatalf("row %d missing", i)
		}
		var raw string
		var id sql.NullString
		var n sql.NullInt32
		if err := got.Scan(&raw, &id, &n); err != nil {
			t.Fatal(err)
		}
		if raw != w.raw || id != w.eventID || n != w.lineNo {
			t.Fatalf("row %d = %q %v %v, want %q %v %v", i, raw, id, n, w.raw, w.eventID, w.lineNo)
		}
	}
}

func TestIngest_BadEventIDFails(t *testing.T) {
	ds, err := NewDuckDBShard("badid", "")
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	err = ds.Ingest([]*pb.LogRow{{Raw: "x", CustomerId: 1, EventId: "not-a-uuid"}}, nil)
	if err == nil {
		t.Fatal("want an error for an unparseable event_id")
	}
}

// TestNewDuckDBShard_MigratesEventLineColumns: a shard created before the
// columns existed gains them on open and ingests split rows.
func TestNewDuckDBShard_MigratesEventLineColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.duckdb")
	ds, err := NewDuckDBShard("old", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"event_id", "line_no"} {
		if _, err := ds.db.Exec(fmt.Sprintf(`ALTER TABLE logs DROP COLUMN %s`, c)); err != nil {
			t.Fatal(err)
		}
	}
	ds.Close()

	ds, err = NewDuckDBShard("old", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	if err := ds.Ingest([]*pb.LogRow{{Raw: "a", CustomerId: 1, EventId: testEventID, LineNo: 3}}, nil); err != nil {
		t.Fatalf("ingest after migration: %v", err)
	}
	var n int
	if err := ds.db.QueryRow(`SELECT line_no FROM logs WHERE event_id IS NOT NULL`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("line_no = %d, err %v", n, err)
	}
}
