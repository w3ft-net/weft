package shard

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/shape"
)

// A keyed archive sizes its time buckets per key, so a big key's files and a
// small key's files each come out near targetFileBytes. docs/plans/per-service-tables.md,
// "Archive layout".

func TestSizedPartitionPath(t *testing.T) {
	cases := []struct {
		path         string
		minutes      int
		keyed, sized bool
	}{
		{"s3://b/40-apps/shard-1.attempt-1/sized/**/*.parquet", 0, true, true},
		{"/var/archive/shard-1.attempt-3.r17/sized/**/*.parquet", 0, true, true},
		// the first keyed layout, one width for every key: still read, no longer written
		{"s3://b/40-apps/shard-1.attempt-1/k1440/**/*.parquet", 1440, true, false},
		{"s3://b/40-apps/shard-1.attempt-1/m1440/**/*.parquet", 1440, false, false},
		{"s3://b/40-apps/shard-1.attempt-1/**/*.parquet", 0, false, false},
		{"s3://b/40-apps/shard-1.attempt-1.parquet", 0, false, false},
		// a directory that merely starts with these is not the layout
		{"s3://b/sizedx/**/*.parquet", 0, false, false},
		{"s3://b/k0/**/*.parquet", 0, false, false},
	}
	for _, c := range cases {
		minutes, _ := PartitionMinutes(c.path)
		if minutes != c.minutes || IsKeyed(c.path) != c.keyed || IsSized(c.path) != c.sized {
			t.Errorf("%q: width %d keyed %v sized %v; want width %d keyed %v sized %v",
				c.path, minutes, IsKeyed(c.path), IsSized(c.path), c.minutes, c.keyed, c.sized)
		}
	}
}

func TestPickWidth(t *testing.T) {
	const mib = 1 << 20
	day := 24 * time.Hour
	for _, c := range []struct {
		name string
		est  int64
		span time.Duration
		want int
	}{
		// 1 GiB is 64 target files; a week over 64 is 157 minutes, so the next width up
		{"a week of 1 GiB", 1024 * mib, 7 * day, 360},
		// 200 MiB is 12.5 files; 3 days over 12.5 is 5.8 hours, so 6 hours
		{"three days of 200 MiB", 200 * mib, 3 * day, 360},
		// less than one file: one bucket for the whole span, the widest
		{"a key too small for a second file", 10 * mib, 7 * day, 10080},
		{"nothing to size", 0, 7 * day, 10080},
		// more files than minutes: the narrowest
		{"an hour of 1 GiB", 1024 * mib, time.Hour, 60},
		{"a day of 100 GiB", 100 * 1024 * mib, day, 60},
		// all rows at one instant: one bucket
		{"no span", 1024 * mib, 0, 10080},
		// 192 MiB is exactly 12 files; 3 days over 12 is exactly 360 minutes, which stays 360
		{"on a width exactly", 192 * mib, 3 * day, 360},
	} {
		got := pickWidth(c.est, c.span)
		if got != c.want {
			t.Errorf("%s: pickWidth = %d, want %d", c.name, got, c.want)
		}
	}
}

// In one shard a big key and a small key get buckets of different widths, and a
// time-bounded read returns exactly the same rows with the pruning as without
// it, for each. The big key has 4,000 rows over a week and most of the bytes;
// the small one has 20 rows.
func TestSizedExport_EachKeyGetsAWidthForItsOwnSize(t *testing.T) {
	ds, err := NewDuckDBShard("widths", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })
	for _, q := range []string{
		`INSERT INTO logs (raw, ts, customer_id, namespace, service, kind)
			SELECT 'big line ' || i, TIMESTAMP '2026-10-14 00:00:00' + ((i * 151) || ' seconds')::INTERVAL, 1, 'weft', 'big', 'main' FROM range(4000) r(i)`,
		`INSERT INTO logs (raw, ts, customer_id, namespace, service, kind)
			SELECT 'small line ' || i, TIMESTAMP '2026-10-14 00:00:00' + ((i * 8) || ' hours')::INTERVAL, 1, 'weft', 'small', 'main' FROM range(20) r(i)`,
	} {
		if _, err := ds.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	dir, glob, rows := exportSizedTo(t, ds, 1<<30) // taken to be 1 GiB of Parquet
	if rows != 4020 {
		t.Fatalf("export wrote %d rows, want 4020", rows)
	}

	widthOf := func(service string) int {
		t.Helper()
		dirs, _ := filepath.Glob(filepath.Join(dir, "sized", "customer_id=1", "namespace=weft", "service="+service, "kind=main", "_w=*"))
		if len(dirs) != 1 {
			t.Fatalf("service %s has %d width directories, want 1: %v", service, len(dirs), dirs)
		}
		w, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(dirs[0]), "_w="))
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	big, small := widthOf("big"), widthOf("small")
	if small != 10080 {
		t.Errorf("the small key has %d-minute buckets, want the widest (10080)", small)
	}
	if big >= small || big > 1440 {
		t.Errorf("the big key has %d-minute buckets, want narrower than the small key's %d and at most a day", big, small)
	}

	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, service := range []string{"big", "small"} {
		for _, w := range []struct{ lo, hi time.Time }{
			{time.Date(2026, 10, 17, 12, 30, 0, 0, time.UTC), time.Date(2026, 10, 17, 14, 30, 0, 0, time.UTC)},
			{time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 14, 0, 10, 0, 0, time.UTC)},
			{time.Date(2026, 10, 20, 22, 0, 0, 0, time.UTC), time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)},
			{time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
		} {
			args := []string{service, w.lo.Format("2006-01-02 15:04:05"), w.hi.Format("2006-01-02 15:04:05")}
			base := "SELECT count(*) FROM logs WHERE service = ? AND ts >= CAST(? AS TIMESTAMP) AND ts <= CAST(? AS TIMESTAMP)"
			count := func(q string) int {
				var n int
				q = replaceFirst(q, "FROM logs", "FROM "+ParquetReadExpr(glob, shape.Legacy()))
				if err := db.QueryRow(q, toAny(args)...).Scan(&n); err != nil {
					t.Fatalf("%v\n%s", err, q)
				}
				return n
			}
			want, got := count(base), count(PruneColdPartitions(base, args, glob))
			if got != want {
				t.Errorf("%s %v..%v: pruned read counts %d rows, unpruned %d", service, w.lo, w.hi, got, want)
			}
		}
	}
}

// The compactor sorts what it writes, which can need more than DuckDB's memory
// limit. In a Lambda DuckDB would believe it has the host's memory, not the
// function's, and its default spill directory is relative to a working directory
// that is read-only: the sort would be killed or fail with "Failed to create
// directory .tmp: Permission denied". So the limit comes from the function's
// memory and the spill goes to the temp directory.
func TestLambdaSettings(t *testing.T) {
	spill := "SET temp_directory='" + filepath.Join(os.TempDir(), "duckdb-spill") + "'"

	t.Setenv("AWS_LAMBDA_FUNCTION_MEMORY_SIZE", "2048")
	got := strings.Join(lambdaSettings(), "; ")
	if want := fmt.Sprintf("SET memory_limit='1228MB'; SET threads=%d; %s", min(runtime.NumCPU(), 2), spill); got != want {
		t.Errorf("in a 2048 MB Lambda: %q, want %q", got, want)
	}

	t.Setenv("AWS_LAMBDA_FUNCTION_MEMORY_SIZE", "")
	if got, want := strings.Join(lambdaSettings(), "; "), spill; got != want {
		t.Errorf("outside a Lambda: %q, want just %q", got, want)
	}

	t.Setenv("AWS_LAMBDA_FUNCTION_MEMORY_SIZE", "not a number")
	if got, want := strings.Join(lambdaSettings(), "; "), spill; got != want {
		t.Errorf("with a bad memory size: %q, want just %q", got, want)
	}
}

// Under the memory a small Lambda has, from a read-only working directory, a
// conversion larger than memory completes by spilling.
func TestCopyArchive_ConvertsInAMemoryLimitedLambda(t *testing.T) {
	if testing.Short() {
		t.Skip("writes and sorts a few hundred MB")
	}
	ds, err := NewDuckDBShard("spill", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })
	const rows = 2_400_000
	if _, err := ds.db.Exec(fmt.Sprintf(`INSERT INTO logs (raw, ts, customer_id, namespace, service, kind)
		SELECT repeat('x', 180) || i, TIMESTAMP '2026-10-14 00:00:00' + ((i %% 432000) || ' seconds')::INTERVAL,
		       1, 'weft', 'svc' || (i %% 40), 'main' FROM range(%d) r(i)`, rows)); err != nil {
		t.Fatal(err)
	}
	// Small row groups, so that the readers (one per core: 2 in a Lambda, more on
	// the machine running this test) do not themselves exhaust the limit; what
	// has to spill is the sort.
	src := filepath.Join(t.TempDir(), "src.parquet")
	if _, err := ds.db.Exec(fmt.Sprintf("COPY logs TO '%s' (FORMAT PARQUET, ROW_GROUP_SIZE 8192)", src)); err != nil {
		t.Fatal(err)
	}

	readonly := t.TempDir()
	if err := os.Chmod(readonly, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(readonly, 0o755) })
	t.Chdir(readonly)
	t.Setenv("AWS_LAMBDA_FUNCTION_MEMORY_SIZE", "512") // a 307 MB limit for about 480 MB of rows

	dstDir := filepath.Join(t.TempDir(), "shard.attempt-1.r1")
	copied, err := CopyArchive(src, dstDir+"/sized/**/*.parquet", shape.Legacy(), nil)
	if err != nil {
		t.Fatalf("a conversion larger than memory failed: %v", err)
	}
	if copied != rows {
		t.Errorf("copied %d rows, want %d", copied, rows)
	}
}

// Every width pickWidth can return is one a reader's bucket arithmetic and a
// day-aligned grid both handle: whole hours, dividing a week or a day.
func TestKeyedWidthsAreAligned(t *testing.T) {
	for _, w := range keyedWidths {
		if w%60 != 0 {
			t.Errorf("width %d is not a whole number of hours", w)
		}
		if 10080%w != 0 && w%1440 != 0 {
			t.Errorf("width %d neither divides a week nor is whole days", w)
		}
	}
}
