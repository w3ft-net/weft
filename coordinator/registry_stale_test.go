package coordinator

import (
	"testing"
	"time"

	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/store"
)

// TestRegistry_MatchingShards_SkipsStaleHeartbeat confirms that shards
// whose LastHeartbeatAt is older than heartbeatStaleThreshold are
// excluded from query routing. Without this, the coordinator's registry
// holds entries pointing at containers that died and never came back,
// and Phase 2 retries against those dead Addrs produce repeated errors
// for the duration of any user session that touches apps owned by
// those shards.
func TestRegistry_MatchingShards_SkipsStaleHeartbeat(t *testing.T) {
	freshShard, err := shard.NewDuckDBShard("fresh", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { freshShard.Close() })
	staleShard, err := shard.NewDuckDBShard("stale", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { staleShard.Close() })

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID: "fresh", Apps: []string{"vimgym"},
		WindowStart: t0, WindowEnd: t2, State: StateIngesting,
		Shard: freshShard, LastHeartbeatAt: time.Now(),
	})
	reg.Add(&ShardEntry{
		ID: "stale", Apps: []string{"vimgym"},
		WindowStart: t0, WindowEnd: t2, State: StateIngesting,
		Shard: staleShard,
		// Heartbeat absent for 5 minutes — well past the 2-minute threshold.
		LastHeartbeatAt: time.Now().Add(-5 * time.Minute),
	})

	matching := reg.MatchingShards(fullRange, "vimgym", "bolt")
	if len(matching) != 1 {
		t.Fatalf("expected 1 matching shard (stale skipped), got %d", len(matching))
	}
	if matching[0].ID() != "fresh" {
		t.Fatalf("expected fresh shard, got %s", matching[0].ID())
	}
}

// TestRegistry_IngestingShard_SkipsStaleHeartbeat confirms write
// routing also excludes stale entries. A dead Addr can't accept
// writes, so picking it would just produce repeated ingest errors.
func TestRegistry_IngestingShard_SkipsStaleHeartbeat(t *testing.T) {
	freshShard, err := shard.NewDuckDBShard("fresh", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { freshShard.Close() })
	staleShard, err := shard.NewDuckDBShard("stale", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { staleShard.Close() })

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID: "fresh", Apps: []string{"vimgym"},
		WindowStart: t0, WindowEnd: t2, State: StateIngesting,
		Shard: freshShard, LastHeartbeatAt: time.Now(),
	})
	reg.Add(&ShardEntry{
		ID: "stale", Apps: []string{"vimgym"},
		WindowStart: t0, WindowEnd: t2, State: StateIngesting,
		Shard:           staleShard,
		LastHeartbeatAt: time.Now().Add(-5 * time.Minute),
	})

	// Multiple round-robin selections must never land on the stale shard.
	for i := 0; i < 10; i++ {
		picked := reg.IngestingShard("vimgym")
		if picked == nil {
			t.Fatal("expected IngestingShard to return fresh shard")
		}
		if picked.ID() == "stale" {
			t.Fatalf("iteration %d: IngestingShard returned stale shard", i)
		}
	}
}

// TestRegistry_ZeroLastHeartbeatAtCountsAsFresh preserves backward
// compatibility for tests that construct ShardEntry literals without
// the heartbeat field — e.g., archiver_test, coordinator_test. The
// zero value should NOT be treated as "infinitely stale"; only an
// explicitly aged timestamp triggers the filter.
func TestRegistry_ZeroLastHeartbeatAtCountsAsFresh(t *testing.T) {
	s, err := shard.NewDuckDBShard("legacy", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID: "legacy", Apps: []string{"vimgym"},
		WindowStart: t0, WindowEnd: t2, State: StateIngesting,
		Shard: s,
		// LastHeartbeatAt left zero.
	})

	matching := reg.MatchingShards(fullRange, "vimgym", "bolt")
	if len(matching) != 1 {
		t.Fatalf("zero LastHeartbeatAt should count as fresh; got %d matching", len(matching))
	}
	if reg.IngestingShard("vimgym") == nil {
		t.Fatal("zero LastHeartbeatAt should be eligible for write routing")
	}
}

// TestProcessHeartbeat_RefreshesLastHeartbeatAt confirms that the
// same-address heartbeat path updates LastHeartbeatAt. This is the
// hot path in steady state — every ~10s a heartbeat arrives at the
// same Addr, and we need that to push LastHeartbeatAt forward, or
// every entry would go stale 2 minutes after it was first registered.
func TestProcessHeartbeat_RefreshesLastHeartbeatAt(t *testing.T) {
	reg := NewShardRegistry()
	ds, err := shard.NewDuckDBShard("shard-live", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })

	// Pre-populate the entry with an aged heartbeat so we can detect
	// the refresh.
	priorBeat := time.Now().Add(-3 * time.Minute)
	reg.Add(&ShardEntry{
		ID:              "shard-live",
		Apps:            []string{"weft"},
		State:           StateIngesting,
		Addr:            "10.0.0.1:9000",
		Shard:           ds,
		CreatedAt:       time.Now(),
		LastHeartbeatAt: priorBeat,
	})

	prov := &Provisioner{
		registry:   reg,
		config:     ProvisionerConfig{PollInterval: time.Second, WindowDuration: 7 * 24 * time.Hour},
		configurer: &stubConfigurer{},
	}

	prov.ProcessHeartbeat(store.ShardHeartbeat{
		ShardID: "shard-live",
		Addr:    "10.0.0.1:9000",
		State:   "INGESTING",
	})

	got := reg.Get("shard-live")
	if got == nil {
		t.Fatal("entry vanished after heartbeat")
	}
	if !got.LastHeartbeatAt.After(priorBeat) {
		t.Fatalf("LastHeartbeatAt not advanced: was %v, still %v", priorBeat, got.LastHeartbeatAt)
	}
	if time.Since(got.LastHeartbeatAt) > 5*time.Second {
		t.Fatalf("LastHeartbeatAt should be ~now, got %v ago", time.Since(got.LastHeartbeatAt))
	}
}
