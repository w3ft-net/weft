package logdb

import (
	"testing"
	"time"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/store"
)

// TestMatchingEntries_SkipsSuspended pins Issue 2: a SUSPENDED shard
// must never be returned for query routing.
//
// When a shard's instance goes stale, the provisioner sets
// State=SUSPENDED, Addr="", InstanceID="" — but does NOT clear the
// (now stale) e.Shard pointer. So MatchingEntries' "Addr=="" &&
// Shard==nil" guard doesn't skip it, and SUSPENDED wasn't in the
// state-exclusion list (only EXPIRING was). The entry was therefore
// routed, the descriptor builder took the non-archived branch
// (GrpcAddr = e.Addr = ""), and compute logged
// "no grpc_addr or lambda_url" (e.g. shard-20260511T0000-822eab40).
// ReconstructShard documents SUSPENDED as "instance dead, awaiting
// replacement. No shard to dial" — it is unqueryable by definition
// until it rebinds (→ INGESTING/READ_ONLY with a fresh addr) or is
// archived (→ ARCHIVED, queryable via Lambda).
//
// The SUSPENDED entry below is given a NON-empty Addr and a non-nil
// Shard on purpose: that proves it is excluded by *state*, not
// incidentally caught by the addr/shard guard.
func TestMatchingEntries_SkipsSuspended(t *testing.T) {
	now := time.Now()
	reg := NewShardRegistry()

	reg.Add(&ShardEntry{
		ID:          "shard-suspended",
		Services:    []store.ShardService{{NamespaceID: 1, Service: "vimgym"}},
		WindowStart: now.Add(-30 * time.Minute),
		WindowEnd:   now.Add(-20 * time.Minute),
		State:       StateSuspended,
		Addr:        "10.0.0.9:9000", // non-empty: must still be excluded
		RowCount:    500,
	})
	reg.Add(&ShardEntry{
		ID:          "shard-live",
		Services:    []store.ShardService{{NamespaceID: 1, Service: "vimgym"}},
		WindowStart: now.Add(-30 * time.Minute),
		WindowEnd:   now.Add(-20 * time.Minute),
		State:       StateIngesting,
		Addr:        "10.0.0.1:9000",
		RowCount:    10,
	})

	tr := TimeRange{Start: now.Add(-time.Hour), End: now}
	got := map[string]bool{}
	for _, e := range reg.MatchingEntries(tr, servingFor(reg, "vimgym"), store.Tier("")) {
		got[e.ID] = true
	}

	if got["shard-suspended"] {
		t.Error("SUSPENDED shard was returned for query routing — its instance is dead/awaiting rebind, " +
			"so this only produces the 'no grpc_addr or lambda_url' dial error")
	}
	if !got["shard-live"] {
		t.Error("healthy INGESTING shard in the same window was excluded — regression")
	}
}

// TestSuspend_RequiresConsecutiveFailures — one flaky RPC must not flip
// a shard to Suspended. That was the bug behind 2026-04-17's
// shard-count inflation (intermittent NLB-loop heartbeat failures →
// immediate suspend → provisioner replaces → orphan accumulation). The
// fix is a consecutive-failure counter that only trips at
// SuspendFailureThreshold, and any single success resets it.
//
// Drives RecordIngestFailure/ResetIngestFailures directly rather than
// going through Coordinator's drain loop; the drain loop's
// route-to-other-shards-on-failure behavior makes it hard to
// deterministically deliver N consecutive attempts to the same shard.
func TestSuspend_RequiresConsecutiveFailures(t *testing.T) {
	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID: "shard-a", State: StateIngesting, CreatedAt: time.Now(),
	})

	// First failure: recorded, but below threshold.
	reg.RecordIngestFailure("shard-a")
	if reg.Get("shard-a").Suspended {
		t.Fatal("one failure should not suspend")
	}

	// A successful ingest resets the counter.
	reg.ResetIngestFailures("shard-a")

	// Below-threshold run of consecutive failures — still healthy.
	for i := 0; i < SuspendFailureThreshold-1; i++ {
		reg.RecordIngestFailure("shard-a")
	}
	if reg.Get("shard-a").Suspended {
		t.Fatalf("suspended after only %d failures (threshold is %d)",
			SuspendFailureThreshold-1, SuspendFailureThreshold)
	}

	// One more failure trips the threshold.
	reg.RecordIngestFailure("shard-a")
	if !reg.Get("shard-a").Suspended {
		t.Fatalf("should be suspended after %d failures", SuspendFailureThreshold)
	}
}

// TestSuspended_ExcludedFromWrites_IncludedInQueries — once a shard is
// Suspended, both write-routing entry points (IngestingShard /
// DefaultIngestingShard) must skip it, but its data remains queryable
// via MatchingEntries until the lifecycle moves it to a non-queryable
// state. The Shard pointer stays attached so existing data can still
// be read.
func TestSuspended_ExcludedFromWrites_IncludedInQueries(t *testing.T) {
	ds, err := shard.NewDuckDBShard("shard-0", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID: "shard-0", Services: []store.ShardService{{NamespaceID: 1, Service: "vimgym"}},
		State: StateIngesting, Shard: ds,
		Suspended: true, WindowStart: t0, WindowEnd: t2, CreatedAt: time.Now(),
	})

	if sh := reg.IngestingShard(1, "vimgym"); sh != nil {
		t.Error("IngestingShard should skip suspended shard")
	}

	matching := reg.MatchingEntries(fullRange, servingFor(reg, "vimgym"), store.Tier("bolt"))
	if len(matching) != 1 {
		t.Errorf("MatchingEntries should include suspended shard, got %d", len(matching))
	}
}

// TestSuspend_ViaShardIngest verifies the Shard.Ingest →
// IngestRecorder → registry wiring end-to-end: a failing shard's
// Ingest calls feed RecordIngestFailure on the registry (passed in
// as the recorder), and after SuspendFailureThreshold calls the
// entry flips to Suspended. A healthy sibling, exercised in the
// same loop, stays unsuspended because each success resets its
// counter.
//
// This was the integration the old coordinator-side liveness test
// covered; moving it to logdb removes the drain loop from the
// picture but keeps the bit that actually matters — that Shard.Ingest
// drives the suspend counter through the recorder interface.
func TestSuspend_ViaShardIngest(t *testing.T) {
	reg := NewShardRegistry()
	shardA := &failingShard{id: "shard-a", failing: true}
	shardB := &failingShard{id: "shard-b"}
	reg.Add(&ShardEntry{ID: "shard-a", State: StateIngesting, Shard: shardA, CreatedAt: time.Now()})
	reg.Add(&ShardEntry{ID: "shard-b", State: StateIngesting, Shard: shardB, CreatedAt: time.Now()})

	// *ShardRegistry directly satisfies shard.IngestRecorder.
	rows := []*pb.LogRow{{Raw: "x", TsUnixNs: time.Now().UnixNano()}}
	for i := 0; i < SuspendFailureThreshold; i++ {
		_ = shardA.Ingest(rows, reg)
		_ = shardB.Ingest(rows, reg)
	}

	if !reg.Get("shard-a").Suspended {
		t.Errorf("shard-a should be Suspended after %d failed Ingest calls", SuspendFailureThreshold)
	}
	if reg.Get("shard-b").Suspended {
		t.Error("shard-b should not be Suspended — every Ingest succeeded")
	}
}

// TestSuspend_LastShardDown_NoRouting — when the only INGESTING
// shard for an app trips the suspend threshold, IngestingShard must
// stop returning it. Pins the drain-loop's "no shard owns this app"
// DLQ branch.
func TestSuspend_LastShardDown_NoRouting(t *testing.T) {
	reg := NewShardRegistry()
	shardA := &failingShard{id: "shard-a", failing: true}
	reg.Add(&ShardEntry{
		ID: "shard-a", Services: []store.ShardService{{NamespaceID: 1, Service: "weft/coordinator"}},
		State: StateIngesting, Shard: shardA, CreatedAt: time.Now(),
	})

	rows := []*pb.LogRow{{Raw: "x", TsUnixNs: time.Now().UnixNano()}}
	for i := 0; i < SuspendFailureThreshold; i++ {
		_ = shardA.Ingest(rows, reg)
	}

	if !reg.Get("shard-a").Suspended {
		t.Fatalf("shard-a should be Suspended after %d failed Ingest calls", SuspendFailureThreshold)
	}
	if sh := reg.IngestingShard(1, "weft/coordinator"); sh != nil {
		t.Errorf("IngestingShard should return nil for an app whose only shard is suspended; got %s", sh.ID())
	}
}
