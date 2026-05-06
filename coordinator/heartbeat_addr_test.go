package coordinator

import (
	"os"
	"testing"
	"time"

	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/store"
)

// TestMatchingEntries_AfterRestart proves that MatchingEntries returns
// INGESTING shards loaded from the DB even when they have no live
// Shard pointer (the normal state after a coordinator restart, before
// the first heartbeat re-dials the gRPC connection).
//
// This is the production bug from d477ea2: MatchingEntries inherited
// the `e.Shard == nil → skip` guard from the old MatchingShards. That
// guard was correct when the coordinator dialed shards itself (it
// needed a live Shard). After d477ea2, the coordinator only needs
// e.Addr to build a ShardDescriptor for compute — but the nil-Shard
// guard hides every shard after restart, so compute gets zero
// descriptors and every query returns empty / times out.
func TestMatchingEntries_AfterRestart(t *testing.T) {
	dsn := os.Getenv("WEFT_TEST_DSN")
	if dsn == "" {
		dsn = "host=localhost port=5433 dbname=weft user=weft password=weft sslmode=disable"
	}
	db, err := store.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	tx, rollback := db.BeginTest()
	t.Cleanup(func() {
		rollback()
		db.Close()
	})

	bucket := &store.Bucket{
		Label:                "matching-entries-restart-test",
		Apps:                 `["testapp"]`,
		TargetIngestingCount: 1,
		Tier:                 "small",
		CreatedAt:            time.Now().UTC().Format(time.RFC3339),
		WindowDuration:       "1h",
	}
	if err := tx.SaveBucket(bucket); err != nil {
		t.Fatalf("seed bucket: %v", err)
	}

	const shardAddr = "10.0.99.1:9000"
	now := time.Now().UTC()

	// Phase 1: christen — save a shard with Addr and a live Shard pointer.
	reg := NewShardRegistry()
	reg.SetStore(tx)
	reg.Add(&ShardEntry{
		ID:          "shard-restart-test",
		Apps:        []string{"testapp"},
		WindowStart: now.Add(-time.Hour),
		WindowEnd:   now.Add(time.Hour),
		State:       StateIngesting,
		Addr:        shardAddr,
		InstanceID:  "i-fake",
		Tier:        "small",
		CreatedAt:   now,
		BucketID:    bucket.ID,
	})

	// Phase 2: simulate coordinator restart — LoadFromStore with
	// reconstruct returning nil (the production reconstruct only
	// handles ARCHIVED shards; live shards get Shard=nil until a
	// heartbeat arrives and the provisioner dials them).
	reg2 := NewShardRegistry()
	reg2.SetStore(tx)
	if err := reg2.LoadFromStore(func(r store.ShardRecord) (shard.Shard, error) {
		return nil, nil
	}); err != nil {
		t.Fatalf("LoadFromStore: %v", err)
	}

	reloaded := reg2.Get("shard-restart-test")
	if reloaded == nil {
		t.Fatal("shard not in registry after LoadFromStore")
	}
	if reloaded.Addr != shardAddr {
		t.Fatalf("Addr after LoadFromStore: got %q, want %q", reloaded.Addr, shardAddr)
	}
	if reloaded.Shard != nil {
		t.Fatal("expected Shard==nil after LoadFromStore (no reconstruct for live shards)")
	}

	// Phase 3: MatchingEntries must include this shard. The
	// coordinator builds ShardDescriptors from Addr, not from
	// Shard — a nil Shard with a valid Addr is queryable.
	tr := TimeRange{Start: now.Add(-30 * time.Minute), End: now}
	entries := reg2.MatchingEntries(tr, "", store.TierBolt)
	if len(entries) == 0 {
		t.Fatal("MatchingEntries returned 0 entries after restart — this is the d477ea2 production bug")
	}

	found := false
	for _, e := range entries {
		if e.ID == "shard-restart-test" {
			found = true
			if e.Addr != shardAddr {
				t.Fatalf("matched entry has wrong Addr: got %q, want %q", e.Addr, shardAddr)
			}
		}
	}
	if !found {
		t.Fatal("shard-restart-test not in MatchingEntries result")
	}
}
