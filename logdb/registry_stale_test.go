package logdb

import (
	"context"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/shard"
	"github.com/graybeardcorp/weft/store"
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

// TestRegistry_MatchingEntries_ArchivedShardsIgnoreStaleHeartbeat pins
// a production bug: ARCHIVED shards never heartbeat again once their
// instance is retired, so LastHeartbeatAt freezes at whatever it was
// before archival. evaluateArchiving clears it to zero in-memory
// (shardsm.go), but that clear is never persisted — ShardStore.
// SetShardParquetPath only updates the parquet_path column. So the
// moment the coordinator restarts, LoadFromStore rehydrates the shard
// from Postgres with its stale pre-archival LastHeartbeatAt, and
// isStale() (designed to detect a dead EC2 instance) permanently
// excludes the shard from every future query — even though the data
// is sitting right there in S3. In prod this made every archived
// shard — months of history — invisible to queries with zero error or
// log trace, and the cold-shard Lambda was never invoked (its
// Invocations metric had zero datapoints). Staleness is a live-
// instance liveness signal; it must not apply once a shard has no
// instance left to be stale about.
func TestRegistry_MatchingEntries_ArchivedShardsIgnoreStaleHeartbeat(t *testing.T) {
	now := time.Now()
	reg := NewShardRegistry()

	reg.Add(&Shard{
		ID:          "archived-old",
		Services:    []store.ShardService{{NamespaceID: 1, Service: "vimgym"}},
		WindowStart: now.Add(-45 * 24 * time.Hour),
		WindowEnd:   now.Add(-44 * 24 * time.Hour),
		State:       StateArchived,
		Addr:        "10.0.0.1:9000", // non-empty so the addr/Live guard passes; see registry_empty_shard_test.go
		ParquetPath: "s3://weft-cold/1-apps/shard-archived-old.parquet",
		RowCount:    340,
		// Rehydrated from a stale DB row after a coordinator restart —
		// last observed heartbeat from well before this shard archived,
		// never cleared in Postgres.
		LastHeartbeatAt: now.Add(-50 * 24 * time.Hour),
	})

	tr := TimeRange{Start: now.Add(-46 * 24 * time.Hour), End: now.Add(-43 * 24 * time.Hour)}
	matching := reg.MatchingEntries(tr, servingFor(reg, "vimgym"), "bolt")
	if len(matching) != 1 {
		t.Fatalf("archived shard with real, in-window data was excluded by a stale pre-archival heartbeat; got %d matching", len(matching))
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

// A coordinator that doesn't hold a shard's heartbeat stream learns of its
// beats only from the store sweep, so between sweeps its copy of the last
// beat ages. Judging that copy against the wall clock made the coordinator's
// own lateness count against the shard: a sweep that ran late (a slow FSM
// tick, a GC pause) dropped a healthy shard from query and write routing, and
// a one-minute sweep against a one-minute threshold did so before every tick.
// Liveness must be judged as of the last read that could have seen a beat.
func TestAdoptFromHeartbeats_LateSweepKeepsShardLive(t *testing.T) {
	ds, err := shard.NewDuckDBShard("shard-elsewhere", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })

	// The sweep ran 70s ago and saw a beat 9s old: a healthy shard whose
	// next sweep is late.
	sweptAt := time.Now().Add(-70 * time.Second)
	now := sweptAt
	reg := NewShardRegistry()
	reg.Add(&Shard{
		ID: "shard-elsewhere", Services: []store.ShardService{{NamespaceID: 1, Service: "vimgym"}},
		WindowStart: t0, WindowEnd: t2, State: StateIngesting,
		Addr: "10.0.0.2:9000", Live: ds,
	})
	hbs := &mockHeartbeatStore{heartbeats: []store.ShardHeartbeat{{
		ShardID: "shard-elsewhere", Addr: "10.0.0.2:9000", State: "INGESTING", Age: 9 * time.Second,
	}}}
	prov := &Provisioner{
		registry:   reg,
		config:     ProvisionerConfig{Store: hbs, PollInterval: time.Minute, WindowDuration: 7 * 24 * time.Hour},
		configurer: &stubConfigurer{},
		clock:      func() time.Time { return now },
	}
	prov.shardDeps().BootedAt = sweptAt.Add(-time.Hour)
	prov.adoptFromHeartbeats()
	now = sweptAt.Add(70 * time.Second)

	if got := reg.MatchingEntries(fullRange, servingFor(reg, "vimgym"), "bolt"); len(got) != 1 {
		t.Fatalf("a late sweep dropped a shard whose last-read beat was 9s old: %d shards routed, want 1", len(got))
	}
	if reg.IngestingShard(1, "vimgym") == nil {
		t.Fatal("a late sweep dropped a healthy shard from write routing")
	}
	if reg.Get("shard-elsewhere").actuallyStale(context.Background()) {
		t.Fatal("a late sweep would SUSPEND a shard whose last-read beat was 9s old")
	}

	// The next sweep no longer sees a beat: now the shard is dead.
	hbs.heartbeats = nil
	prov.adoptFromHeartbeats()
	if got := reg.MatchingEntries(fullRange, servingFor(reg, "vimgym"), "bolt"); len(got) != 0 {
		t.Fatalf("a sweep that saw no beat for 79s still routes the shard: %d shards", len(got))
	}
	if !reg.Get("shard-elsewhere").actuallyStale(context.Background()) {
		t.Fatal("a sweep that saw no beat for 79s should mark the shard stale")
	}
}
