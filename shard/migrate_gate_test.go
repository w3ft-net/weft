package shard

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// A shard whose schema already has every column the code writes needs no
// migration, and a spare opens its database inside an upgrade's hold window:
// the ~20 statements cost 0.1s to 1.4s there for nothing.
func TestNewDuckDBShard_ACurrentSchemaSkipsTheMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shard.db")
	first, err := NewDuckDBShard("s-1", path)
	if err != nil {
		t.Fatal(err)
	}
	first.Close()

	ds, err := NewDuckDBShard("s-1", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })

	if got := ds.OpenTiming(); !strings.Contains(got, "migrate=skipped") {
		t.Errorf("OpenTiming() = %q, want the migrations skipped on a current schema", got)
	}
}

// An older shard still converges: the gate only skips when nothing is missing.
func TestNewDuckDBShard_AnOlderSchemaIsStillMigrated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shard.db")
	db, err := sql.Open("duckdb", path)
	if err != nil {
		t.Fatal(err)
	}
	// The shape from before the dimension columns: host_group, and none of the
	// namespace, trace or line columns the migrations add.
	if _, err := db.Exec(`CREATE TABLE logs (raw TEXT, fields TEXT[], ts TIMESTAMP, level TEXT, instance TEXT, customer_id BIGINT, payload JSON, host_group TEXT)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	ds, err := NewDuckDBShard("s-1", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })

	if got := ds.OpenTiming(); strings.Contains(got, "migrate=skipped") {
		t.Errorf("OpenTiming() = %q, want an older schema migrated", got)
	}
	have := map[string]bool{}
	rows, err := ds.db.Query(`SELECT name FROM pragma_table_info('logs')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		rows.Scan(&n)
		have[n] = true
	}
	for _, c := range logColumns {
		if !have[c] {
			t.Errorf("column %q missing after migrating an older shard", c)
		}
	}
	if have["host_group"] {
		t.Error("host_group still present after migrating")
	}
}
