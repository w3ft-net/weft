package retention

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/logdb"
	"github.com/graybeardcorp/weft/shard"
)

// A keyed archive (docs/plans/per-service-tables.md, "Archive layout") must
// come out of a retention rewrite keyed, or the next pass undoes it. Rewriting is
// all the janitor does with one: converting an archive that is not keyed is a
// one-off, not part of the pass.

func TestRewriteDestPath_KeepsTheLayout(t *testing.T) {
	now := time.Unix(1_700_000_000, 5)
	tag := fmt.Sprintf(".r%d", now.UnixNano())
	for _, c := range []struct{ old, want string }{
		// a sized archive is rewritten as a sized one
		{"s3://b/40-apps/shard-1.attempt-1/sized/**/*.parquet", "s3://b/40-apps/shard-1.attempt-1" + tag + "/sized/**/*.parquet"},
		{"s3://b/40-apps/shard-1.attempt-3.r17/sized/**/*.parquet", "s3://b/40-apps/shard-1.attempt-3.r17" + tag + "/sized/**/*.parquet"},
		// the first keyed layout (one width for every key) is rewritten as a sized one
		{"s3://b/40-apps/shard-1.attempt-3/k7/**/*.parquet", "s3://b/40-apps/shard-1.attempt-3" + tag + "/sized/**/*.parquet"},
		{"s3://b/40-apps/shard-1.attempt-1/k1440/**/*.parquet", "s3://b/40-apps/shard-1.attempt-1" + tag + "/sized/**/*.parquet"},
		// archives that are not keyed are rewritten as a single file, as they always were
		{"s3://b/40-apps/shard-1.attempt-1/m1440/**/*.parquet", "s3://b/40-apps/shard-1.attempt-1/m1440" + tag + ".parquet"},
		{"s3://b/40-apps/shard-1.parquet", "s3://b/40-apps/shard-1" + tag + ".parquet"},
		{"s3://b/40-apps/shard-20261012.attempt-1/**/*.parquet", "s3://b/40-apps/shard-20261012.attempt-1" + tag + ".parquet"},
	} {
		if got := rewriteDestPath(c.old, now); got != c.want {
			t.Errorf("rewriteDestPath(%q) = %q, want %q", c.old, got, c.want)
		}
	}
}

// keyedArchive writes rows as a keyed archive under a local directory and
// returns its glob path: the first keyed layout, one bucket width for every key,
// as an early export wrote it.
func keyedArchive(t *testing.T, rows []string) string {
	t.Helper()
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE logs (raw TEXT, ts TIMESTAMP, customer_id BIGINT, namespace TEXT, service TEXT, kind TEXT)"); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		parts := strings.Split(r, "|") // raw|age in days
		var age int
		fmt.Sscan(parts[1], &age)
		if _, err := db.Exec("INSERT INTO logs VALUES (?, ?, ?, 'ns', 'svc', 'main')", parts[0], janitorNow.Add(-days(age)), int64(payingCo)); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir() + "/sh-keyed.attempt-1"
	if err := os.MkdirAll(dir, 0o755); err != nil { // the export makes it too
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf(
		"COPY (SELECT *, time_bucket(INTERVAL '1440 minutes', ts, TIMESTAMP '1970-01-01') AS _part FROM logs) TO '%s' (FORMAT PARQUET, PARTITION_BY (customer_id, namespace, service, kind, _part), OVERWRITE_OR_IGNORE)",
		dir+"/k1440")); err != nil {
		t.Fatal(err)
	}
	return dir + "/k1440/**/*.parquet"
}

func TestExecutePurge_RewriteKeepsAKeyedArchiveKeyed(t *testing.T) {
	path := keyedArchive(t, []string{"old|60", "new|1"})
	stats := []shard.PartitionStat{{PartitionValues: []string{strconv.FormatInt(int64(payingCo), 10)}, MinTs: janitorNow.Add(-days(60)), MaxTs: janitorNow.Add(-days(1)), RowCount: 2}}
	ss, resolver, deps, retired := purgeFixture(t, "sh-keyed", path, stats)

	res, err := ss.ExecutePurge(PlannedShard{ShardID: "sh-keyed", ParquetPath: path, Disposition: DispositionRewrite}, resolver, deps)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeRewritten {
		t.Fatalf("outcome = %+v", res)
	}
	if !shard.IsSized(res.NewPath) {
		t.Fatalf("the rewrite of a keyed archive is %q, which is not keyed", res.NewPath)
	}
	rec, _, _ := ss.GetShard("sh-keyed")
	if rec.ParquetPath != res.NewPath {
		t.Errorf("parquet_path %q != the rewrite %q", rec.ParquetPath, res.NewPath)
	}
	if len(*retired) != 0 {
		t.Errorf("a rewrite that kept a row retired the shard: %v", *retired)
	}

	cold, err := shard.NewColdShard("v", nil, rec.ParquetPath, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cold.Close()
	rows, err := cold.Query(t.Context(), "SELECT raw, ts FROM logs WHERE customer_id = CAST(? AS BIGINT) AND service = ?", "900001", "svc")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Raw != "new" {
		t.Errorf("survivors = %v, want just the live row", rows)
	}
	// the old glob is what GC deletes: the deleter expands it
	var tombs []logdb.ParquetTombstone
	ss.DB().Where("shard_id = ?", "sh-keyed").Find(&tombs)
	if len(tombs) != 1 || tombs[0].Path != path {
		t.Errorf("tombstones = %+v, want one for %q", tombs, path)
	}
}

// Only the leader coordinator runs a pass. Two coordinators both running one
// would rewrite the same archives at once: double the Lambda work, two copies of
// each, and one copy orphaned by the later swap. A follower must not touch the
// store, so this janitor has none: reaching it would panic.
func TestJanitor_RunOnceDoesNothingOnAFollower(t *testing.T) {
	j := &Janitor{Clock: time.Now, IsLeader: func() bool { return false }}
	if err := j.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce on a follower: %v", err)
	}
}
