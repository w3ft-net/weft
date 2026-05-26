package logdb

import (
	"testing"
	"time"

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
