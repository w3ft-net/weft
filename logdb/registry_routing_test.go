package logdb

import (
	"testing"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/store"
)

// TestRegistry_AppScopedQueryRouting — MatchingEntries narrows by App,
// and the empty-app sentinel hits every entry in the window. Two
// shards, each scoped to a different app; ingest is irrelevant here,
// the test just checks which entries the matcher returns.
func TestRegistry_AppScopedQueryRouting(t *testing.T) {
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
	if err := sVim.Ingest([]*pb.LogRow{{App: "vimgym", Customer: "kevlar", TsUnixNs: t0.UnixNano()}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := sCb.Ingest([]*pb.LogRow{{App: "callbox", Customer: "kevlar", TsUnixNs: t0.UnixNano()}}, nil); err != nil {
		t.Fatal(err)
	}

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID: "shard-vimgym", Apps: []string{"vimgym"},
		WindowStart: t0, WindowEnd: t2, State: StateIngesting, Shard: sVim,
	})
	reg.Add(&ShardEntry{
		ID: "shard-callbox", Apps: []string{"callbox"},
		WindowStart: t0, WindowEnd: t2, State: StateIngesting, Shard: sCb,
	})

	matching := reg.MatchingEntries(fullRange, "vimgym", store.Tier("bolt"))
	if len(matching) != 1 {
		t.Fatalf("expected 1 shard for vimgym, got %d", len(matching))
	}
	if matching[0].ID != "shard-vimgym" {
		t.Fatalf("expected shard-vimgym, got %s", matching[0].ID)
	}

	matching = reg.MatchingEntries(fullRange, "callbox", store.Tier("bolt"))
	if len(matching) != 1 {
		t.Fatalf("expected 1 shard for callbox, got %d", len(matching))
	}

	// Empty app = all apps in the window.
	matching = reg.MatchingEntries(fullRange, "", store.Tier("bolt"))
	if len(matching) != 2 {
		t.Fatalf("expected 2 shards for all apps, got %d", len(matching))
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
	reg.Add(&ShardEntry{
		ID: "s0", Apps: []string{"vimgym", "grantcentral"},
		WindowStart: t0, WindowEnd: t2, State: StateIngesting, Shard: s0,
	})

	if sh := reg.IngestingShard("vimgym"); sh == nil {
		t.Fatal("expected ingesting shard for vimgym")
	}
	if sh := reg.IngestingShard("grantcentral"); sh == nil {
		t.Fatal("expected ingesting shard for grantcentral")
	}
	if sh := reg.IngestingShard("unknown"); sh != nil {
		t.Fatal("expected nil for unknown app")
	}
}

// TestRegistry_ExpiringShardExcluded was removed alongside
// StateExpiring itself — the state was declared but never written
// under the prior model and is gone in the FSM rewrite. See
// docs/plans/asg-as-api-stateless.md.
