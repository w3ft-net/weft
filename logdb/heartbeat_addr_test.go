package logdb

import (
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/shardstore"

	"github.com/w3ft-net/weft/shard"
)

// TestMatchingEntries_AfterRestart proves that MatchingEntries returns
// INGESTING shards loaded from the DB even when they have no live
// Shard pointer (the normal state after a coordinator restart, before
// the first heartbeat re-dials the gRPC connection).
//
// This is the production bug from d477ea2: MatchingEntries inherited
// the `e.Live == nil → skip` guard from the old MatchingShards. That
// guard was correct when the coordinator dialed shards itself (it
// needed a live Shard). After d477ea2, the coordinator only needs
// e.Addr to build a ShardDescriptor for compute — but the nil-Shard
// guard hides every shard after restart, so compute gets zero
// descriptors and every query returns empty / times out.
func TestMatchingEntries_AfterRestart(t *testing.T) {
	tx := openSMTestStore(t)

	bucket := &shardstore.Bucket{
		Label:                "matching-entries-restart-test",
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
	reg.Add(&Shard{
		ShardRow: ShardRow{
			ID:          "shard-restart-test",
			WindowStart: now.Add(-time.Hour),
			WindowEnd:   now.Add(time.Hour),
			State:       StateIngesting,
			Addr:        shardAddr,
			InstanceID:  "i-fake",
			Tier:        "small",
			CreatedAt:   now,
			BucketID:    bucket.ID,
		},
		Services: []shardstore.ShardService{{NamespaceID: 1, Service: "testapp"}},
	})

	// Phase 2: simulate coordinator restart — LoadFromStore with
	// reconstruct returning nil (the production reconstruct only
	// handles ARCHIVED shards; live shards get Shard=nil until a
	// heartbeat arrives and the provisioner dials them).
	reg2 := NewShardRegistry()
	reg2.SetStore(tx)
	if err := reg2.LoadFromStore(func(r *Shard) (shard.Shard, error) {
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
	if reloaded.Live != nil {
		t.Fatal("expected Shard==nil after LoadFromStore (no reconstruct for live shards)")
	}

	// Phase 3: MatchingEntries must include this shard. The
	// coordinator builds ShardDescriptors from Addr, not from
	// Shard — a nil Shard with a valid Addr is queryable.
	tr := TimeRange{Start: now.Add(-30 * time.Minute), End: now}
	entries := mustMatch(t, reg2, tr, nil, "bolt")
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

// TestTouchHeartbeat_PersistsLastHeartbeatAt proves the heartbeat path now
// writes last_heartbeat_at through to shard_records — the column was zero
// at insert and stayed zero forever because TouchHeartbeat only persisted
// row_count, so shard liveness couldn't be observed via SQL.
func TestTouchHeartbeat_PersistsLastHeartbeatAt(t *testing.T) {
	tx := openSMTestStore(t)

	bucket := &shardstore.Bucket{
		Label:                "touch-hb-test",
		TargetIngestingCount: 1,
		Tier:                 "small",
		CreatedAt:            time.Now().UTC().Format(time.RFC3339),
		WindowDuration:       "1h",
	}
	if err := tx.SaveBucket(bucket); err != nil {
		t.Fatalf("seed bucket: %v", err)
	}

	now := time.Now().UTC()
	reg := NewShardRegistry()
	reg.SetStore(tx)
	reg.Add(&Shard{
		ShardRow: ShardRow{
			ID:          "shard-hb-test",
			WindowStart: now.Add(-time.Hour),
			WindowEnd:   now.Add(time.Hour),
			State:       StateIngesting,
			Tier:        "small",
			CreatedAt:   now,
			BucketID:    bucket.ID,
		},
	})

	// At insert the column is the zero sentinel (no heartbeat yet) — the
	// value isStale() reads as "fresh".
	rec, found, err := tx.GetShard("shard-hb-test")
	if err != nil || !found {
		t.Fatalf("GetShard after Add: found=%v err=%v", found, err)
	}
	if !rec.LastHeartbeatAt.IsZero() {
		t.Fatalf("last_heartbeat_at should be zero at insert, got %v", rec.LastHeartbeatAt)
	}

	// Simulate a heartbeat: the SM stamps the in-memory entry, then the
	// registry flushes the heartbeat-owned fields (row_count,
	// last_heartbeat_at, and the shard's compile-time binary_sha — Phase 3
	// of shard-roll uses the persisted SHA to skip rolling a shard that's
	// already on the target binary).
	entry := reg.Get("shard-hb-test")
	hbAt := now.Add(5 * time.Minute)
	entry.mu.Lock()
	entry.LastHeartbeatAt = hbAt
	entry.RowCount = 42
	entry.DiskBytes = 7 << 30
	entry.BinarySHA = "deadbeefcafe1234"
	entry.mu.Unlock()
	reg.TouchHeartbeat(entry)

	rec, found, err = tx.GetShard("shard-hb-test")
	if err != nil || !found {
		t.Fatalf("GetShard after TouchHeartbeat: found=%v err=%v", found, err)
	}
	if rec.LastHeartbeatAt.IsZero() {
		t.Fatal("last_heartbeat_at still zero after TouchHeartbeat — not persisted")
	}
	// Tolerance covers Postgres timestamptz microsecond rounding vs Go ns.
	if d := rec.LastHeartbeatAt.Sub(hbAt); d > time.Millisecond || d < -time.Millisecond {
		t.Fatalf("last_heartbeat_at: got %v, want ~%v (diff %v)", rec.LastHeartbeatAt, hbAt, d)
	}
	if rec.RowCount != 42 {
		t.Fatalf("row_count: got %d, want 42", rec.RowCount)
	}
	if rec.DiskBytes != 7<<30 {
		t.Fatalf("disk_bytes: got %d, want %d (export sizing reads it from shard_records)", rec.DiskBytes, int64(7<<30))
	}
	if rec.BinarySHA != "deadbeefcafe1234" {
		t.Fatalf("binary_sha: got %q, want %q (TouchHeartbeat must persist the build SHA so weft-admin can skip shards already on the target)", rec.BinarySHA, "deadbeefcafe1234")
	}
}
