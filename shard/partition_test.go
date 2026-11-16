package shard

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"testing"
	"time"

	"github.com/w3ft-net/weft/shape"
)

// The reader works out which partition holds a timestamp itself, so it has to
// agree with the bucket DuckDB writes. This pins the writer's expression and
// the reader's arithmetic to each other across widths that don't divide an
// hour or a day.
func TestBucketStart_MatchesWhatTheExportWrites(t *testing.T) {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rng := rand.New(rand.NewSource(1))
	base := time.Date(2026, 10, 11, 23, 51, 35, 0, time.UTC)
	for _, minutes := range []int{1, 7, 10, 60, 180, 594, 1440, 2000} {
		for i := 0; i < 40; i++ {
			ts := base.Add(time.Duration(rng.Int63n(int64(9 * 24 * time.Hour))))
			var got time.Time
			q := fmt.Sprintf("SELECT %s", partitionExpr(minutes, "CAST(? AS TIMESTAMP)"))
			if err := db.QueryRow(q, ts.Format("2006-01-02 15:04:05.999999")).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if want := BucketStart(ts, minutes); !got.Equal(want) {
				t.Fatalf("width %d, ts %s: export puts it in %s, BucketStart says %s", minutes, ts, got, want)
			}
		}
	}
}

// Whatever width is chosen, the archive must read back exactly the rows a
// time-bounded query asks for with the narrowed bounds, and the files outside
// the bounds must not need opening.
func TestExportedArchive_BoundedReadMatchesAnUnboundedOne(t *testing.T) {
	ctx := context.Background()
	ds, err := NewDuckDBShard("pw", "")
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	t0 := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 72*60/5; i++ { // 3 days, a row every 5 minutes
		ts := t0.Add(time.Duration(i) * 5 * time.Minute)
		if _, err := ds.db.Exec("INSERT INTO logs (raw, ts, customer_id, namespace, service, kind) VALUES (?, ?, 1, 'weft', 'shard', 'main')", ts.Format(time.RFC3339), ts); err != nil {
			t.Fatal(err)
		}
	}

	for _, minutes := range []int{7, 60, 594, 1440} {
		dir := t.TempDir() + "/shard.attempt-1"
		if err := os.MkdirAll(dir, 0o755); err != nil { // the local export makes it too
			t.Fatal(err)
		}
		glob, _, err := ds.exportParquet(ctx, dir, minutes)
		if err != nil {
			t.Fatalf("width %d: export: %v", minutes, err)
		}
		if got, ok := PartitionMinutes(glob); !ok || got != minutes {
			t.Fatalf("width %d: exported path %q reads back as %d, %v", minutes, glob, got, ok)
		}
		lo := time.Date(2026, 10, 15, 11, 58, 0, 0, time.UTC)
		hi := time.Date(2026, 10, 15, 12, 22, 0, 0, time.UTC)
		args := []string{"1", lo.Format("2006-01-02 15:04:05"), hi.Format("2006-01-02 15:04:05")}
		base := "SELECT count(*) FROM logs WHERE customer_id = CAST(? AS BIGINT) AND ts >= CAST(? AS TIMESTAMP) AND ts <= CAST(? AS TIMESTAMP)"

		count := func(q string) int {
			db, err := sql.Open("duckdb", "")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			q = replaceFirst(q, "FROM logs", "FROM "+ParquetReadExpr(glob, shape.Legacy()))
			var n int
			if err := db.QueryRow(q, toAny(args)...).Scan(&n); err != nil {
				t.Fatalf("width %d: %v\n%s", minutes, err, q)
			}
			return n
		}
		want := count(base)
		if want == 0 {
			t.Fatal("window holds no rows")
		}
		if got := count(PruneColdPartitions(base, args, glob)); got != want {
			t.Errorf("width %d: pruned read counts %d rows, unpruned %d", minutes, got, want)
		}
	}
}

func replaceFirst(s, old, new string) string {
	for i := 0; i+len(old) <= len(s); i++ {
		if s[i:i+len(old)] == old {
			return s[:i] + new + s[i+len(old):]
		}
	}
	return s
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
