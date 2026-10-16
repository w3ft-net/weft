package logdb

import (
	"context"
	"testing"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
	"github.com/graybeardcorp/weft/store"
)

// servingFor builds the serving set a query for app would resolve to,
// mirroring the per-entry hasApp match that MatchingEntries used to do
// internally. Tests whose real subject is staleness / suspension /
// retention / archived-inclusion (not service routing) use this to keep
// expressing "a query scoped to app X" now that the serving-set lookup
// lives upstream of the pure registry filter. "" yields nil (no filter).
func servingFor(reg *ShardRegistry, app string) map[string]struct{} {
	if app == "" {
		return nil
	}
	out := map[string]struct{}{}
	for _, e := range reg.Entries() {
		if e.serves(1, app) {
			out[e.ID] = struct{}{}
		}
	}
	return out
}

// TestRegistry_ServingSetQueryRouting — MatchingEntries narrows to the
// shard-ID set the caller passes (resolved upstream from shard_services),
// a nil set hits every entry in the window, and an empty (non-nil) set
// matches nothing. Two shards in the same window; the test drives the
// serving filter directly, DB-free — the registry no longer does app
// matching itself.
func TestRegistry_ServingSetQueryRouting(t *testing.T) {
	sVim, err := shard.NewDuckDBShard("shard-vimgym", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sVim.Close() })

	sCb, err := shard.NewDuckDBShard("shard-callbox", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sCb.Close() })

	// Ingest one row each so the entries are non-empty; MatchingEntries
	// drops ShardRowCount==0 archived entries but ingesting entries are
	// returned regardless. A row each keeps the test honest if that
	// guard ever tightens.
	if err := sVim.Ingest(context.Background(), []*pb.LogRow{{Namespace: "vimgym", CustomerId: 1, TsUnixNs: t0.UnixNano()}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := sCb.Ingest(context.Background(), []*pb.LogRow{{Namespace: "callbox", CustomerId: 1, TsUnixNs: t0.UnixNano()}}, nil); err != nil {
		t.Fatal(err)
	}

	reg := NewShardRegistry()
	reg.Add(&Shard{
		ID: "shard-vimgym", Services: []store.ShardService{{NamespaceID: 1, Service: "vimgym"}},
		WindowStart: t0, WindowEnd: t2, State: StateIngesting, Live: sVim,
	})
	reg.Add(&Shard{
		ID: "shard-callbox", Services: []store.ShardService{{NamespaceID: 1, Service: "callbox"}},
		WindowStart: t0, WindowEnd: t2, State: StateIngesting, Live: sCb,
	})

	serving := func(ids ...string) map[string]struct{} {
		s := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			s[id] = struct{}{}
		}
		return s
	}

	matching := reg.MatchingEntries(fullRange, serving("shard-vimgym"), store.Tier("bolt"))
	if len(matching) != 1 {
		t.Fatalf("expected 1 shard for the vimgym serving set, got %d", len(matching))
	}
	if matching[0].ID != "shard-vimgym" {
		t.Fatalf("expected shard-vimgym, got %s", matching[0].ID)
	}

	matching = reg.MatchingEntries(fullRange, serving("shard-callbox"), store.Tier("bolt"))
	if len(matching) != 1 || matching[0].ID != "shard-callbox" {
		t.Fatalf("expected only shard-callbox, got %v", matching)
	}

	// Nil serving set = no service filter = all shards in the window.
	matching = reg.MatchingEntries(fullRange, nil, store.Tier("bolt"))
	if len(matching) != 2 {
		t.Fatalf("expected 2 shards for the nil (all-services) set, got %d", len(matching))
	}

	// Empty (non-nil) serving set = a service no shard serves = nothing.
	matching = reg.MatchingEntries(fullRange, serving(), store.Tier("bolt"))
	if len(matching) != 0 {
		t.Fatalf("expected 0 shards for the empty serving set, got %d", len(matching))
	}
}

// TestRegistry_DegradedExcludedFromWritesButQueryable pins the
// circuit-breaker routing contract: a DEGRADED shard is pulled from write
// routing (IngestingShard skips it) but stays read-queryable
// (MatchingEntries still returns it — its data is intact on a live
// instance).
func TestRegistry_DegradedExcludedFromWritesButQueryable(t *testing.T) {
	s, err := shard.NewDuckDBShard("shard-degraded", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Ingest(context.Background(), []*pb.LogRow{{Namespace: "vimgym", CustomerId: 1, TsUnixNs: t0.UnixNano()}}, nil); err != nil {
		t.Fatal(err)
	}

	reg := NewShardRegistry()
	reg.Add(&Shard{
		ID: "shard-degraded", Services: []store.ShardService{{NamespaceID: 1, Service: "vimgym"}},
		WindowStart: t0, WindowEnd: t2, State: StateDegraded, Live: s, RowCount: 1,
	})

	// Write routing skips it — no INGESTING candidate for vimgym.
	if got := reg.IngestingShard(1, "vimgym"); got != nil {
		t.Fatalf("IngestingShard returned a DEGRADED shard; want nil (excluded from writes)")
	}

	// Read routing still includes it.
	matching := reg.MatchingEntries(fullRange, servingFor(reg, "vimgym"), store.Tier("bolt"))
	if len(matching) != 1 || matching[0].ID != "shard-degraded" {
		t.Fatalf("DEGRADED shard not read-queryable: got %v", matching)
	}
}

// TestRegistry_IngestingShard — IngestingShard looks up the entry by
// app, with a per-shard Apps list. An app that no entry owns returns
// nil; multi-app entries serve all their apps.
func TestRegistry_IngestingShard(t *testing.T) {
	s0, err := shard.NewDuckDBShard("s0", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s0.Close() })

	reg := NewShardRegistry()
	reg.Add(&Shard{
		ID: "s0", Services: []store.ShardService{{NamespaceID: 1, Service: "vimgym"}, {NamespaceID: 1, Service: "grantcentral"}},
		WindowStart: t0, WindowEnd: t2, State: StateIngesting, Live: s0,
	})

	if sh := reg.IngestingShard(1, "vimgym"); sh == nil {
		t.Fatal("expected ingesting shard for vimgym")
	}
	if sh := reg.IngestingShard(1, "grantcentral"); sh == nil {
		t.Fatal("expected ingesting shard for grantcentral")
	}
	if sh := reg.IngestingShard(1, "unknown"); sh != nil {
		t.Fatal("expected nil for unknown app")
	}
}

// TestRegistry_ExpiringShardExcluded was removed alongside
// StateExpiring itself — the state was declared but never written
// under the prior model and is gone in the FSM rewrite. See
// docs/plans/asg-as-api-stateless.md.
