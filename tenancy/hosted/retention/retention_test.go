package retention

import (
	"testing"
	"time"

	"github.com/graybeardcorp/weft/logdb"
	"github.com/graybeardcorp/weft/shard"
)

// A partition stat is a customer's when it has one numeric value. One with no
// values is the group an older shard counted under customer 0; anything else is
// not hosted's, and is left out rather than recorded as someone.
func TestArchiveStatRows_ReadOnlyWhatIsACustomers(t *testing.T) {
	at := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	rows := archiveStatRows("sh", []shard.PartitionStat{
		{PartitionValues: []string{"5"}, MinTs: at.Add(time.Hour), MaxTs: at.Add(2 * time.Hour), RowCount: 7},
		{MinTs: at, MaxTs: at, RowCount: 2}, // an older shard's customer 0
		{PartitionValues: []string{"gold"}, MinTs: at, MaxTs: at, RowCount: 1},
		{PartitionValues: []string{"5", "x"}, MinTs: at, MaxTs: at, RowCount: 1},
		{PartitionValues: []string{"5"}, MinTs: at, MaxTs: at.Add(3 * time.Hour), RowCount: 1}, // the same customer again
	})
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want customers 5 and 0", rows)
	}
	five, zero := rows[0], rows[1]
	if five.OrgID != 5 || five.RowCount != 8 || !five.MinTs.Equal(at) || !five.MaxTs.Equal(at.Add(3*time.Hour)) {
		t.Errorf("customer 5 = %+v, want its two groups folded: 8 rows over [%v, %v]", five, at, at.Add(3*time.Hour))
	}
	if zero.OrgID != 0 || zero.RowCount != 2 {
		t.Errorf("customer 0 = %+v", zero)
	}
}

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
	if err := r.ArchiveRecorded("sh-r", []shard.PartitionStat{{PartitionValues: []string{"5"}, MinTs: at, MaxTs: at, RowCount: 7}, {PartitionValues: []string{"6"}, MinTs: at, MaxTs: at, RowCount: 3}}); err != nil {
		t.Fatal(err)
	}
	if err := r.ArchiveRecorded("sh-r", []shard.PartitionStat{{PartitionValues: []string{"6"}, MinTs: at, MaxTs: at, RowCount: 4}}); err != nil {
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
