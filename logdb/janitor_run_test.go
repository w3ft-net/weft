package logdb

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
	"github.com/graybeardcorp/weft/store"
)

type fakeDeleter struct{ deleted []string }

func (f *fakeDeleter) DeleteParquet(p string) error {
	f.deleted = append(f.deleted, p)
	return nil
}

func TestSweepTombstones(t *testing.T) {
	db := openSMTestStore(t)
	ss := ShardStore{db}
	ss.DB().Exec("DELETE FROM parquet_tombstones") // ignore pool residue from other tests

	due := filepath.Join(t.TempDir(), "old.parquet")
	if err := os.WriteFile(due, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ss.recordTombstone(due, "sh-x", "rewrite", janitorNow.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	// A not-yet-due tombstone must be left alone.
	if err := ss.recordTombstone("s3://b/future.parquet", "sh-y", "rewrite", janitorNow.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	n, err := ss.SweepTombstones(janitorNow, LocalFileDeleter{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept %d, want 1 (only the due tombstone)", n)
	}
	if _, err := os.Stat(due); !os.IsNotExist(err) {
		t.Errorf("due object not deleted: %v", err)
	}
	var remaining []ParquetTombstone
	ss.DB().Find(&remaining)
	if len(remaining) != 1 || remaining[0].ShardID != "sh-y" {
		t.Errorf("remaining tombstones = %+v, want only the future sh-y", remaining)
	}
}

// TestJanitor_RunOnce drives a full pass over three archived shards — one
// to rewrite, one to delete, one to keep — and asserts each disposition
// took its action through the injected collaborators.
func TestJanitor_RunOnce(t *testing.T) {
	db := openSMTestStore(t)
	ss := ShardStore{db}
	// PlanRetention scans every ARCHIVED shard. Scope this transaction's
	// view to our own seeds — other tests commit archived rows (pointing at
	// now-deleted temp parquet) into the shared pool DB. The deletes roll
	// back with the test transaction.
	ss.DB().Exec("DELETE FROM shard_records WHERE state = 'ARCHIVED'")
	ss.DB().Exec("DELETE FROM parquet_tombstones")
	bucket := seedBucket(t, db, BucketStasis)

	cust, err := db.FindOrCreateOrg("paying-co")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSubscription(&store.Subscription{
		ID: "sub", OrgID: cust.ID, Tier: "swatch", Status: "active",
		StartsAt: janitorNow.Add(-days(90)), CreatedAt: janitorNow.Add(-days(90)),
	}); err != nil {
		t.Fatal(err)
	}

	seed := func(id string, rows []*pb.LogRow) {
		t.Helper()
		path, stats := archivedParquet(t, rows)
		if err := ss.SaveShard(&Shard{
			ID: id, State: StateArchived, BucketID: bucket.ID, Tier: "small",
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
	deleter := &fakeDeleter{}
	j := &Janitor{
		Store:     ss,
		Compactor: shard.LocalCompactor{},
		Deleter:   deleter,
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
	// Tombstones from this pass are future-dated, so the GC sweep deletes
	// nothing yet.
	if len(deleter.deleted) != 0 {
		t.Errorf("GC deleted %v, want nothing (tombstones still within grace)", deleter.deleted)
	}
}
