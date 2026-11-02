package logdb

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
	"github.com/graybeardcorp/weft/store"
)

// A keyed archive (docs/plans/per-service-tables.md, "Archive layout") must
// come out of a retention rewrite keyed, or the next pass undoes it.

func TestRewriteDestPath_KeepsTheLayout(t *testing.T) {
	now := time.Unix(1_700_000_000, 5)
	tag := fmt.Sprintf(".r%d", now.UnixNano())
	for _, c := range []struct {
		old, want string
		toKeyed   bool
	}{
		// a sized archive is rewritten as a sized one, with or without conversion on
		{"s3://b/40-apps/shard-1.attempt-1/sized/**/*.parquet", "s3://b/40-apps/shard-1.attempt-1" + tag + "/sized/**/*.parquet", false},
		{"s3://b/40-apps/shard-1.attempt-3.r17/sized/**/*.parquet", "s3://b/40-apps/shard-1.attempt-3.r17" + tag + "/sized/**/*.parquet", true},
		// the first keyed layout (one width for every key) is rewritten as a sized one
		{"s3://b/40-apps/shard-1.attempt-3/k7/**/*.parquet", "s3://b/40-apps/shard-1.attempt-3" + tag + "/sized/**/*.parquet", false},
		{"s3://b/40-apps/shard-1.attempt-1/k1440/**/*.parquet", "s3://b/40-apps/shard-1.attempt-1" + tag + "/sized/**/*.parquet", true},
		// the layouts that were already single-file rewrites are unchanged
		{"s3://b/40-apps/shard-1.attempt-1/m1440/**/*.parquet", "s3://b/40-apps/shard-1.attempt-1/m1440" + tag + ".parquet", false},
		{"s3://b/40-apps/shard-1.parquet", "s3://b/40-apps/shard-1" + tag + ".parquet", false},
		// converting: any other layout becomes a sized archive
		{"s3://b/40-apps/shard-1.attempt-1/m60/**/*.parquet", "s3://b/40-apps/shard-1.attempt-1" + tag + "/sized/**/*.parquet", true},
		{"s3://b/40-apps/shard-1.parquet", "s3://b/40-apps/shard-1" + tag + "/sized/**/*.parquet", true},
		{"s3://b/40-apps/shard-1.r1700.parquet", "s3://b/40-apps/shard-1.r1700" + tag + "/sized/**/*.parquet", true},
		{"s3://b/40-apps/shard-20261012.attempt-1/**/*.parquet", "s3://b/40-apps/shard-20261012.attempt-1" + tag + "/sized/**/*.parquet", true},
	} {
		if got := rewriteDestPath(c.old, now, c.toKeyed); got != c.want {
			t.Errorf("rewriteDestPath(%q, %v) = %q, want %q", c.old, c.toKeyed, got, c.want)
		}
	}
}

// keyedArchive writes rows as a keyed archive under a local directory and
// returns its glob path, the way the export would.
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
	stats := []shard.CustomerStat{{CustomerID: int64(payingCo), MinTs: janitorNow.Add(-days(60)), MaxTs: janitorNow.Add(-days(1)), RowCount: 2}}
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
	var tombs []ParquetTombstone
	ss.DB().Where("shard_id = ?", "sh-keyed").Find(&tombs)
	if len(tombs) != 1 || tombs[0].Path != path {
		t.Errorf("tombstones = %+v, want one for %q", tombs, path)
	}
}

// Only the leader coordinator runs a pass. Two coordinators both running one
// would convert the same archives at once: double the Lambda work, two copies of
// each, and one copy orphaned by the later swap. A follower must not touch the
// store, so this janitor has none: reaching it would panic.
func TestJanitor_RunOnceDoesNothingOnAFollower(t *testing.T) {
	j := &Janitor{Clock: time.Now, IsLeader: func() bool { return false }}
	if err := j.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce on a follower: %v", err)
	}
}

// Converting is opt-in: with it off a live archive is left alone, as ever.
// With it on, an archive that is not keyed and has nothing to delete is
// planned for conversion; a keyed one and an expired one are not.
func TestPlanRetention_ConvertsUnkeyedArchivesWhenAsked(t *testing.T) {
	db := openSMTestStore(t)
	bucket := seedBucket(t, db, BucketStasis)
	ss := ShardStore{db}
	ss.DB().Exec("DELETE FROM shard_records WHERE state = 'ARCHIVED'")

	paying := seedPayingCo(t, db)
	if err := db.CreateSubscription(&store.Subscription{
		ID: "sub-paying", OrgID: paying.ID, Tier: "swatch",
		Status: "active", StartsAt: janitorNow.Add(-days(60)), CreatedAt: janitorNow.Add(-days(60)),
	}); err != nil {
		t.Fatal(err)
	}
	seed := func(id, path string, minAge, maxAge int) {
		t.Helper()
		if err := ss.SaveShard(&Shard{
			ID: id, State: StateArchived, BucketID: bucket.ID, Tier: "small", ParquetPath: path,
			WindowStart: janitorNow.Add(-days(60)), WindowEnd: janitorNow.Add(-days(53)), CreatedAt: janitorNow.Add(-days(60)),
		}); err != nil {
			t.Fatal(err)
		}
		if err := ss.RecordArchiveStats(id, []shard.CustomerStat{{CustomerID: int64(payingCo),
			MinTs: janitorNow.Add(-days(minAge)), MaxTs: janitorNow.Add(-days(maxAge)), RowCount: 9}}); err != nil {
			t.Fatal(err)
		}
	}
	seed("live-single", "s3://b/x/live-single.parquet", 5, 1)
	seed("live-timed", "s3://b/x/live-timed.attempt-1/m1440/**/*.parquet", 5, 1)
	seed("live-k1440", "s3://b/x/live-k1440.attempt-1/k1440/**/*.parquet", 5, 1) // the first keyed layout
	seed("live-sized", "s3://b/x/live-sized.attempt-1/sized/**/*.parquet", 5, 1)
	seed("expired", "s3://b/x/expired.parquet", 60, 50)

	plan := func(convert bool) map[string]ShardDisposition {
		t.Helper()
		p, err := ss.PlanRetention(janitorNow, testGrace, convert)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]ShardDisposition{}
		for _, s := range p {
			got[s.ShardID] = s.Disposition
		}
		return got
	}
	if got := plan(false); len(got) != 1 || got["expired"] != DispositionDelete {
		t.Errorf("with conversion off the plan is %v, want only expired→delete", got)
	}
	got := plan(true)
	if len(got) != 4 || got["live-single"] != DispositionConvert || got["live-timed"] != DispositionConvert ||
		got["live-k1440"] != DispositionConvert || got["expired"] != DispositionDelete {
		t.Errorf("with conversion on the plan is %v, want live-single, live-timed and live-k1440 to convert, expired to delete, live-sized left alone", got)
	}
}

func TestExecutePurge_ConvertMakesAKeyedArchive(t *testing.T) {
	path, stats := archivedParquet(t, []*pb.LogRow{mkRow("one", 3), mkRow("two", 2), mkRow("three", 1)})
	ss, resolver, deps, retired := purgeFixture(t, "sh-conv", path, stats)

	res, err := ss.ExecutePurge(PlannedShard{ShardID: "sh-conv", ParquetPath: path, Disposition: DispositionConvert}, resolver, deps)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeRewritten || !shard.IsSized(res.NewPath) {
		t.Fatalf("outcome %+v: want a rewrite to a keyed path", res)
	}
	rec, _, _ := ss.GetShard("sh-conv")
	if rec.ParquetPath != res.NewPath {
		t.Errorf("parquet_path %q != %q", rec.ParquetPath, res.NewPath)
	}
	if len(*retired) != 0 {
		t.Errorf("a conversion retired the shard: %v", *retired)
	}
	cold, err := shard.NewColdShard("v", nil, rec.ParquetPath, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cold.Close()
	rows, err := cold.Query(t.Context(), "SELECT raw, ts FROM logs WHERE customer_id = CAST(? AS BIGINT)", "900001")
	if err != nil || len(rows) != 3 {
		t.Fatalf("converted archive returns %d rows (err %v), want all 3", len(rows), err)
	}
	var tombs []ParquetTombstone
	ss.DB().Where("shard_id = ?", "sh-conv").Find(&tombs)
	if len(tombs) != 1 || tombs[0].Path != path || tombs[0].Reason != "rewrite" {
		t.Errorf("tombstones = %+v, want the old archive for GC", tombs)
	}
}

// A converted copy that does not hold exactly the rows the archive recorded is
// never swapped in; what was written is tombstoned and the shard is untouched.
func TestExecutePurge_ConvertRefusesACopyThatLostRows(t *testing.T) {
	path, stats := archivedParquet(t, []*pb.LogRow{mkRow("one", 3), mkRow("two", 2)})
	stats[0].RowCount = 3 // the archive's recorded stats claim a row the file does not have
	ss, resolver, deps, _ := purgeFixture(t, "sh-lost", path, stats)

	_, err := ss.ExecutePurge(PlannedShard{ShardID: "sh-lost", ParquetPath: path, Disposition: DispositionConvert}, resolver, deps)
	if err == nil {
		t.Fatal("a conversion whose row count disagrees with the archive's stats succeeded")
	}
	rec, _, _ := ss.GetShard("sh-lost")
	if rec.ParquetPath != path {
		t.Errorf("parquet_path moved to %q", rec.ParquetPath)
	}
	var tombs []ParquetTombstone
	ss.DB().Where("shard_id = ?", "sh-lost").Find(&tombs)
	if len(tombs) != 1 || tombs[0].Reason != "convert-failed" || tombs[0].Path == path {
		t.Errorf("tombstones = %+v, want the written copy, not the archive", tombs)
	}
}

// With conversion on, a retention rewrite of an unkeyed archive comes out keyed.
func TestExecutePurge_RewriteConvertsWhenAsked(t *testing.T) {
	path, stats := archivedParquet(t, []*pb.LogRow{mkRow("old", 60), mkRow("new", 1)})
	ss, resolver, deps, _ := purgeFixture(t, "sh-rwconv", path, stats)
	deps.ConvertToKeyed = true

	res, err := ss.ExecutePurge(PlannedShard{ShardID: "sh-rwconv", ParquetPath: path, Disposition: DispositionRewrite}, resolver, deps)
	if err != nil {
		t.Fatal(err)
	}
	if !shard.IsSized(res.NewPath) {
		t.Errorf("the rewrite is %q, not a keyed archive", res.NewPath)
	}
}
