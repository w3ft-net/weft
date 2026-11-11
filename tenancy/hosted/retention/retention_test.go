package retention

import (
	"testing"
	"time"

	"github.com/graybeardcorp/weft/logdb"
	"github.com/graybeardcorp/weft/shard"
)

// ArchiveRecorded replaces a shard's rows rather than adding to them, so a
// re-archive leaves no stale per-customer rows.
func TestArchiveRecordedReplacesTheShardsRows(t *testing.T) {
	db := openTestStore(t)
	bucket := seedBucket(t, db)
	if err := newStore(db).SaveShard(&logdb.Shard{ID: "sh-r", State: logdb.StateArchived, BucketID: bucket.ID, Tier: "small"}); err != nil {
		t.Fatal(err)
	}
	r := New(db, "")
	at := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	if err := r.ArchiveRecorded("sh-r", []shard.CustomerStat{{CustomerID: 5, MinTs: at, MaxTs: at, RowCount: 7}, {CustomerID: 6, MinTs: at, MaxTs: at, RowCount: 3}}); err != nil {
		t.Fatal(err)
	}
	if err := r.ArchiveRecorded("sh-r", []shard.CustomerStat{{CustomerID: 6, MinTs: at, MaxTs: at, RowCount: 4}}); err != nil {
		t.Fatal(err)
	}
	var rows []ShardArchiveStat
	if err := db.DB().Where("shard_id = ?", "sh-r").Order("org_id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].OrgID != 6 || rows[0].RowCount != 4 {
		t.Fatalf("rows = %+v, want only customer 6 with 4 rows", rows)
	}
}
