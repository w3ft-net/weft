//go:build test

package logdb

import (
	"testing"
	"time"

	"github.com/graybeardcorp/weft/store"
)

func exportFactsFixture(t *testing.T) (ShardStore, *store.Store) {
	t.Helper()
	_, db, _ := makeFixtureShardSM(t, StateArchiving)
	return ShardStore{db}, db
}

func getShardRow(t *testing.T, ss ShardStore) *Shard {
	t.Helper()
	sh, ok, err := ss.GetShard("s-test")
	if err != nil || !ok {
		t.Fatalf("GetShard: ok=%v err=%v", ok, err)
	}
	return sh
}

func TestShardStore_BeginExportAttemptIsACompareAndSet(t *testing.T) {
	ss, _ := exportFactsFixture(t)
	started := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	won, err := ss.BeginExportAttempt("s-test", 0, "r7g.large", started)
	if err != nil || !won {
		t.Fatalf("first claim: won=%v err=%v", won, err)
	}
	sh := getShardRow(t, ss)
	if sh.ExportAttempt != 1 || sh.ExportInstanceType != "r7g.large" || sh.ExportStartedAt == nil || !sh.ExportStartedAt.Equal(started) {
		t.Fatalf("claim not recorded: %+v", sh)
	}

	// A second coordinator that read attempt=0 before the first claim lost the
	// race: it must not start attempt 1 again.
	won, err = ss.BeginExportAttempt("s-test", 0, "r7g.2xlarge", started)
	if err != nil {
		t.Fatal(err)
	}
	if won {
		t.Fatal("a stale claim (attempt 0) won after attempt 1 was already begun")
	}
	if got := getShardRow(t, ss); got.ExportAttempt != 1 || got.ExportInstanceType != "r7g.large" {
		t.Fatalf("losing claim changed the row: %+v", got)
	}

	// The next legitimate attempt claims from the current value.
	if won, err = ss.BeginExportAttempt("s-test", 1, "r7g.xlarge", started); err != nil || !won {
		t.Fatalf("attempt 2: won=%v err=%v", won, err)
	}
}

func TestShardStore_ExportFactsAreNarrowWrites(t *testing.T) {
	ss, _ := exportFactsFixture(t)
	if err := ss.SetShardLaunchResult("s-test", "i-shard", "us-east-2a", "vol-1", nil); err != nil {
		t.Fatal(err)
	}
	if err := ss.SetShardParquetPath("s-test", "s3://x"); err != nil {
		t.Fatal(err)
	}

	if err := ss.SetExportSnapshot("s-test", "snap-1"); err != nil {
		t.Fatal(err)
	}
	if err := ss.SetExportInstance("s-test", "i-export"); err != nil {
		t.Fatal(err)
	}
	got := getShardRow(t, ss)
	if got.ExportSnapshotID != "snap-1" || got.ExportInstanceID != "i-export" {
		t.Fatalf("facts not written: %+v", got)
	}
	if got.InstanceID != "i-shard" || got.EBSVolumeID != "vol-1" || got.ParquetPath != "s3://x" || got.State != StateArchiving {
		t.Fatalf("an export write trampled an unrelated column: %+v", got)
	}
}

func TestShardStore_ClearExportFactsKeepsTheAttemptCounter(t *testing.T) {
	ss, _ := exportFactsFixture(t)
	started := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	if _, err := ss.BeginExportAttempt("s-test", 0, "r7g.large", started); err != nil {
		t.Fatal(err)
	}
	_ = ss.SetExportSnapshot("s-test", "snap-1")
	_ = ss.SetExportInstance("s-test", "i-export")

	if err := ss.ClearExportFacts("s-test"); err != nil {
		t.Fatal(err)
	}
	got := getShardRow(t, ss)
	if got.ExportSnapshotID != "" || got.ExportInstanceID != "" || got.ExportInstanceType != "" || got.ExportStartedAt != nil {
		t.Fatalf("facts not cleared: %+v", got)
	}
	// The counter is what caps retries and keeps the next attempt's tags
	// distinct from the last; clearing the facts must not reset it.
	if got.ExportAttempt != 1 {
		t.Fatalf("attempt counter = %d, want 1 (cleared with the facts)", got.ExportAttempt)
	}
}

func TestShardStore_CountActiveExports(t *testing.T) {
	ss, db := exportFactsFixture(t)
	if n, err := ss.CountActiveExports(); err != nil || n != 0 {
		t.Fatalf("no exports: n=%d err=%v", n, err)
	}
	if err := ss.SetExportInstance("s-test", "i-export"); err != nil {
		t.Fatal(err)
	}
	if n, _ := ss.CountActiveExports(); n != 1 {
		t.Fatalf("one export instance: n=%d", n)
	}
	// A snapshot alone is not a running instance: it does not count against the
	// instance cap.
	bucket := seedBucket(t, db, BucketStasis)
	if err := ss.SaveShard(&Shard{
		ID: "s-other", State: StateArchiving, BucketID: bucket.ID, Tier: "small",
		WindowStart: time.Now(), WindowEnd: time.Now(), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := ss.SetExportSnapshot("s-other", "snap-9"); err != nil {
		t.Fatal(err)
	}
	if n, _ := ss.CountActiveExports(); n != 1 {
		t.Fatalf("a snapshot-only shard counted: n=%d", n)
	}
}

// SaveShard upserts the whole row from persistedCopy. If the copy omitted the
// export facts, any registry persist() — which fires on ordinary state changes —
// would write them back empty, mid-export.
func TestShard_PersistedCopyCarriesExportFacts(t *testing.T) {
	ss, _ := exportFactsFixture(t)
	started := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	entry := getShardRow(t, ss)
	entry.ExportSnapshotID, entry.ExportInstanceID = "snap-1", "i-export"
	entry.ExportInstanceType, entry.ExportAttempt, entry.ExportStartedAt = "r7g.large", 2, &started

	cp := entry.persistedCopy()
	if cp.ExportStartedAt == entry.ExportStartedAt {
		t.Fatal("persistedCopy aliases the live entry's ExportStartedAt pointer")
	}
	if err := ss.SaveShard(cp); err != nil {
		t.Fatal(err)
	}
	got := getShardRow(t, ss)
	if got.ExportSnapshotID != "snap-1" || got.ExportInstanceID != "i-export" || got.ExportInstanceType != "r7g.large" ||
		got.ExportAttempt != 2 || got.ExportStartedAt == nil || !got.ExportStartedAt.Equal(started) {
		t.Fatalf("export facts lost through persistedCopy+SaveShard: %+v", got)
	}
}

// A new leader's entry must see the previous leader's in-flight attempt, or it
// would start a second one.
func TestRefreshFromStore_SyncsExportFacts(t *testing.T) {
	ss, db := exportFactsFixture(t)
	reg := NewShardRegistry()
	reg.SetStore(ss)
	if err := reg.LoadFromStore(noReconstruct); err != nil {
		t.Fatal(err)
	}
	entry := reg.Get("s-test")
	if entry == nil {
		t.Fatal("shard missing after LoadFromStore")
	}

	started := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	if _, err := ss.BeginExportAttempt("s-test", 0, "r7g.xlarge", started); err != nil {
		t.Fatal(err)
	}
	_ = ss.SetExportSnapshot("s-test", "snap-7")
	_ = ss.SetExportInstance("s-test", "i-exp-7")
	_ = db

	reg.RefreshFromStore()

	entry.mu.RLock()
	defer entry.mu.RUnlock()
	if entry.ExportAttempt != 1 || entry.ExportSnapshotID != "snap-7" || entry.ExportInstanceID != "i-exp-7" ||
		entry.ExportInstanceType != "r7g.xlarge" || entry.ExportStartedAt == nil {
		t.Fatalf("export facts not refreshed: attempt=%d snap=%q inst=%q type=%q started=%v",
			entry.ExportAttempt, entry.ExportSnapshotID, entry.ExportInstanceID, entry.ExportInstanceType, entry.ExportStartedAt)
	}
}

func TestShardStore_LatestShardDiskBytes(t *testing.T) {
	ss, db := exportFactsFixture(t)
	if n, err := ss.LatestShardDiskBytes("s-test"); err != nil || n != 0 {
		t.Fatalf("no heartbeats: n=%d err=%v, want 0 and no error", n, err)
	}
	// Explicit beat_at: InsertHeartbeat stamps NOW(), which is constant within
	// the test's transaction and would collide on the primary key.
	now := time.Now()
	for i, disk := range []int64{100, 300} {
		beatAt := now.Add(time.Duration(i) * time.Minute)
		if err := db.DB().Exec(`
			INSERT INTO shard_heartbeats (shard_id, addr, instance_id, state, row_count, disk_bytes, beat_at)
			VALUES (?, '', '', 'READ_ONLY', 0, ?, ?)`, "s-test", disk, beatAt).Error; err != nil {
			t.Fatal(err)
		}
	}
	if n, err := ss.LatestShardDiskBytes("s-test"); err != nil || n != 300 {
		t.Fatalf("latest disk bytes = %d err=%v, want 300 (the newest heartbeat)", n, err)
	}
}
