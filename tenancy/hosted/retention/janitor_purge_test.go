package retention

import (
	"context"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/logdb"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
	"github.com/graybeardcorp/weft/store"
)

// archivedParquet ingests rows and archives them to a local temp dir,
// returning the parquet path and the per-customer stats Archive captured.
func archivedParquet(t *testing.T, rows []*pb.LogRow) (string, []shard.PartitionStat) {
	t.Helper()
	s, err := shard.NewDuckDBShard("purge-fix", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Ingest(context.Background(), rows, nil); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	path, stats, err := s.Archive(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	return path, stats
}

// payingCo is the fixture customer's org id. It is explicit (and far above
// any sequence-assigned id) so the parquet rows, which are written before the
// database fixture exists, can carry the same customer_id the org will have.
const payingCo store.OrgID = 900001

// seedPayingCo creates the "paying-co" org with the fixed id payingCo.
func seedPayingCo(t *testing.T, db *store.Store) *store.Organization {
	t.Helper()
	cust := &store.Organization{ID: payingCo, Name: "paying-co"}
	if err := db.DB().Create(cust).Error; err != nil {
		t.Fatalf("customer: %v", err)
	}
	return cust
}

// purgeFixture seeds a swatch customer and an ARCHIVED shard pointing at
// path, optionally recording its archive stats. Returns the bound store,
// the resolver, and PurgeDeps with a fake Retire that records calls.
func purgeFixture(t *testing.T, shardID, path string, stats []shard.PartitionStat) (Store, *RetentionResolver, PurgeDeps, *[]string) {
	t.Helper()
	db := openTestStore(t)
	ss := newStore(db)
	bucket := seedBucket(t, db)

	cust := seedPayingCo(t, db)
	if err := db.CreateSubscription(&store.Subscription{
		ID: "sub-" + shardID, OrgID: cust.ID, Tier: "swatch",
		Status: "active", StartsAt: janitorNow.Add(-days(90)), CreatedAt: janitorNow.Add(-days(90)),
	}); err != nil {
		t.Fatalf("subscription: %v", err)
	}
	if err := ss.SaveShard(&logdb.Shard{
		ID: shardID, State: logdb.StateArchived, BucketID: bucket.ID, Tier: "small",
		ParquetPath: path,
		WindowStart: janitorNow.Add(-days(60)), WindowEnd: janitorNow.Add(-days(53)),
		CreatedAt: janitorNow.Add(-days(60)),
	}); err != nil {
		t.Fatalf("seed shard: %v", err)
	}
	if stats != nil {
		if err := ss.RecordArchiveStats(shardID, stats); err != nil {
			t.Fatalf("seed stats: %v", err)
		}
	}

	tiers, err := db.ActiveCustomerTiers()
	if err != nil {
		t.Fatalf("tiers: %v", err)
	}
	var retired []string
	deps := PurgeDeps{
		Compactor: LocalCompactor{}, // local parquet, no S3
		Grace:     testGrace,
		GCDelay:   time.Hour,
		Now:       func() time.Time { return janitorNow },
		Retire:    func(id string) error { retired = append(retired, id); return nil },
	}
	return ss, NewRetentionResolver(tiers), deps, &retired
}

func mkRow(raw string, age int) *pb.LogRow {
	return &pb.LogRow{Raw: raw, CustomerId: int64(payingCo), TsUnixNs: janitorNow.Add(-days(age)).UnixNano()}
}

func TestExecutePurge_Rewrite(t *testing.T) {
	// One expired row (60d) + one live row (1d) → straddles cutoff → Rewrite.
	path, stats := archivedParquet(t, []*pb.LogRow{mkRow("old", 60), mkRow("new", 1)})
	ss, resolver, deps, retired := purgeFixture(t, "sh-rw", path, stats)

	res, err := ss.ExecutePurge(PlannedShard{ShardID: "sh-rw", ParquetPath: path, Disposition: DispositionRewrite}, resolver, deps)
	if err != nil {
		t.Fatalf("ExecutePurge: %v", err)
	}
	if res.Outcome != OutcomeRewritten || res.NewPath == "" {
		t.Fatalf("outcome = %+v, want rewritten with a new path", res)
	}

	rec, _, _ := ss.GetShard("sh-rw")
	if rec.ParquetPath != res.NewPath {
		t.Fatalf("parquet_path %q != reported NewPath %q", rec.ParquetPath, res.NewPath)
	}
	if rec.ParquetPath == path || rec.ParquetPath == "" {
		t.Fatalf("parquet_path not swapped: %q", rec.ParquetPath)
	}
	if rec.LastPurgedAt == nil {
		t.Error("last_purged_at not set")
	}
	if len(*retired) != 0 {
		t.Errorf("Retire should not fire on a non-empty rewrite, got %v", *retired)
	}
	// Survivors: only the live row remains in the new parquet.
	cold, err := shard.NewColdShard("v", nil, rec.ParquetPath, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cold.Close()
	rows, err := cold.Query(context.Background(), "SELECT raw, ts FROM logs WHERE customer_id = 900001")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("survivors = %d, want 1 (only the live row)", len(rows))
	}
	// Refreshed stats reflect the survivor count.
	fresh, _ := ss.shardArchiveStats("sh-rw")
	if len(fresh) != 1 || fresh[0].RowCount != 1 {
		t.Errorf("refreshed stats = %+v, want one row with count 1", fresh)
	}
	// Old object tombstoned for GC.
	var tombs []logdb.ParquetTombstone
	db := ss.DB()
	db.Where("shard_id = ?", "sh-rw").Find(&tombs)
	if len(tombs) != 1 || tombs[0].Path != path || tombs[0].Reason != "rewrite" {
		t.Errorf("tombstone = %+v, want one rewrite tombstone for %q", tombs, path)
	}
	if !tombs[0].DeleteAfter.After(janitorNow) {
		t.Errorf("delete_after %v should be after now %v", tombs[0].DeleteAfter, janitorNow)
	}
}

func TestExecutePurge_Delete(t *testing.T) {
	// Both rows expired → Delete: tombstone the whole object, fire Retire.
	path, stats := archivedParquet(t, []*pb.LogRow{mkRow("old1", 60), mkRow("old2", 55)})
	ss, resolver, deps, retired := purgeFixture(t, "sh-del", path, stats)

	res, err := ss.ExecutePurge(PlannedShard{ShardID: "sh-del", ParquetPath: path, Disposition: DispositionDelete}, resolver, deps)
	if err != nil {
		t.Fatalf("ExecutePurge: %v", err)
	}
	if res.Outcome != OutcomeRetired {
		t.Fatalf("outcome = %+v, want retired", res)
	}

	if len(*retired) != 1 || (*retired)[0] != "sh-del" {
		t.Errorf("Retire calls = %v, want [sh-del]", *retired)
	}
	var tombs []logdb.ParquetTombstone
	ss.DB().Where("shard_id = ?", "sh-del").Find(&tombs)
	if len(tombs) != 1 || tombs[0].Reason != "retention-delete" || tombs[0].Path != path {
		t.Errorf("tombstone = %+v, want one retention-delete for %q", tombs, path)
	}
}

func TestExecutePurge_UnknownBackfillsStats(t *testing.T) {
	// Archived shard with NO stats seeded → Unknown → backfill from the file.
	path, _ := archivedParquet(t, []*pb.LogRow{mkRow("a", 60), mkRow("b", 1)})
	ss, resolver, deps, retired := purgeFixture(t, "sh-unk", path, nil)

	res, err := ss.ExecutePurge(PlannedShard{ShardID: "sh-unk", ParquetPath: path, Disposition: DispositionUnknown}, resolver, deps)
	if err != nil {
		t.Fatalf("ExecutePurge: %v", err)
	}
	if res.Outcome != OutcomeStatsBackfilled {
		t.Fatalf("outcome = %+v, want stats-backfilled", res)
	}

	stats, _ := ss.shardArchiveStats("sh-unk")
	if len(stats) != 1 || stats[0].OrgID != payingCo || stats[0].RowCount != 2 {
		t.Errorf("backfilled stats = %+v, want paying-co/2", stats)
	}
	if len(*retired) != 0 {
		t.Errorf("Unknown should not retire, got %v", *retired)
	}
	rec, _, _ := ss.GetShard("sh-unk")
	if rec.ParquetPath != path {
		t.Errorf("Unknown should not change parquet_path: %q", rec.ParquetPath)
	}
	if rec.LastPurgedAt == nil {
		t.Error("last_purged_at not set on backfill")
	}
}
