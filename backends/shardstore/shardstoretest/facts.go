package shardstoretest

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/shardstore"
)

var factsCases = []struct {
	name string
	fn   func(*testing.T, Subject)
}{
	{"FactsRoundTripThroughSaveAndLoad", factsRoundTrip},
	{"BeginExportAttemptIsACompareAndSet", exportCompareAndSet},
	{"BeginExportAttemptHonorsTheCap", exportCap},
	{"BeginExportAttemptCapHoldsUnderConcurrentClaims", exportCapConcurrent},
	{"ClearingFactsFreesTheSlot", exportClearFreesSlot},
	{"ExportFactsAreNarrowWrites", exportNarrow},
	{"ClearExportFactsKeepsTheAttemptCounter", exportClearKeepsCounter},
	{"LatestShardDiskBytes", latestDiskBytes},
	{"UpgradeIsACompareAndSetOnThePhase", upgradeCompareAndSet},
	{"DeletingAShardDeletesItsFacts", deleteTakesFacts},
}

var export0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

// archiving is a fresh ARCHIVING shard in a fresh bucket.
func archiving(t *testing.T, s Subject, id string) *shardstore.ShardRow {
	t.Helper()
	return newShard(t, s, newBucket(t, s).ID, id, shardstore.StateArchiving)
}

func utcExport(f shardstore.ExportFacts) shardstore.ExportFacts {
	if f.StartedAt != nil {
		t := f.StartedAt.UTC()
		f.StartedAt = &t
	}
	return f
}

func utcUpgrade(f shardstore.UpgradeFacts) shardstore.UpgradeFacts {
	if f.PhaseAt != nil {
		t := f.PhaseAt.UTC()
		f.PhaseAt = &t
	}
	if f.PausedAt != nil {
		t := f.PausedAt.UTC()
		f.PausedAt = &t
	}
	return f
}

// Export and upgrade facts survive SaveShard → GetShard / ListShards, and a
// shard saved without them reads back with none.
func factsRoundTrip(t *testing.T, s Subject) {
	archiving(t, s, "s-1")
	sh := &shardstore.ShardRow{}
	*sh = get(t, s, "s-1")
	at, paused := export0, export0.Add(time.Minute)
	sh.Export = shardstore.ExportFacts{SnapshotID: "snap", Instance: "i-x", InstanceType: "r7g.large", StartedAt: &at, Attempt: 2}
	sh.Upgrade = shardstore.UpgradeFacts{
		Phase: shardstore.PhasePausing, Origin: shardstore.StateIngesting, PhaseAt: &at, PausedAt: &paused,
		SuccessorInstanceID: "i-new", SuccessorAddr: "10.0.0.2:9000", DemotedInstanceID: "i-old", DemotedAddr: "10.0.0.1:9000",
		TraceID: "trace", ParentSpanID: "span",
	}
	if err := s.Store.SaveShard(sh); err != nil {
		t.Fatal(err)
	}
	got := get(t, s, "s-1")
	if !reflect.DeepEqual(utcExport(got.Export), sh.Export) || !reflect.DeepEqual(utcUpgrade(got.Upgrade), sh.Upgrade) {
		t.Fatalf("facts lost:\n got %+v / %+v\nwant %+v / %+v", got.Export, got.Upgrade, sh.Export, sh.Upgrade)
	}
	all, err := s.Store.ListShards()
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range all {
		if l.ID == "s-1" && !reflect.DeepEqual(utcUpgrade(l.Upgrade), sh.Upgrade) {
			t.Fatalf("ListShards dropped the upgrade facts: %+v", l.Upgrade)
		}
	}
	byBucket, err := s.Store.ListShardsByBucket(sh.BucketID)
	if err != nil || len(byBucket) != 1 || !reflect.DeepEqual(utcExport(byBucket[0].Export), sh.Export) {
		t.Fatalf("ListShardsByBucket dropped the export facts: %+v err=%v", byBucket, err)
	}

	got.Upgrade, got.Export = shardstore.UpgradeFacts{}, shardstore.ExportFacts{}
	if err := s.Store.SaveShard(&got); err != nil {
		t.Fatal(err)
	}
	if again := get(t, s, "s-1"); again.Upgrade != (shardstore.UpgradeFacts{}) || again.Export != (shardstore.ExportFacts{}) {
		t.Fatalf("cleared facts came back: %+v / %+v", again.Upgrade, again.Export)
	}
}

func exportCompareAndSet(t *testing.T, s Subject) {
	archiving(t, s, "s-1")
	claim, err := s.Store.BeginExportAttempt("s-1", 0, "r7g.large", export0, 5)
	if err != nil || claim != shardstore.ExportClaimWon {
		t.Fatalf("first claim: %v err=%v", claim, err)
	}
	sh := get(t, s, "s-1")
	if sh.Export.Attempt != 1 || sh.Export.InstanceType != "r7g.large" || sh.Export.StartedAt == nil || !sh.Export.StartedAt.Equal(export0) {
		t.Fatalf("claim not recorded: %+v", sh.Export)
	}

	// A second evaluator that read attempt=0 before the first claim lost the
	// race: it must not start attempt 1 again.
	claim, err = s.Store.BeginExportAttempt("s-1", 0, "r7g.2xlarge", export0, 5)
	if err != nil || claim != shardstore.ExportClaimLost {
		t.Fatalf("a stale claim after attempt 1 began = %v err=%v, want lost", claim, err)
	}
	if got := get(t, s, "s-1"); got.Export.Attempt != 1 || got.Export.InstanceType != "r7g.large" {
		t.Fatalf("losing claim changed the row: %+v", got.Export)
	}

	// The next attempt claims from the current value, after the failed one's
	// facts are cleared.
	if err := s.Store.ClearExportFacts("s-1"); err != nil {
		t.Fatal(err)
	}
	if claim, err = s.Store.BeginExportAttempt("s-1", 1, "r7g.xlarge", export0, 5); err != nil || claim != shardstore.ExportClaimWon {
		t.Fatalf("attempt 2: %v err=%v", claim, err)
	}
	if claim, err = s.Store.BeginExportAttempt("gone", 0, "r7g.large", export0, 5); err != nil || claim != shardstore.ExportClaimLost {
		t.Fatalf("a missing shard: %v err=%v, want lost", claim, err)
	}
}

// The cap on concurrent exports is enforced by the claim itself, so a cohort
// reaching ARCHIVING together cannot start more attempts than the cap, and a
// shard the cap holds has started nothing: no counter moved, no clock running.
func exportCap(t *testing.T, s Subject) {
	b := newBucket(t, s)
	newShard(t, s, b.ID, "s-1", shardstore.StateArchiving)
	newShard(t, s, b.ID, "s-2", shardstore.StateArchiving)

	if claim, err := s.Store.BeginExportAttempt("s-1", 0, "r7g.large", export0, 1); err != nil || claim != shardstore.ExportClaimWon {
		t.Fatalf("first shard: %v err=%v", claim, err)
	}
	if claim, err := s.Store.BeginExportAttempt("s-2", 0, "r7g.large", export0, 1); err != nil || claim != shardstore.ExportClaimHeld {
		t.Fatalf("second shard at cap 1 = %v err=%v, want held", claim, err)
	}
	if other := get(t, s, "s-2"); other.Export.Attempt != 0 || other.Export.StartedAt != nil {
		t.Fatalf("a held claim changed the row: %+v", other.Export)
	}
	if claim, _ := s.Store.BeginExportAttempt("s-2", 0, "r7g.large", export0, 2); claim != shardstore.ExportClaimWon {
		t.Fatalf("second shard at cap 2 = %v, want won", claim)
	}
}

// The cap must hold when many shards are evaluated at once.
func exportCapConcurrent(t *testing.T, s Subject) {
	if !s.Concurrent {
		t.Skip("the store under test is not safe for concurrent callers")
	}
	b := newBucket(t, s)
	const shards, cap = 12, 3
	ids := make([]string, shards)
	for i := range ids {
		ids[i] = fmt.Sprintf("s-conc-%d", i)
		newShard(t, s, b.ID, ids[i], shardstore.StateArchiving)
	}
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		mu      sync.Mutex
		results = map[shardstore.ExportClaim]int{}
		errs    []error
	)
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			<-start
			claim, err := s.Store.BeginExportAttempt(id, 0, "r7g.large", time.Now(), cap)
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
	if results[shardstore.ExportClaimWon] != cap || results[shardstore.ExportClaimHeld] != shards-cap || results[shardstore.ExportClaimLost] != 0 {
		t.Fatalf("with %d concurrent claims at cap %d: won=%d held=%d lost=%d, want exactly %d won and %d held",
			shards, cap, results[shardstore.ExportClaimWon], results[shardstore.ExportClaimHeld], results[shardstore.ExportClaimLost], cap, shards-cap)
	}
}

// A failed attempt gives its slot back.
func exportClearFreesSlot(t *testing.T, s Subject) {
	b := newBucket(t, s)
	newShard(t, s, b.ID, "s-1", shardstore.StateArchiving)
	newShard(t, s, b.ID, "s-2", shardstore.StateArchiving)
	if claim, _ := s.Store.BeginExportAttempt("s-1", 0, "r7g.large", export0, 1); claim != shardstore.ExportClaimWon {
		t.Fatal("setup claim failed")
	}
	if claim, _ := s.Store.BeginExportAttempt("s-2", 0, "r7g.large", export0, 1); claim != shardstore.ExportClaimHeld {
		t.Fatal("expected held")
	}
	if err := s.Store.ClearExportFacts("s-1"); err != nil {
		t.Fatal(err)
	}
	if claim, err := s.Store.BeginExportAttempt("s-2", 0, "r7g.large", export0, 1); err != nil || claim != shardstore.ExportClaimWon {
		t.Fatalf("after the slot freed: %v err=%v", claim, err)
	}
}

func exportNarrow(t *testing.T, s Subject) {
	archiving(t, s, "s-1")
	if err := s.Store.SetShardLaunchResult("s-1", "i-shard", "us-east-2a", "vol-1", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SetShardParquetPath("s-1", "s3://x"); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SetExportSnapshot("s-1", "snap-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SetExportInstance("s-1", "i-export"); err != nil {
		t.Fatal(err)
	}
	got := get(t, s, "s-1")
	if got.Export.SnapshotID != "snap-1" || got.Export.Instance != "i-export" {
		t.Fatalf("facts not written: %+v", got.Export)
	}
	if got.InstanceID != "i-shard" || got.EBSVolumeID != "vol-1" || got.ParquetPath != "s3://x" || got.State != shardstore.StateArchiving {
		t.Fatalf("an export write trampled an unrelated column: %+v", got)
	}
}

func exportClearKeepsCounter(t *testing.T, s Subject) {
	archiving(t, s, "s-1")
	if _, err := s.Store.BeginExportAttempt("s-1", 0, "r7g.large", export0, 5); err != nil {
		t.Fatal(err)
	}
	_ = s.Store.SetExportSnapshot("s-1", "snap-1")
	_ = s.Store.SetExportInstance("s-1", "i-export")
	if err := s.Store.ClearExportFacts("s-1"); err != nil {
		t.Fatal(err)
	}
	got := get(t, s, "s-1").Export
	if got.SnapshotID != "" || got.Instance != "" || got.InstanceType != "" || got.StartedAt != nil {
		t.Fatalf("facts not cleared: %+v", got)
	}
	// The counter caps retries and keeps the next attempt's tags distinct.
	if got.Attempt != 1 {
		t.Fatalf("attempt counter = %d, want 1", got.Attempt)
	}
}

func latestDiskBytes(t *testing.T, s Subject) {
	archiving(t, s, "s-1")
	if n, err := s.Store.LatestShardDiskBytes("s-1"); err != nil || n != 0 {
		t.Fatalf("no heartbeats yet: n=%d err=%v, want 0", n, err)
	}
	for _, disk := range []int64{100, 300} {
		if err := s.Store.TouchHeartbeat("s-1", 0, time.Now(), "", disk); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.Store.LatestShardDiskBytes("s-1"); err != nil || n != 300 {
		t.Fatalf("disk bytes = %d err=%v, want 300", n, err)
	}
	// A beat that reports no footprint must not erase the last known one.
	if err := s.Store.TouchHeartbeat("s-1", 0, time.Now(), "", 0); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Store.LatestShardDiskBytes("s-1"); err != nil || n != 300 {
		t.Fatalf("after a beat reporting 0: %d err=%v, want 300", n, err)
	}
	if n, err := s.Store.LatestShardDiskBytes("s-unknown"); err != nil || n != 0 {
		t.Fatalf("unknown shard: n=%d err=%v, want 0", n, err)
	}
}

func upgradeCompareAndSet(t *testing.T, s Subject) {
	archiving(t, s, "s-1")
	now := time.Now().UTC().Truncate(time.Second)
	claiming := shardstore.UpgradeFacts{Phase: shardstore.PhaseClaiming, Origin: "operator", PhaseAt: &now}

	if err := s.Store.SetShardUpgrade("s-1", "", claiming); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SetShardUpgrade("s-1", "", claiming); !errors.Is(err, shardstore.ErrUpgradeMoved) {
		t.Fatalf("a second start from no phase: %v, want ErrUpgradeMoved", err)
	}
	pausing := shardstore.UpgradeFacts{Phase: shardstore.PhasePausing, Origin: "operator", PhaseAt: &now, SuccessorInstanceID: "i-new"}
	if err := s.Store.SetShardUpgrade("s-1", shardstore.PhaseClaiming, pausing); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SetShardUpgrade("s-1", shardstore.PhaseClaiming, pausing); !errors.Is(err, shardstore.ErrUpgradeMoved) {
		t.Fatalf("a stale writer: %v, want ErrUpgradeMoved", err)
	}
	if got := get(t, s, "s-1").Upgrade; got.Phase != shardstore.PhasePausing || got.SuccessorInstanceID != "i-new" {
		t.Fatalf("upgrade facts: %+v", got)
	}
	if err := s.Store.SetShardUpgrade("s-1", shardstore.PhasePausing, shardstore.UpgradeFacts{}); err != nil {
		t.Fatal(err)
	}
	if got := get(t, s, "s-1").Upgrade; got != (shardstore.UpgradeFacts{}) {
		t.Fatalf("zero facts did not clear the upgrade: %+v", got)
	}
	if err := s.Store.SetShardUpgrade("gone", "", claiming); !errors.Is(err, shardstore.ErrUpgradeMoved) {
		t.Fatalf("a missing shard: %v, want ErrUpgradeMoved", err)
	}
}

// A shard id reused after a delete starts without the old shard's facts.
func deleteTakesFacts(t *testing.T, s Subject) {
	b := newBucket(t, s)
	newShard(t, s, b.ID, "s-1", shardstore.StateArchiving)
	if _, err := s.Store.BeginExportAttempt("s-1", 0, "r7g.large", export0, 5); err != nil {
		t.Fatal(err)
	}
	now := export0
	if err := s.Store.SetShardUpgrade("s-1", "", shardstore.UpgradeFacts{Phase: shardstore.PhaseClaiming, PhaseAt: &now}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.DeleteShard("s-1"); err != nil {
		t.Fatal(err)
	}
	newShard(t, s, b.ID, "s-1", shardstore.StateArchiving)
	got := get(t, s, "s-1")
	if got.Export != (shardstore.ExportFacts{}) || got.Upgrade != (shardstore.UpgradeFacts{}) {
		t.Fatalf("a recreated shard inherited facts: %+v / %+v", got.Export, got.Upgrade)
	}
}
