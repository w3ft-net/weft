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

	"github.com/w3ft-net/weft/shape"
)

// A keyed archive sizes its time buckets per key, so a big key's files and a
// small key's files each come out near targetFileBytes.

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
func TestWriteArchive_ConvertsInAMemoryLimitedLambda(t *testing.T) {
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
	copied, err := copyArchive(src, dstDir+"/sized/**/*.parquet", shape.Legacy())
	if err != nil {
		t.Fatalf("a conversion larger than memory failed: %v", err)
	}
	if copied != rows {
		t.Errorf("copied %d rows, want %d", copied, rows)
	}
}
