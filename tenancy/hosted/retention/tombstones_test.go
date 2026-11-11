package retention

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/logdb"
	"github.com/graybeardcorp/weft/store"
)

var sweepNow = time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)

type fakeDeleter struct{ deleted []string }

func (f *fakeDeleter) DeleteParquet(p string) error {
	f.deleted = append(f.deleted, p)
	return nil
}

func tombstone(t *testing.T, db *store.Store, path, shardID string, after time.Time) {
	t.Helper()
	if err := db.DB().Create(&ParquetTombstone{Path: path, ShardID: shardID, Reason: "rewrite", DeleteAfter: after}).Error; err != nil {
		t.Fatal(err)
	}
}

func TestSweepTombstones(t *testing.T) {
	db := openTestStore(t)
	s := newStore(db)
	s.DB().Exec("DELETE FROM parquet_tombstones") // ignore pool residue from other tests

	due := filepath.Join(t.TempDir(), "old.parquet")
	if err := os.WriteFile(due, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	tombstone(t, db, due, "sh-x", sweepNow.Add(-time.Minute))
	// A not-yet-due tombstone must be left alone.
	tombstone(t, db, "s3://b/future.parquet", "sh-y", sweepNow.Add(time.Hour))

	n, err := s.SweepTombstones(sweepNow, logdb.LocalFileDeleter{})
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
	s.DB().Find(&remaining)
	if len(remaining) != 1 || remaining[0].ShardID != "sh-y" {
		t.Errorf("remaining tombstones = %+v, want only the future sh-y", remaining)
	}
}

func TestTombstoneSweeper_DeletesWhatIsDueAndOnlyOnTheLeader(t *testing.T) {
	db := openTestStore(t)
	s := newStore(db)
	s.DB().Exec("DELETE FROM parquet_tombstones")
	tombstone(t, db, "s3://b/due.parquet", "sh-due", sweepNow.Add(-time.Minute))
	tombstone(t, db, "s3://b/later.parquet", "sh-later", sweepNow.Add(time.Hour))

	del := &fakeDeleter{}
	(&TombstoneSweeper{Store: s, Deleter: del, Clock: func() time.Time { return sweepNow }}).RunOnce()
	if len(del.deleted) != 1 || del.deleted[0] != "s3://b/due.parquet" {
		t.Errorf("deleted %v, want only the due object", del.deleted)
	}

	// a follower does not sweep
	del2 := &fakeDeleter{}
	(&TombstoneSweeper{Store: s, Deleter: del2, Clock: func() time.Time { return sweepNow.Add(2 * time.Hour) }, IsLeader: func() bool { return false }}).RunOnce()
	if len(del2.deleted) != 0 {
		t.Errorf("a follower deleted %v", del2.deleted)
	}
}
