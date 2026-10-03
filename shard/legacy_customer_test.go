package shard

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// TestNewDuckDBShard_ConvertsLegacyCustomerTextColumn pins the cutover
// contract for a shard that reattaches a volume written before
// customer-bigint-pk: its logs table has `customer TEXT` and no
// `customer_id`. CREATE TABLE IF NOT EXISTS leaves that table alone, so
// without a migration step Ingest's Appender (which binds customer_id) and
// every planner query (customer_id = ?) fail on the reattached volume.
// Legacy rows keep their data: the two live tenants' names map to their
// fixed ids (graybeard=1, loom=2); any other name is unattributable and
// becomes NULL, which no tenant's customer_id predicate matches.
func TestNewDuckDBShard_ConvertsLegacyCustomerTextColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shard.duckdb")

	legacy, err := sql.Open("duckdb", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	// The pre-cutover table: today's shape except `customer TEXT` stands in
	// for `customer_id BIGINT`.
	legacyDDL := strings.Replace(createTableSQL, "customer_id    BIGINT", "customer       TEXT", 1)
	if legacyDDL == createTableSQL {
		t.Fatal("createTableSQL no longer has the customer_id line this test rewrites")
	}
	if _, err := legacy.Exec(legacyDDL); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if _, err := legacy.Exec(`INSERT INTO logs (raw, ts, namespace, service, kind, customer) VALUES
		('a', TIMESTAMP '2026-10-01 00:00:00', 'ns', 'svc', 'main', 'graybeard'),
		('b', TIMESTAMP '2026-10-01 00:00:01', 'ns', 'svc', 'main', 'loom'),
		('c', TIMESTAMP '2026-10-01 00:00:02', 'ns', 'svc', 'main', 'gbc')`); err != nil {
		t.Fatalf("seed legacy rows: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	ds, err := NewDuckDBShard("legacy", path)
	if err != nil {
		t.Fatalf("open reattached volume: %v", err)
	}
	defer ds.Close()

	newRow := &pb.LogRow{
		Raw: "new", Namespace: "ns", Service: "svc", Kind: "main",
		CustomerId: 2, TsUnixNs: time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC).UnixNano(),
	}
	if err := ds.Ingest([]*pb.LogRow{newRow}, nil); err != nil {
		t.Fatalf("ingest onto reattached legacy volume: %v", err)
	}

	rows, err := ds.db.Query(`SELECT raw, customer_id FROM logs ORDER BY raw`)
	if err != nil {
		t.Fatalf("query customer_id: %v", err)
	}
	defer rows.Close()
	want := map[string]sql.NullInt64{
		"a":   {Int64: 1, Valid: true},
		"b":   {Int64: 2, Valid: true},
		"c":   {},
		"new": {Int64: 2, Valid: true},
	}
	seen := 0
	for rows.Next() {
		var raw string
		var cid sql.NullInt64
		if err := rows.Scan(&raw, &cid); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen++
		if w, ok := want[raw]; !ok || cid != w {
			t.Errorf("row %q: customer_id = %+v, want %+v", raw, cid, want[raw])
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if seen != len(want) {
		t.Fatalf("want %d rows, got %d", len(want), seen)
	}
	rows.Close()

	// A second boot finds the converted table and must leave it alone.
	if err := ds.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	again, err := NewDuckDBShard("legacy", path)
	if err != nil {
		t.Fatalf("reopen converted volume: %v", err)
	}
	defer again.Close()
	if got := again.RowCount(); got != int64(len(want)) {
		t.Fatalf("RowCount after reopen: want %d, got %d", len(want), got)
	}
}
