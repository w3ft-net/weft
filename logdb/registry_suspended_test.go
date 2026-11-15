package logdb

import (
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/shardstore"
)

// TestMatchingEntries_SuspendedFailsTheQuery pins Issue 2 and its
// successor. A SUSPENDED shard must never be dialed: its instance is dead
// or awaiting rebind, so routing it only produced compute's "no grpc_addr
// or lambda_url" (e.g. shard-20260511T0000-822eab40). But it still holds
// rows for its window, so skipping it answered with the other shards'
// rows alone. The query fails instead, naming it.
//
// The SUSPENDED entry below is given a NON-empty Addr on purpose: that
// proves it is caught by *state*, not incidentally by the addr guard.
func TestMatchingEntries_SuspendedFailsTheQuery(t *testing.T) {
	now := time.Now()
	reg := NewShardRegistry()

	reg.Add(&Shard{
		ShardRow: ShardRow{
			ID:          "shard-suspended",
			WindowStart: now.Add(-30 * time.Minute),
			WindowEnd:   now.Add(-20 * time.Minute),
			State:       StateSuspended,
			Addr:        "10.0.0.9:9000",
			RowCount:    500,
		},
		Services: []shardstore.ShardService{{NamespaceID: 1, Service: "vimgym"}},
	})
	reg.Add(&Shard{
		ShardRow: ShardRow{
			ID:          "shard-live",
			WindowStart: now.Add(-30 * time.Minute),
			WindowEnd:   now.Add(-20 * time.Minute),
			State:       StateIngesting,
			Addr:        "10.0.0.1:9000",
			RowCount:    10,
		},
		Services: []shardstore.ShardService{{NamespaceID: 1, Service: "vimgym"}},
	})

	tr := TimeRange{Start: now.Add(-time.Hour), End: now}
	got, err := reg.MatchingEntries(tr, servingFor(reg, "vimgym"), time.Now().Add(-tierRetention("")))
	requireUnavailable(t, got, err, "shard-suspended")

	// Outside its window, or for a service it doesn't serve, the
	// SUSPENDED shard has nothing the query needs and doesn't fail it.
	before := TimeRange{Start: now.Add(-15 * time.Minute), End: now}
	if got := mustMatch(t, reg, before, servingFor(reg, "vimgym"), ""); len(got) != 0 {
		t.Fatalf("a range after both windows matched %d shards", len(got))
	}
	if got := mustMatch(t, reg, tr, servingFor(reg, "callbox"), ""); len(got) != 0 {
		t.Fatalf("a service neither shard serves matched %d shards", len(got))
	}
}

// A shard relaunching after SUSPENDED or UPGRADING reattaches its old
// volume: its rows exist but nothing can serve them yet. A fresh LAUNCHING
// shard has never ingested and holds nothing.
func TestMatchingEntries_RelaunchingShardFailsTheQuery(t *testing.T) {
	now := time.Now()
	reg := NewShardRegistry()
	mk := func(id string, rows int64) *Shard {
		return &Shard{
			ShardRow: ShardRow{
				ID:          id,
				WindowStart: now.Add(-30 * time.Minute),
				WindowEnd:   now.Add(10 * time.Minute),
				State:       StateLaunching,
				RowCount:    rows,
			},
			Services: []shardstore.ShardService{{NamespaceID: 1, Service: "vimgym"}},
		}
	}
	reg.Add(mk("shard-fresh", 0))
	reg.Add(mk("shard-relaunching", 500))

	tr := TimeRange{Start: now.Add(-time.Hour), End: now}
	got, err := reg.MatchingEntries(tr, servingFor(reg, "vimgym"), time.Now().Add(-tierRetention("")))
	requireUnavailable(t, got, err, "shard-relaunching")
}
