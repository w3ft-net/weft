package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/shard"
)

// Archives are exported hive-partitioned at a granularity picked from the
// shard's time span: a day, an hour, 10 minutes or a minute. A query's time
// bounds must pick out exactly its rows from any of them, and must do it
// without opening files outside the bounds: the Lambda starts a fresh DuckDB
// per query, and opening every file's footer in a week of hourly partitions
// is where an archived read's seconds go.
func TestHandleQuery_PrunesPartitionsAtEveryGranularity(t *testing.T) {
	// segment is the m<minutes> directory a width-carrying archive nests its
	// partitions under (shard.PartitionMinutes); legacy archives have none.
	grans := map[string]struct{ expr, segment string }{
		"legacy day":   {"date_trunc('day', ts)", ""},
		"legacy hour":  {"date_trunc('hour', ts)", ""},
		"legacy 10min": {"time_bucket(INTERVAL '10 minutes', ts)", ""},
		"legacy 1min":  {"time_bucket(INTERVAL '1 minute', ts)", ""},
		"width 594":    {"time_bucket(INTERVAL '594 minutes', ts, TIMESTAMP '1970-01-01')", "m594"},
		"width 7":      {"time_bucket(INTERVAL '7 minutes', ts, TIMESTAMP '1970-01-01')", "m7"},
	}
	t0 := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	// Rows every 7 minutes for 3 days, so they straddle every bucket edge.
	var stamps []time.Time
	for ts := t0; ts.Before(t0.Add(72 * time.Hour)); ts = ts.Add(7 * time.Minute) {
		stamps = append(stamps, ts)
	}
	start := time.Date(2026, 10, 15, 11, 55, 0, 0, time.UTC)
	end := time.Date(2026, 10, 15, 12, 10, 0, 0, time.UTC)
	want := 0
	for _, ts := range stamps {
		if !ts.Before(start) && !ts.After(end) {
			want++
		}
	}
	if want == 0 {
		t.Fatal("test window holds no rows")
	}

	for name, g := range grans {
		t.Run(name, func(t *testing.T) {
			dir := exportPartitioned(t, stamps, g.expr, g.segment)
			corruptPartitionsFrom(t, dir, t0.Add(48*time.Hour))

			var buf bytes.Buffer
			err := HandleQuery(context.Background(), &buf, QueryRequest{
				SQL:         "SELECT raw, ts FROM logs WHERE customer_id = CAST(? AS BIGINT) AND ts >= CAST(? AS TIMESTAMP) AND ts <= CAST(? AS TIMESTAMP)",
				Args:        []string{"1", start.Format("2006-01-02 15:04:05"), end.Format("2006-01-02 15:04:05")},
				ParquetPath: filepath.Join(dir, g.segment) + "/**/*.parquet",
			}, nil)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			rows, err := shard.ReadAllResultRows(&buf)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if len(rows) != want {
				t.Fatalf("%s granularity: got %d rows, want %d", name, len(rows), want)
			}
		})
	}
}

// exportPartitioned writes rows (raw=timestamp, customer 1) the way the
// shard's archive does: COPY ... PARTITION_BY (_part).
func exportPartitioned(t *testing.T, stamps []time.Time, partExpr, segment string) string {
	t.Helper()
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE logs (raw TEXT, ts TIMESTAMP, customer_id BIGINT)"); err != nil {
		t.Fatal(err)
	}
	for _, ts := range stamps {
		if _, err := db.Exec("INSERT INTO logs VALUES (?, ?, 1)", ts.Format(time.RFC3339), ts); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	if _, err := db.Exec(fmt.Sprintf("COPY (SELECT *, %s AS _part FROM logs) TO '%s' (FORMAT PARQUET, PARTITION_BY (_part), OVERWRITE_OR_IGNORE)", partExpr, filepath.Join(dir, segment))); err != nil {
		t.Fatal(err)
	}
	return dir
}

// corruptPartitionsFrom overwrites every parquet file whose partition starts
// at or after notBefore, so a query that opens one fails: only a read that
// skips those files survives.
func corruptPartitionsFrom(t *testing.T, root string, notBefore time.Time) {
	t.Helper()
	files, err := filepath.Glob(root + "/*/*/*.parquet")
	if len(files) == 0 {
		files, err = filepath.Glob(root + "/*/*.parquet")
	}
	if err != nil || len(files) == 0 {
		t.Fatalf("no partitions under %s: %v", root, err)
	}
	corrupted := 0
	for _, f := range files {
		dir, err := url.PathUnescape(filepath.Base(filepath.Dir(f)))
		if err != nil {
			t.Fatal(err)
		}
		start, err := time.Parse("_part=2006-01-02 15:04:05", dir)
		if err != nil {
			t.Fatalf("partition dir %q: %v", dir, err)
		}
		if !start.Before(notBefore) {
			if err := os.WriteFile(f, []byte("not parquet"), 0o644); err != nil {
				t.Fatal(err)
			}
			corrupted++
		}
	}
	if corrupted == 0 {
		t.Fatalf("no partition at or after %s to corrupt", notBefore)
	}
}
