package shard

import (
	"testing"
)

// ScanResultRows must carry any column beyond the first two (raw, ts)
// into ResultRow.MetaVars, keyed by its column name — the mechanism
// the planner relies on to fix "jq's reserved meta-variables ...
// resolve to null when read as output" (docs/known-issues.md): a bare
// "$host"-shaped read only works if a row's MetaVars actually carries
// the "instance" column value for compute (compute/metavars.go) to
// splice into the row's JSON body.
func TestScanResultRows_CarriesExtraColumnsAsMetaVars(t *testing.T) {
	ds, err := NewDuckDBShard("scan-metavars-test", "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer ds.Close()

	if _, err := ds.db.Exec(
		`INSERT INTO logs (raw, ts, namespace, instance) VALUES (?, ?, ?, ?)`,
		`{"pct":42}`, "2026-01-01 00:00:00", "myco", "web-1",
	); err != nil {
		t.Fatalf("insert: %v", err)
	}

	rows, err := ds.Query(`SELECT raw, ts, namespace, instance FROM logs`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	row := rows[0]
	if row.Raw != `{"pct":42}` {
		t.Errorf("Raw = %q", row.Raw)
	}
	if row.MetaVars["namespace"] != "myco" {
		t.Errorf("MetaVars[namespace] = %q, want %q", row.MetaVars["namespace"], "myco")
	}
	if row.MetaVars["instance"] != "web-1" {
		t.Errorf("MetaVars[instance] = %q, want %q", row.MetaVars["instance"], "web-1")
	}
	if len(row.MetaVars) != 2 {
		t.Errorf("MetaVars = %v, want exactly 2 entries", row.MetaVars)
	}
}

// The common (raw, ts)-only query shape must not allocate a MetaVars
// map at all — the overwhelming majority of queries never ask for
// meta-var columns, and a nil map serializes to nothing extra over the
// wire.
func TestScanResultRows_NoExtraColumnsLeavesMetaVarsNil(t *testing.T) {
	ds, err := NewDuckDBShard("scan-no-metavars-test", "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer ds.Close()

	if _, err := ds.db.Exec(
		`INSERT INTO logs (raw, ts) VALUES (?, ?)`,
		`{"pct":42}`, "2026-01-01 00:00:00",
	); err != nil {
		t.Fatalf("insert: %v", err)
	}

	rows, err := ds.Query(`SELECT raw, ts FROM logs`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].MetaVars != nil {
		t.Errorf("MetaVars = %v, want nil", rows[0].MetaVars)
	}
}
