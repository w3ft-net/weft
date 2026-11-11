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

// TestJanitor_RunOnce drives a full pass over three archived shards — one
// to rewrite, one to delete, one to keep — and asserts each disposition
// took its action through the injected collaborators.
func TestJanitor_RunOnce(t *testing.T) {
	db := openTestStore(t)
	ss := newStore(db)
	// PlanRetention scans every ARCHIVED shard. Scope this transaction's
	// view to our own seeds — other tests commit archived rows (pointing at
	// now-deleted temp parquet) into the shared pool DB. The deletes roll
	// back with the test transaction.
	ss.DB().Exec("DELETE FROM shard_records WHERE state = 'ARCHIVED'")
	ss.DB().Exec("DELETE FROM parquet_tombstones")
	bucket := seedBucket(t, db)

	cust := seedPayingCo(t, db)
	if err := db.CreateSubscription(&store.Subscription{
		ID: "sub", OrgID: cust.ID, Tier: "swatch", Status: "active",
		StartsAt: janitorNow.Add(-days(90)), CreatedAt: janitorNow.Add(-days(90)),
	}); err != nil {
		t.Fatal(err)
	}

	seed := func(id string, rows []*pb.LogRow) {
		t.Helper()
		path, stats := archivedParquet(t, rows)
		if err := ss.SaveShard(&logdb.Shard{
			ID: id, State: logdb.StateArchived, BucketID: bucket.ID, Tier: "small",
			ParquetPath: path,
			WindowStart: janitorNow.Add(-days(60)), WindowEnd: janitorNow.Add(-days(53)),
			CreatedAt: janitorNow.Add(-days(60)),
		}); err != nil {
			t.Fatal(err)
		}
		if err := ss.RecordArchiveStats(id, stats); err != nil {
			t.Fatal(err)
		}
	}
	seed("rw", []*pb.LogRow{mkRow("old", 60), mkRow("new", 1)}) // straddle → rewrite
	seed("del", []*pb.LogRow{mkRow("o1", 60), mkRow("o2", 55)}) // all expired → delete
	seed("keep", []*pb.LogRow{mkRow("n1", 2), mkRow("n2", 1)})  // all live → keep

	var retired []string
	refreshed := map[string]string{}
	j := &Janitor{
		Store:     ss,
		Compactor: shard.LocalCompactor{},
		Retire:    func(id string) error { retired = append(retired, id); return nil },
		Refresh:   func(id, p string) { refreshed[id] = p },
		Grace:     testGrace,
		GCDelay:   time.Hour,
		Clock:     func() time.Time { return janitorNow },
		Interval:  time.Hour,
	}
	if err := j.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// del → retired.
	if len(retired) != 1 || retired[0] != "del" {
		t.Errorf("retired = %v, want [del]", retired)
	}
	// rw → swapped + refresh notified with the new path.
	rec, _, _ := ss.GetShard("rw")
	if refreshed["rw"] == "" || refreshed["rw"] != rec.ParquetPath {
		t.Errorf("refresh[rw] = %q, want the new parquet_path %q", refreshed["rw"], rec.ParquetPath)
	}
	if _, ok := refreshed["keep"]; ok {
		t.Errorf("keep shard should not be refreshed")
	}
	// keep → untouched.
	keepRec, _, _ := ss.GetShard("keep")
	if keepRec.LastPurgedAt != nil {
		t.Errorf("keep shard was touched: last_purged_at = %v", keepRec.LastPurgedAt)
	}
}
