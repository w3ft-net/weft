package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/duckdb/duckdb-go/v2"
)

// rowsScanned runs q on one thread and returns DuckDB's own account of the
// rows its scans read. One thread because the counter is summed per busy
// thread: with the default thread count the same query reports a multiple of
// its rows (8x on an 8-core laptop). The handler divides that back out
// (coldQuery.Stats); this test pins the single-thread truth it relies on.
func rowsScanned(t *testing.T, dir, q string) int64 {
	t.Helper()
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	out := filepath.Join(dir, "profile.json")
	os.Remove(out)
	for _, s := range []string{"SET threads=1", "PRAGMA enable_profiling='json'", "PRAGMA profiling_output='" + out + "'"} {
		if _, err := conn.ExecContext(context.Background(), s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	rows, err := conn.QueryContext(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
	}
	rows.Close()

	b, _ := os.ReadFile(out)
	var p struct {
		Scanned int64 `json:"cumulative_rows_scanned"`
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &p); err != nil {
			t.Fatalf("profile: %v\n%s", err, b)
		}
	}
	return p.Scanned
}

// What a rows-read meter can and can't see. These pin DuckDB's behavior, so a
// version bump that changes any of it fails here instead of skewing the meter.
func TestRowsScannedAsAMeter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.parquet")
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("COPY (SELECT i AS ts, 'line ' || i AS raw FROM range(1000000) t(i)) TO '" + path + "' (FORMAT parquet, ROW_GROUP_SIZE 10000)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	src := "read_parquet('" + path + "')"

	for _, tt := range []struct {
		name, query string
		want        int64
	}{
		{"a grep reads every row", "SELECT raw FROM " + src + " WHERE regexp_matches(raw, 'line 99999')", 1_000_000},
		{"a filtered count reads every row", "SELECT count(*) FROM " + src + " WHERE regexp_matches(raw, '7$')", 1_000_000},
		// Known over-count: the pruned scan decodes one 10,000-row group but
		// reports the file's rows. Hive partition pruning, which narrows the
		// files opened, is what the meter does see.
		{"row-group pruning is not seen", "SELECT raw FROM " + src + " WHERE ts BETWEEN 500000 AND 509999 AND regexp_matches(raw, 'line 5')", 1_000_000},
		// Known under-counts: both cost little, so the meter can ignore them.
		{"a bare count is metadata only", "SELECT count(*) FROM " + src, 0},
		{"a limit stops early", "SELECT raw FROM " + src + " LIMIT 5", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := rowsScanned(t, dir, tt.query); got != tt.want {
				t.Errorf("rows scanned = %d, want %d", got, tt.want)
			}
		})
	}
}
