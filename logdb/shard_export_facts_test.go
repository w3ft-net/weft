//go:build test

package logdb

import (
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/shardstore"
)

func exportFactsFixture(t *testing.T) (shardstore.Store, shardstore.Store) {
	t.Helper()
	_, db, _ := makeFixtureShardSM(t, StateArchiving)
	return db, db
}

func getShardRow(t *testing.T, ss shardstore.Store) *ShardRow {
	t.Helper()
	sh, ok, err := ss.GetShard("s-test")
	if err != nil || !ok {
		t.Fatalf("GetShard: ok=%v err=%v", ok, err)
	}
	return sh
}

// SaveShard upserts the whole row from persistedCopy. If the copy omitted the
// export facts, any registry persist() — which fires on ordinary state changes —
// would write them back empty, mid-export.
func TestShard_PersistedCopyCarriesExportFacts(t *testing.T) {
	ss, _ := exportFactsFixture(t)
	started := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	entry := entryOf(getShardRow(t, ss))
	entry.Export.SnapshotID, entry.Export.Instance = "snap-1", "i-export"
	entry.Export.InstanceType, entry.Export.Attempt, entry.Export.StartedAt = "r7g.large", 2, &started

	cp := entry.persistedCopy()
	if cp.Export.StartedAt == entry.Export.StartedAt {
		t.Fatal("persistedCopy aliases the live entry's ExportStartedAt pointer")
	}
	if err := ss.SaveShard(cp); err != nil {
		t.Fatal(err)
	}
	got := getShardRow(t, ss)
	if got.Export.SnapshotID != "snap-1" || got.Export.Instance != "i-export" || got.Export.InstanceType != "r7g.large" ||
		got.Export.Attempt != 2 || got.Export.StartedAt == nil || !got.Export.StartedAt.Equal(started) {
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

	reg.RefreshFromStore(false)

	entry.mu.RLock()
	defer entry.mu.RUnlock()
	if entry.Export.Attempt != 1 || entry.Export.SnapshotID != "snap-7" || entry.Export.Instance != "i-exp-7" ||
		entry.Export.InstanceType != "r7g.xlarge" || entry.Export.StartedAt == nil {
		t.Fatalf("export facts not refreshed: attempt=%d snap=%q inst=%q type=%q started=%v",
			entry.Export.Attempt, entry.Export.SnapshotID, entry.Export.Instance, entry.Export.InstanceType, entry.Export.StartedAt)
	}
}
