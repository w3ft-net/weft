//go:build test

package logdb

import (
	"fmt"
	"sync"
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

	claim, err := ss.BeginExportAttempt("s-test", 0, "r7g.large", started, 5)
	if err != nil || claim != ExportClaimWon {
		t.Fatalf("first claim: %v err=%v", claim, err)
	}
	sh := getShardRow(t, ss)
	if sh.ExportAttempt != 1 || sh.ExportInstanceType != "r7g.large" || sh.ExportStartedAt == nil || !sh.ExportStartedAt.Equal(started) {
		t.Fatalf("claim not recorded: %+v", sh)
	}

	// A second coordinator that read attempt=0 before the first claim lost the
	// race: it must not start attempt 1 again.
	claim, err = ss.BeginExportAttempt("s-test", 0, "r7g.2xlarge", started, 5)
	if err != nil {
		t.Fatal(err)
	}
	if claim != ExportClaimLost {
		t.Fatalf("a stale claim (attempt 0) = %v after attempt 1 was already begun, want lost", claim)
	}
	if got := getShardRow(t, ss); got.ExportAttempt != 1 || got.ExportInstanceType != "r7g.large" {
		t.Fatalf("losing claim changed the row: %+v", got)
	}

	// The next legitimate attempt claims from the current value (the facts of the
	// failed attempt are cleared first, as the FSM does).
	if err := ss.ClearExportFacts("s-test"); err != nil {
		t.Fatal(err)
	}
	if claim, err = ss.BeginExportAttempt("s-test", 1, "r7g.xlarge", started, 5); err != nil || claim != ExportClaimWon {
		t.Fatalf("attempt 2: %v err=%v", claim, err)
	}
}

// The cap on concurrent exports is enforced by the claim itself, atomically, so
// a bucket cohort reaching ARCHIVING together cannot start more attempts than
// the cap, and a shard held by the cap has not started anything: no counter
// moved, no clock running.
func TestShardStore_BeginExportAttemptHonorsTheCap(t *testing.T) {
	ss, db := exportFactsFixture(t)
	bucket := seedBucket(t, db, BucketStasis)
	if err := ss.SaveShard(&Shard{
		ID: "s-other", State: StateArchiving, BucketID: bucket.ID, Tier: "small",
		WindowStart: time.Now(), WindowEnd: time.Now(), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	if claim, err := ss.BeginExportAttempt("s-test", 0, "r7g.large", started, 1); err != nil || claim != ExportClaimWon {
		t.Fatalf("first shard: %v err=%v", claim, err)
	}
	claim, err := ss.BeginExportAttempt("s-other", 0, "r7g.large", started, 1)
	if err != nil {
		t.Fatal(err)
	}
	if claim != ExportClaimHeld {
		t.Fatalf("second shard at cap 1 = %v, want held", claim)
	}
	other, _, _ := ss.GetShard("s-other")
	if other.ExportAttempt != 0 || other.ExportStartedAt != nil {
		t.Fatalf("a held claim changed the row: %+v", other)
	}

	// A higher cap admits it.
	if claim, _ = ss.BeginExportAttempt("s-other", 0, "r7g.large", started, 2); claim != ExportClaimWon {
		t.Fatalf("second shard at cap 2 = %v, want won", claim)
	}
}

// The cap must hold when many shards are evaluated at once, which is exactly
// how a bucket cohort reaching ARCHIVING together arrives. This needs a real
// pooled connection and committed rows (the fixtures' transaction-scoped store
// cannot run goroutines), so it seeds its own and cleans up after itself. Without
// the advisory lock every claimant reads "0 in flight" before any has written,
// and more than the cap start.
func TestShardStore_BeginExportAttemptCapHoldsUnderConcurrentClaims(t *testing.T) {
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ss := ShardStore{db}
	bucket := seedBucket(t, db, BucketStasis)

	const shards, cap = 12, 3
	ids := make([]string, shards)
	for i := range ids {
		ids[i] = fmt.Sprintf("s-conc-%d-%d", time.Now().UnixNano(), i)
		if err := ss.SaveShard(&Shard{ID: ids[i], State: StateArchiving, BucketID: bucket.ID, Tier: "small",
			WindowStart: time.Now(), WindowEnd: time.Now(), CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, id := range ids {
			db.DB().Exec("DELETE FROM shard_operations WHERE shard_id = ?", id)
			db.DB().Exec("DELETE FROM shard_records WHERE id = ?", id)
		}
		db.DB().Exec("DELETE FROM buckets WHERE id = ?", bucket.ID)
	})

	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		mu      sync.Mutex
		results = map[ExportClaim]int{}
		errs    []error
	)
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			<-start
			claim, err := ss.BeginExportAttempt(id, 0, "r7g.large", time.Now(), cap)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			results[claim]++
		}(id)
	}
	close(start)
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("claims errored: %v", errs)
	}
	if results[ExportClaimWon] != cap || results[ExportClaimHeld] != shards-cap || results[ExportClaimLost] != 0 {
		t.Fatalf("with %d concurrent claims at cap %d: won=%d held=%d lost=%d, want exactly %d won and %d held",
			shards, cap, results[ExportClaimWon], results[ExportClaimHeld], results[ExportClaimLost], cap, shards-cap)
	}
	var started int64
	db.DB().Raw("SELECT COUNT(*) FROM shard_records WHERE id IN ? AND export_started_at IS NOT NULL", ids).Scan(&started)
	if started != cap {
		t.Fatalf("%d attempts recorded as in flight, want %d", started, cap)
	}
}

// A failed attempt gives its slot back, and a shard that has used up its
// attempts (facts cleared, counter kept) holds none.
func TestShardStore_ClearingFactsFreesTheSlot(t *testing.T) {
	ss, db := exportFactsFixture(t)
	bucket := seedBucket(t, db, BucketStasis)
	if err := ss.SaveShard(&Shard{
		ID: "s-other", State: StateArchiving, BucketID: bucket.ID, Tier: "small",
		WindowStart: time.Now(), WindowEnd: time.Now(), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	if claim, _ := ss.BeginExportAttempt("s-test", 0, "r7g.large", started, 1); claim != ExportClaimWon {
		t.Fatal("setup claim failed")
	}
	if claim, _ := ss.BeginExportAttempt("s-other", 0, "r7g.large", started, 1); claim != ExportClaimHeld {
		t.Fatal("expected held")
	}
	if err := ss.ClearExportFacts("s-test"); err != nil {
		t.Fatal(err)
	}
	if claim, err := ss.BeginExportAttempt("s-other", 0, "r7g.large", started, 1); err != nil || claim != ExportClaimWon {
		t.Fatalf("after the slot freed: %v err=%v", claim, err)
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
	if _, err := ss.BeginExportAttempt("s-test", 0, "r7g.large", started, 5); err != nil {
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
	if _, err := ss.BeginExportAttempt("s-test", 0, "r7g.xlarge", started, 5); err != nil {
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
