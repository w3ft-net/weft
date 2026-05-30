package logdb

import (
	"testing"
	"time"

	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/store"
)

// TestRegistry_MatchingEntries_SkipsStaleHeartbeat confirms that shards
// whose LastHeartbeatAt is older than heartbeatStaleThreshold are
// excluded from query routing.
func TestRegistry_MatchingEntries_SkipsStaleHeartbeat(t *testing.T) {
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
	reg.Add(&Shard{
		ID: "fresh", Services: []store.ShardService{{NamespaceID: 1, Service: "vimgym"}},
		WindowStart: t0, WindowEnd: t2, State: StateIngesting,
		Live: freshShard, LastHeartbeatAt: time.Now(),
	})
	reg.Add(&Shard{
		ID: "stale", Services: []store.ShardService{{NamespaceID: 1, Service: "vimgym"}},
		WindowStart: t0, WindowEnd: t2, State: StateIngesting,
		Live: staleShard,
		// Heartbeat absent for 5 minutes — well past the 1-minute threshold.
		LastHeartbeatAt: time.Now().Add(-5 * time.Minute),
	})

	matching := reg.MatchingEntries(fullRange, servingFor(reg, "vimgym"), "bolt")
	if len(matching) != 1 {
		t.Fatalf("expected 1 matching shard (stale skipped), got %d", len(matching))
	}
	if matching[0].ID != "fresh" {
		t.Fatalf("expected fresh shard, got %s", matching[0].ID)
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
	reg.Add(&Shard{
		ID: "fresh", Services: []store.ShardService{{NamespaceID: 1, Service: "vimgym"}},
		WindowStart: t0, WindowEnd: t2, State: StateIngesting,
		Live: freshShard, LastHeartbeatAt: time.Now(),
	})
	reg.Add(&Shard{
		ID: "stale", Services: []store.ShardService{{NamespaceID: 1, Service: "vimgym"}},
		WindowStart: t0, WindowEnd: t2, State: StateIngesting,
		Live:            staleShard,
		LastHeartbeatAt: time.Now().Add(-5 * time.Minute),
	})

	// Multiple round-robin selections must never land on the stale shard.
	for i := 0; i < 10; i++ {
		picked := reg.IngestingShard(1, "vimgym")
		if picked == nil {
			t.Fatal("expected IngestingShard to return fresh shard")
		}
		if picked.ID() == "stale" {
			t.Fatalf("iteration %d: IngestingShard returned stale shard", i)
		}
	}
}

// TestRegistry_ZeroLastHeartbeatAtCountsAsFresh preserves backward
// compatibility for tests that construct Shard literals without
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
	reg.Add(&Shard{
		ID: "legacy", Services: []store.ShardService{{NamespaceID: 1, Service: "vimgym"}},
		WindowStart: t0, WindowEnd: t2, State: StateIngesting,
		Live: s,
		// LastHeartbeatAt left zero.
	})

	matching := reg.MatchingEntries(fullRange, servingFor(reg, "vimgym"), "bolt")
	if len(matching) != 1 {
		t.Fatalf("zero LastHeartbeatAt should count as fresh; got %d matching", len(matching))
	}
	if reg.IngestingShard(1, "vimgym") == nil {
		t.Fatal("zero LastHeartbeatAt should be eligible for write routing")
	}
}

// TestRegistry_HasApp_EmptyAppsNoLongerCatchAll pins the post-§8-#6
// behavior: a Shard with no explicit Apps no longer "matches all
// apps." Pre-launch the legacy catch-all was a convenience for direct-
// init tests; in production every shard has Apps populated by christen
// or adopt, so the empty-Apps branch was dead in prod and a confusing
// fallback in tests.
func TestRegistry_HasApp_EmptyAppsNoLongerCatchAll(t *testing.T) {
	s, err := shard.NewDuckDBShard("noapps", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	reg := NewShardRegistry()
	reg.Add(&Shard{
		ID: "noapps", State: StateIngesting,
		WindowStart: t0, WindowEnd: t2,
		Live: s,
		// Apps intentionally left nil.
	})

	// Specific-app query should miss.
	if matching := reg.MatchingEntries(fullRange, servingFor(reg, "vimgym"), "bolt"); len(matching) != 0 {
		t.Errorf("empty-Apps shard should NOT match specific app query, got %d", len(matching))
	}
	// Whole-customer query (app=="") still sees all shards — that's a
	// different code path (the hasApp filter is skipped when app is
	// empty), independent of the catch-all rip.
	if matching := reg.MatchingEntries(fullRange, nil, "bolt"); len(matching) != 1 {
		t.Errorf("whole-customer query should see the shard, got %d", len(matching))
	}
	// Write routing: an empty-Apps entry shouldn't be picked for any
	// specific app either.
	if reg.IngestingShard(1, "vimgym") != nil {
		t.Error("empty-Apps shard should NOT be picked as the ingest target for vimgym")
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
	reg.Add(&Shard{
		ID:              "shard-live",
		Services:        []store.ShardService{{NamespaceID: 1, Service: "weft"}},
		State:           StateIngesting,
		Addr:            "10.0.0.1:9000",
		Live:            ds,
		CreatedAt:       time.Now(),
		LastHeartbeatAt: priorBeat,
	})

	// ProcessHeartbeat routes a steady-state (INGESTING) heartbeat to the
	// shard's ObserveHeartbeat, which refreshes liveness. The entry IS its
	// own SM now — ProcessHeartbeat binds it lazily via ensureSM, so no
	// pre-registration is needed. Same addr + live handle means no
	// reconnect — just the touch.
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
