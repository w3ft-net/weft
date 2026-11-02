package shard

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// A keyed archive splits its files by (customer, namespace, service, kind) as
// well as by time, so a query for one log file opens only that file's
// directories. docs/plans/per-service-tables.md, "Archive layout".

func TestKeyedPartitionPath(t *testing.T) {
	cases := []struct {
		path    string
		minutes int
		keyed   bool
	}{
		{"s3://b/40-apps/shard-1.attempt-1/k1440/**/*.parquet", 1440, true},
		{"/var/archive/shard-1.attempt-3/k7/**/*.parquet", 7, true},
		{"s3://b/40-apps/shard-1.attempt-1/m1440/**/*.parquet", 1440, false},
		{"s3://b/40-apps/shard-1.attempt-1/**/*.parquet", 0, false},
		{"s3://b/40-apps/shard-1.attempt-1.parquet", 0, false},
		// a directory that merely starts with k is not a width
		{"s3://b/k180x/**/*.parquet", 0, false},
		{"s3://b/k0/**/*.parquet", 0, false},
	}
	for _, c := range cases {
		minutes, _ := PartitionMinutes(c.path)
		if minutes != c.minutes || IsKeyed(c.path) != c.keyed {
			t.Errorf("%q: width %d keyed %v, want width %d keyed %v", c.path, minutes, IsKeyed(c.path), c.minutes, c.keyed)
		}
	}
}

type keyedRow struct {
	customer                 int64
	namespace, service, kind string // kind "" is stored as NULL
}

func legacyShardWith(t *testing.T, keys []keyedRow, perKey int) *DuckDBShard {
	t.Helper()
	ds, err := NewDuckDBShard("keyed", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })
	t0 := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	for _, k := range keys {
		var kind any = k.kind
		if k.kind == "" {
			kind = nil
		}
		for i := 0; i < perKey; i++ {
			ts := t0.Add(time.Duration(i) * 7 * time.Hour)
			raw := fmt.Sprintf("%d/%s/%s/%s #%d", k.customer, k.namespace, k.service, k.kind, i)
			if _, err := ds.db.Exec("INSERT INTO logs (raw, ts, customer_id, namespace, service, kind) VALUES (?, ?, ?, ?, ?, ?)",
				raw, ts, k.customer, k.namespace, k.service, kind); err != nil {
				t.Fatal(err)
			}
		}
	}
	return ds
}

func exportTo(t *testing.T, ds *DuckDBShard, minutes int, keyed bool) (dir, glob string, rows int64) {
	t.Helper()
	dir = t.TempDir() + "/shard.attempt-1"
	if err := os.MkdirAll(dir, 0o755); err != nil { // the local export makes it too
		t.Fatal(err)
	}
	glob, rows, err := ds.exportParquet(context.Background(), dir, minutes, keyed)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	return dir, glob, rows
}

var keyedFixture = []keyedRow{
	{1, "weft", "api", "main"},
	{1, "weft", "api", "stats"},
	{1, "weft", "web", "main"},
	{2, "weft", "api", "main"},
	{1, "lo/om", "a/b c", "main"}, // names with a slash and a space
	{1, "weft", "api", ""},        // an old row with no kind
}

func TestKeyedExport_WritesADirectoryPerKeyAndReadsBackWhole(t *testing.T) {
	const perKey = 6
	ds := legacyShardWith(t, keyedFixture, perKey)
	dir, glob, rows := exportTo(t, ds, 1440, true)

	if !IsKeyed(glob) {
		t.Fatalf("exported path %q does not say it is keyed", glob)
	}
	if got, ok := PartitionMinutes(glob); !ok || got != 1440 {
		t.Fatalf("exported path %q reads back as width %d, %v", glob, got, ok)
	}
	if want := int64(len(keyedFixture) * perKey); rows != want {
		t.Fatalf("export wrote %d rows, want %d", rows, want)
	}

	// one directory per key, under the key's own path, never another key's
	for _, p := range []string{
		"customer_id=1/namespace=weft/service=api/kind=main",
		"customer_id=2/namespace=weft/service=api/kind=main",
		"customer_id=1/namespace=lo%2Fom/service=a%2Fb%20c/kind=main",
	} {
		files, _ := filepath.Glob(filepath.Join(dir, "k1440", p, "*", "data_0.parquet"))
		if len(files) == 0 {
			t.Errorf("no files under %s", p)
		}
	}

	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	count := func(where string, args ...any) int {
		t.Helper()
		var n int
		q := "SELECT count(*) FROM " + ParquetReadExpr(glob) + " WHERE " + where
		if err := db.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	if got := count("true"); got != len(keyedFixture)*perKey {
		t.Errorf("read back %d rows, want %d", got, len(keyedFixture)*perKey)
	}
	if got := count("customer_id = CAST(? AS BIGINT) AND service = ? AND kind = ?", "1", "api", "main"); got != perKey {
		t.Errorf("weft/api/main read back %d rows, want %d", got, perKey)
	}
	if got := count("service = ?", "a/b c"); got != perKey {
		t.Errorf("a service with a slash and a space read back %d rows, want %d", got, perKey)
	}
	if got := count("kind IS NULL"); got != perKey {
		t.Errorf("rows with no kind read back %d rows, want %d", got, perKey)
	}
	if got := count("customer_id IS NULL"); got != 0 {
		t.Errorf("%d rows came back with no customer", got)
	}
}

// A query for one key must not open another key's files.
func TestKeyedExport_AQueryOpensOnlyItsKeysFiles(t *testing.T) {
	ds := legacyShardWith(t, keyedFixture, 6)
	_, glob, _ := exportTo(t, ds, 1440, true)

	expr := ParquetReadExpr(glob)
	withNames := strings.Replace(expr, "hive_partitioning=true", "hive_partitioning=true, filename=true", 1)
	if withNames == expr {
		t.Fatal("ParquetReadExpr no longer says hive_partitioning=true; update this test")
	}
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT DISTINCT filename FROM "+withNames+
		" WHERE customer_id = CAST(? AS BIGINT) AND namespace = ? AND service = ? AND kind = ?", "1", "weft", "web", "main")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var files []string
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	sort.Strings(files)
	if len(files) == 0 {
		t.Fatal("the query matched no files")
	}
	for _, f := range files {
		if !strings.Contains(f, "service=web/kind=main") {
			t.Errorf("a query for weft/web/main read %s", f)
		}
	}
}

// A service named like a number is typed as a number by DuckDB when every
// service in the archive looks like one; the reader pins the key columns to
// strings so a predicate on another name still binds.
func TestKeyedExport_NumericLookingServiceNamesStayStrings(t *testing.T) {
	ds := legacyShardWith(t, []keyedRow{{1, "weft", "123", "main"}, {1, "weft", "456", "main"}}, 3)
	_, glob, _ := exportTo(t, ds, 1440, true)
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM "+ParquetReadExpr(glob)+" WHERE service = ?", "api").Scan(&n); err != nil {
		t.Fatalf("a predicate on a name that isn't a number failed: %v", err)
	}
	if err := db.QueryRow("SELECT count(*) FROM "+ParquetReadExpr(glob)+" WHERE service = ?", "123").Scan(&n); err != nil || n != 3 {
		t.Fatalf("service 123: %d rows, err=%v, want 3", n, err)
	}
}

// The time pruning restates ts bounds on _part, so it must still be correct
// when the key directories sit above the time ones.
func TestKeyedExport_TimePruningStillMatches(t *testing.T) {
	ds := legacyShardWith(t, keyedFixture, 12)
	_, glob, _ := exportTo(t, ds, 360, true)
	lo := time.Date(2026, 10, 14, 6, 0, 0, 0, time.UTC)
	hi := time.Date(2026, 10, 15, 6, 0, 0, 0, time.UTC)
	args := []string{"1", lo.Format("2006-01-02 15:04:05"), hi.Format("2006-01-02 15:04:05")}
	base := "SELECT count(*) FROM logs WHERE customer_id = CAST(? AS BIGINT) AND ts >= CAST(? AS TIMESTAMP) AND ts <= CAST(? AS TIMESTAMP)"

	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	count := func(q string) int {
		var n int
		q = replaceFirst(q, "FROM logs", "FROM "+ParquetReadExpr(glob))
		if err := db.QueryRow(q, toAny(args)...).Scan(&n); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
		return n
	}
	want := count(base)
	if want == 0 {
		t.Fatal("window holds no rows")
	}
	if got := count(PruneColdPartitions(base, args, glob)); got != want {
		t.Errorf("pruned read counts %d rows, unpruned %d", got, want)
	}
}

// interleavedShard holds rows of 100 services in strict rotation over five days:
// 500 partitions, each batch touching all of them. DuckDB keeps at most 100
// partition files open and starts a new file each time it reopens one, so left
// in this order the export writes many files per partition (1,905 files for
// 308 partitions in a 6M-row trial). Sorted, it writes one each.
func interleavedShard(t *testing.T) *DuckDBShard {
	t.Helper()
	ds, err := NewDuckDBShard("interleaved", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })
	if _, err := ds.db.Exec(`INSERT INTO logs (raw, ts, customer_id, namespace, service, kind)
		SELECT 'line ' || i, TIMESTAMP '2026-10-14 00:00:00' + ((i % 432000) || ' seconds')::INTERVAL,
		       1, 'weft', 'svc' || (i % 100), 'main'
		FROM range(500000) r(i)`); err != nil {
		t.Fatal(err)
	}
	return ds
}

// filesPerPartition counts the parquet files in each partition directory under dir.
func filesPerPartition(t *testing.T, dir string) map[string]int {
	t.Helper()
	counts := map[string]int{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".parquet") {
			counts[filepath.Dir(p)]++
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return counts
}

// checkOneFilePerPartition fails unless every partition under dir is one file,
// so a read of one key opens as few files as the key has days.
func checkOneFilePerPartition(t *testing.T, dir string, wantPartitions int) {
	t.Helper()
	counts := filesPerPartition(t, dir)
	if len(counts) != wantPartitions {
		t.Errorf("%d partitions, want %d", len(counts), wantPartitions)
	}
	var files int
	for _, n := range counts {
		files += n
	}
	if files != len(counts) {
		t.Errorf("%d files in %d partitions, want one each", files, len(counts))
	}
}

func TestKeyedExport_ManyPartitionsGetOneFileEach(t *testing.T) {
	ds := interleavedShard(t)
	dir, _, rows := exportTo(t, ds, 1440, true)
	if rows != 500000 {
		t.Fatalf("export wrote %d rows, want 500000", rows)
	}
	checkOneFilePerPartition(t, filepath.Join(dir, "k1440"), 500) // 100 services x 5 days
}

// Converting a time-only archive makes the same one file per partition.
func TestRewriteRetention_ConversionGetsOneFilePerPartition(t *testing.T) {
	ds := interleavedShard(t)
	_, src, _ := exportTo(t, ds, 1440, false)
	dstDir := filepath.Join(t.TempDir(), "shard.attempt-1.r1")
	res, err := RewriteRetention(src, dstDir+"/k1440/**/*.parquet", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.RowsKept != 500000 {
		t.Fatalf("kept %d rows, want 500000", res.RowsKept)
	}
	checkOneFilePerPartition(t, filepath.Join(dstDir, "k1440"), 500)
}

// A retention rewrite of a keyed archive writes a keyed archive. Written as one
// file it would put every service back in every file and drop the bucket width.
func TestRewriteRetention_KeepsAKeyedLayout(t *testing.T) {
	const perKey = 6
	ds := legacyShardWith(t, keyedFixture, perKey)
	_, src, _ := exportTo(t, ds, 1440, true)

	dstDir := filepath.Join(t.TempDir(), "shard.attempt-1.r1")
	dst := dstDir + "/k1440/**/*.parquet"
	// Rows are 7 hours apart from 2026-10-14 00:00; customer 1 loses the two
	// before 14:00 in each of its 5 keys, customer 2 keeps everything.
	cutoff := time.Date(2026, 10, 14, 14, 0, 0, 0, time.UTC)
	res, err := RewriteRetention(src, dst, []CustomerCutoff{{CustomerID: 1, Cutoff: cutoff}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.RowsKept != 5*4+6 || res.RowsDeleted != 5*2 {
		t.Fatalf("kept %d, deleted %d; want 26 and 10", res.RowsKept, res.RowsDeleted)
	}
	if res.OutputPath != dst || !IsKeyed(res.OutputPath) {
		t.Fatalf("output path %q is not the keyed destination %q", res.OutputPath, dst)
	}
	if minutes, ok := PartitionMinutes(res.OutputPath); !ok || minutes != 1440 {
		t.Errorf("the bucket width did not survive: %d, %v", minutes, ok)
	}
	files, _ := filepath.Glob(filepath.Join(dstDir, "k1440", "customer_id=1/namespace=weft/service=api/kind=main", "*", "data_0.parquet"))
	if len(files) == 0 {
		t.Error("the rewrite wrote no directory for weft/api/main")
	}

	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n, early int
	q := "SELECT count(*), count(*) FILTER (WHERE customer_id = 1 AND ts < TIMESTAMP '2026-10-14 14:00:00') FROM " + ParquetReadExpr(dst)
	if err := db.QueryRow(q).Scan(&n, &early); err != nil {
		t.Fatal(err)
	}
	if n != 26 || early != 0 {
		t.Errorf("rewritten archive holds %d rows, %d of them expired; want 26 and 0", n, early)
	}
	if err := db.QueryRow("SELECT count(*) FROM "+ParquetReadExpr(dst)+" WHERE service = ? AND kind = ? AND customer_id = CAST(? AS BIGINT)", "api", "main", "1").Scan(&n); err != nil || n != 4 {
		t.Errorf("weft/api/main holds %d rows after the rewrite (err %v), want 4", n, err)
	}
}

// Converting an archive is a rewrite with no cutoffs whose destination is
// keyed. The source may be a single file, which has no _part, or a time-only
// archive of any width, whose _part is rebuilt for the new one.
func TestRewriteRetention_ConvertsAnythingToKeyed(t *testing.T) {
	const perKey = 6
	ds := legacyShardWith(t, keyedFixture, perKey)
	want := len(keyedFixture) * perKey

	for _, c := range []struct {
		name    string
		minutes int // width of the time-only source; 0 is a single file
	}{{"single file", 0}, {"time-only, 60 minutes", 60}, {"time-only, 1440 minutes", 1440}} {
		t.Run(c.name, func(t *testing.T) {
			_, src, _ := exportTo(t, ds, c.minutes, false)
			if IsKeyed(src) {
				t.Fatalf("source %q is already keyed", src)
			}
			dstDir := filepath.Join(t.TempDir(), "shard.attempt-1.r1")
			dst := dstDir + "/k1440/**/*.parquet"
			res, err := RewriteRetention(src, dst, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if res.RowsKept != int64(want) || res.RowsDeleted != 0 {
				t.Fatalf("kept %d, deleted %d; want %d and 0", res.RowsKept, res.RowsDeleted, want)
			}
			if files, _ := filepath.Glob(filepath.Join(dstDir, "k1440", "customer_id=1/namespace=weft/service=api/kind=main", "*", "data_0.parquet")); len(files) == 0 {
				t.Error("no directory for weft/api/main")
			}

			db, err := sql.Open("duckdb", "")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var n, parts, nulls int
			q := "SELECT count(*), count(DISTINCT _part), count(*) FILTER (WHERE kind IS NULL) FROM " + ParquetReadExpr(dst)
			if err := db.QueryRow(q).Scan(&n, &parts, &nulls); err != nil {
				t.Fatal(err)
			}
			// rows are 7 hours apart over 35 hours: three day buckets
			if n != want || parts != 2 || nulls != perKey {
				t.Errorf("converted archive: %d rows, %d day buckets, %d with no kind; want %d, 2, %d", n, parts, nulls, want, perKey)
			}
		})
	}
}

// The cold shard answers a query the same way from either layout.
func TestColdShard_KeyedAndTimeOnlyArchivesAnswerAlike(t *testing.T) {
	ds := legacyShardWith(t, keyedFixture, 6)
	_, plain, _ := exportTo(t, ds, 1440, false)
	_, keyed, _ := exportTo(t, ds, 1440, true)
	if IsKeyed(plain) || !IsKeyed(keyed) {
		t.Fatalf("layouts are %q and %q", plain, keyed)
	}
	ask := func(glob string) []string {
		cs, err := NewColdShard("cold", nil, glob, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer cs.Close()
		rows, err := cs.Query(context.Background(),
			"SELECT raw, ts FROM logs WHERE customer_id = CAST(? AS BIGINT) AND namespace = ? AND service = ? AND kind = ? ORDER BY ts",
			"1", "weft", "api", "main")
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, r.Raw)
		}
		return out
	}
	a, b := ask(plain), ask(keyed)
	if len(a) != 6 || strings.Join(a, "\n") != strings.Join(b, "\n") {
		t.Errorf("time-only archive returned %d rows, keyed %d:\n%v\n%v", len(a), len(b), a, b)
	}
}
