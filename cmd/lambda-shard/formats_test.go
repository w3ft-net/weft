package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/planner"
	"github.com/graybeardcorp/weft/shard"
)

// Every archive format the cold shard still has to read, and the keyed one,
// must answer the planner's real queries with the same rows. The formats:
// a single file; a time-partitioned archive with no recorded width (the oldest);
// time-partitioned with the width in the path (m<minutes>); and keyed (k<minutes>),
// which also splits by log key. docs/plans/per-service-tables.md, "Archive layout".

type fixtureKey struct {
	customer           int64
	namespace, service string
	kind               string
	offsetSec          int // keeps every row's timestamp unique, so ORDER BY ts is total
}

var (
	fxAcmeAPIMain    = fixtureKey{5, "acme", "api", "main", 0}
	fxAcmeAPIStats   = fixtureKey{5, "acme", "api", "stats", 1}
	fxAcmeWebMain    = fixtureKey{5, "acme", "web", "main", 2}
	fxOtherAPIMain   = fixtureKey{5, "other", "api", "main", 3}
	fxTenant6APIMain = fixtureKey{6, "acme", "api", "main", 4} // another tenant, same names
	fxKeys           = []fixtureKey{fxAcmeAPIMain, fxAcmeAPIStats, fxAcmeWebMain, fxOtherAPIMain, fxTenant6APIMain}
)

type fixtureRow struct {
	raw string
	ts  time.Time
	key fixtureKey
}

var fxStart = time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)

// fixtureRows is 60 rows per key, an hour apart (2.5 days), every fifth an error.
func fixtureRows() []fixtureRow {
	var rows []fixtureRow
	for _, k := range fxKeys {
		for i := 0; i < 60; i++ {
			level := "ok"
			if i%5 == 0 {
				level = "error"
			}
			rows = append(rows, fixtureRow{
				raw: fmt.Sprintf("c%d %s/%s/%s #%02d %s", k.customer, k.namespace, k.service, k.kind, i, level),
				ts:  fxStart.Add(time.Duration(i)*time.Hour + time.Duration(k.offsetSec)*time.Second),
				key: k,
			})
		}
	}
	return rows
}

func loadFixture(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE logs (raw TEXT, ts TIMESTAMP, customer_id BIGINT, namespace TEXT, service TEXT,
		kind TEXT, env TEXT, event_id UUID, line_no INTEGER)`); err != nil {
		t.Fatal(err)
	}
	for _, r := range fixtureRows() {
		if _, err := db.Exec("INSERT INTO logs (raw, ts, customer_id, namespace, service, kind) VALUES (?, ?, ?, ?, ?, ?)",
			r.raw, r.ts, r.key.customer, r.key.namespace, r.key.service, r.key.kind); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// archiveFormats writes the fixture in each format and returns name -> path.
func archiveFormats(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	root := t.TempDir()
	out := map[string]string{}
	bucket := func(minutes int) string {
		return fmt.Sprintf("time_bucket(INTERVAL '%d minutes', ts, TIMESTAMP '1970-01-01')", minutes)
	}
	copyTo := func(sel, to, partitionBy string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			t.Fatal(err)
		}
		opts := "FORMAT PARQUET"
		if partitionBy != "" {
			opts += ", PARTITION_BY (" + partitionBy + "), OVERWRITE_OR_IGNORE"
		}
		if _, err := db.Exec(fmt.Sprintf("COPY (%s) TO '%s' (%s)", sel, to, opts)); err != nil {
			t.Fatal(err)
		}
	}

	single := filepath.Join(root, "single", "shard.parquet")
	copyTo("SELECT * FROM logs", single, "")
	out["single file"] = single

	legacy := filepath.Join(root, "legacy", "shard.attempt-1")
	copyTo("SELECT *, date_trunc('day', ts) AS _part FROM logs", legacy, "_part")
	out["time-only, no width recorded"] = legacy + "/**/*.parquet"

	for _, m := range []int{60, 1440} {
		dir := filepath.Join(root, fmt.Sprintf("m%d", m), "shard.attempt-1", fmt.Sprintf("m%d", m))
		copyTo(fmt.Sprintf("SELECT *, %s AS _part FROM logs", bucket(m)), dir, "_part")
		out[fmt.Sprintf("time-only, m%d", m)] = dir + "/**/*.parquet"
	}
	// The first keyed layout, one width for every key: no longer written, still read.
	for _, m := range []int{60, 360, 1440} {
		dir := filepath.Join(root, fmt.Sprintf("k%d", m), "shard.attempt-1", fmt.Sprintf("k%d", m))
		copyTo(fmt.Sprintf("SELECT *, %s AS _part FROM logs", bucket(m)), dir, "customer_id, namespace, service, kind, _part")
		out[fmt.Sprintf("keyed, k%d", m)] = dir + "/**/*.parquet"
	}

	// The sized layout: each key's own width in _w, here a different one for most
	// keys, so a query's time bound has to be right for every width at once.
	sized := filepath.Join(root, "sized", "shard.attempt-1", "sized")
	copyTo(`SELECT * EXCLUDE (w), w AS _w, time_bucket(to_minutes(w), ts, TIMESTAMP '1970-01-01') AS _part
		FROM (SELECT *, CASE WHEN service = 'api' AND kind = 'main' THEN 60
			WHEN kind = 'stats' THEN 1440 WHEN service = 'web' THEN 360 ELSE 10080 END AS w FROM logs)`,
		sized, "customer_id, namespace, service, kind, _w, _part")
	out["sized, a width per key"] = sized + "/**/*.parquet"
	return out
}

// planFor is what the coordinator sends a cold shard for a pipeline: the plan's
// SQL and its args, as strings.
func planFor(t *testing.T, pipeline string, customer int64, start, end time.Time) (string, []string) {
	t.Helper()
	plan, err := planner.Plan(pipeline, customer, "", start, end)
	if err != nil {
		t.Fatalf("plan %q: %v", pipeline, err)
	}
	q := plan.DirectShardSQL
	if q == "" {
		q = plan.ShardSQL
	}
	args := make([]string, len(plan.Args))
	for i, a := range plan.Args {
		args[i] = fmt.Sprint(a)
	}
	return q, args
}

func expectedRaws(keep func(fixtureRow) bool, start, end time.Time) []string {
	var rows []fixtureRow
	for _, r := range fixtureRows() {
		if keep(r) && !r.ts.Before(start) && !r.ts.After(end) {
			rows = append(rows, r)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ts.Before(rows[j].ts) })
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.raw
	}
	return out
}

func TestHandleQuery_EveryArchiveFormatAnswersAlike(t *testing.T) {
	db := loadFixture(t)
	formats := archiveFormats(t, db)

	wide0, wide1 := fxStart.Add(-24*time.Hour), fxStart.Add(10*24*time.Hour)
	win0, win1 := fxStart.Add(11*time.Hour+30*time.Minute), fxStart.Add(13*time.Hour+30*time.Minute)
	is := func(keys ...fixtureKey) func(fixtureRow) bool {
		return func(r fixtureRow) bool {
			for _, k := range keys {
				if r.key == k {
					return true
				}
			}
			return false
		}
	}
	queries := []struct {
		name       string
		pipeline   string
		customer   int64
		start, end time.Time
		keep       func(fixtureRow) bool
	}{
		{"one key, whole range", "cat /log/acme/api/main.log", 5, wide0, wide1, is(fxAcmeAPIMain)},
		{"one key, a two-hour window", "cat /log/acme/api/main.log", 5, win0, win1, is(fxAcmeAPIMain)},
		{"another kind", "cat /log/acme/api/stats.log", 5, wide0, wide1, is(fxAcmeAPIStats)},
		{"another namespace", "cat /log/other/api/main.log", 5, wide0, wide1, is(fxOtherAPIMain)},
		{"the same path as another tenant", "cat /log/acme/api/main.log", 6, wide0, wide1, is(fxTenant6APIMain)},
		{"two services", "cat /log/acme/api/main.log /log/acme/web/main.log", 5, wide0, wide1, is(fxAcmeAPIMain, fxAcmeWebMain)},
		{"a grep the shard runs", "cat /log/acme/web/main.log | grep error", 5, wide0, wide1,
			func(r fixtureRow) bool { return r.key == fxAcmeWebMain && strings.HasSuffix(r.raw, "error") }},
		{"a key with no rows", "cat /log/acme/missing/main.log", 5, wide0, wide1, is()},
	}

	for name, path := range formats {
		for _, q := range queries {
			t.Run(name+"/"+q.name, func(t *testing.T) {
				query, args := planFor(t, q.pipeline, q.customer, q.start, q.end)
				var buf bytes.Buffer
				if err := HandleQuery(context.Background(), &buf, QueryRequest{SQL: query, ParquetPath: path, Args: args}, nil); err != nil {
					t.Fatalf("HandleQuery: %v\nsql: %s\nargs: %v", err, query, args)
				}
				rows, err := shard.ReadAllResultRows(&buf)
				if err != nil {
					t.Fatal(err)
				}
				var got []string
				for _, r := range rows {
					got = append(got, r.Raw)
				}
				want := expectedRaws(q.keep, q.start, q.end)
				if strings.Join(got, "\n") != strings.Join(want, "\n") {
					t.Errorf("got %d rows, want %d\n got: %v\nwant: %v", len(got), len(want), head(got), head(want))
				}
			})
		}
	}
}

func head(s []string) []string {
	if len(s) > 4 {
		return append(s[:4:4], fmt.Sprintf("... (%d total)", len(s)))
	}
	return s
}
