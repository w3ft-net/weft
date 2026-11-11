package logdb

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/shard"
	"github.com/graybeardcorp/weft/store"
)

// swapNow is a fixed instant for the tombstone and swap tests.
var swapNow = time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)

// archivedShard seeds an ARCHIVED shard whose parquet is at path.
func archivedShard(t *testing.T, shardID, path string) ShardStore {
	t.Helper()
	db := openSMTestStore(t)
	ss := ShardStore{db}
	bucket := seedBucket(t, db, BucketStasis)
	if err := ss.SaveShard(&Shard{
		ID: shardID, State: StateArchived, BucketID: bucket.ID, Tier: "small",
		ParquetPath: path,
		WindowStart: swapNow.Add(-60 * 24 * time.Hour), WindowEnd: swapNow.Add(-53 * 24 * time.Hour),
		CreatedAt: swapNow.Add(-60 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("seed shard: %v", err)
	}
	return ss
}

type fakeDeleter struct{ deleted []string }

func (f *fakeDeleter) DeleteParquet(p string) error {
	f.deleted = append(f.deleted, p)
	return nil
}

func tombstone(t *testing.T, db *store.Store, path, shardID string, after time.Time) {
	t.Helper()
	if err := db.DB().Create(&ParquetTombstone{Path: path, ShardID: shardID, Reason: "swap", DeleteAfter: after}).Error; err != nil {
		t.Fatal(err)
	}
}

func TestSweepTombstones(t *testing.T) {
	db := openSMTestStore(t)
	ss := ShardStore{db}
	ss.DB().Exec("DELETE FROM parquet_tombstones") // ignore pool residue from other tests

	due := filepath.Join(t.TempDir(), "old.parquet")
	if err := os.WriteFile(due, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	tombstone(t, db, due, "sh-x", swapNow.Add(-time.Minute))
	// A not-yet-due tombstone must be left alone.
	tombstone(t, db, "s3://b/future.parquet", "sh-y", swapNow.Add(time.Hour))

	n, err := ss.SweepTombstones(swapNow, LocalFileDeleter{})
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

// The GC is core: with no retention feature installed, a swap's tombstone is
// still deleted once due.
func TestTombstoneSweeper_DeletesWhatIsDueWithNoRetentionInstalled(t *testing.T) {
	db := openSMTestStore(t)
	ss := ShardStore{db}
	ss.DB().Exec("DELETE FROM parquet_tombstones")
	tombstone(t, db, "s3://b/due.parquet", "sh-due", swapNow.Add(-time.Minute))
	tombstone(t, db, "s3://b/later.parquet", "sh-later", swapNow.Add(time.Hour))

	del := &fakeDeleter{}
	(&TombstoneSweeper{Store: ss, Deleter: del, Clock: func() time.Time { return swapNow }}).RunOnce()
	if len(del.deleted) != 1 || del.deleted[0] != "s3://b/due.parquet" {
		t.Errorf("deleted %v, want only the due object", del.deleted)
	}

	// a follower does not sweep
	del2 := &fakeDeleter{}
	(&TombstoneSweeper{Store: ss, Deleter: del2, Clock: func() time.Time { return swapNow.Add(2 * time.Hour) }, IsLeader: func() bool { return false }}).RunOnce()
	if len(del2.deleted) != 0 {
		t.Errorf("a follower deleted %v", del2.deleted)
	}
}

// recordingRetention is a Retention that remembers what it was told.
type recordingRetention struct {
	shardID string
	stats   []shard.CustomerStat
}

func (r *recordingRetention) ArchiveRecorded(id string, stats []shard.CustomerStat) error {
	r.shardID, r.stats = id, stats
	return nil
}

func (r *recordingRetention) Start(context.Context, *Provisioner) {}
