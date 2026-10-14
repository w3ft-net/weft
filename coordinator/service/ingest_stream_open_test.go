package service

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/coordinator"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/store"
)

// TestIngestStream_SteadyStateOpenTouchesNoAuthTables pins the point of the
// "Stream-open cost" work in docs/plans/weftd-offset-checkpointing.md: once
// a token is warm, opening an IngestStream must not touch the database for
// auth (agent_tokens + the two Preload targets) or the last_seen stamp.
// Ack cycling opens a stream per active file every commit_interval, so
// anything per-open here multiplies by hosts x files.
func TestIngestStream_SteadyStateOpenTouchesNoAuthTables(t *testing.T) {
	tx, cleanup := testStore(t)
	defer cleanup()

	cust, err := tx.FindOrCreateOrg("kevlar")
	if err != nil {
		t.Fatal(err)
	}
	tx.SeedDevSubscription(cust.ID)
	ns, _ := tx.FindOrCreateNamespace(cust.ID, "opencost")
	rawTok, tok, err := tx.CreateAgentToken(cust.ID, ns.ID, "open-cost-agent", "")
	if err != nil {
		t.Fatal(err)
	}

	tx.EnableAuthCache()
	seen := store.NewAgentSeenTracker(tx)

	coord := coordinator.NewCoordinator(coordinator.CoordinatorConfig{Compute: &compute.Engine{}, Store: tx})
	srv := &Server{Coord: coord, Store: tx, AgentSeen: seen}

	var stmts atomic.Int64
	remove := tx.OnSQLForTest("count_open_cost", func(sql string) {
		for _, tbl := range []string{`"agent_tokens"`, `"organizations"`, `"namespaces"`} {
			if strings.Contains(sql, tbl) {
				stmts.Add(1)
				return
			}
		}
	})
	defer remove()

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"authorization", "Bearer "+rawTok,
		"weft-client", "weftd/0.99.0",
	))
	open := func() {
		st := &fakeIngestStream{ctx: ctx, rows: []*pb.LogRow{{Raw: "hello"}}}
		if err := srv.IngestStream(st); err != nil {
			t.Fatalf("IngestStream: %v", err)
		}
	}

	open() // cold: pays for the lookup
	if stmts.Load() == 0 {
		t.Fatal("cold open issued no auth statements; counter is not wired")
	}
	warm := stmts.Load()
	for i := 0; i < 10; i++ {
		open()
	}
	if extra := stmts.Load() - warm; extra != 0 {
		t.Errorf("10 warm stream opens issued %d auth/stamp statements, want 0", extra)
	}

	// The stamp is deferred, not dropped: a flush lands it.
	if err := seen.Flush(); err != nil {
		t.Fatal(err)
	}
	var got store.AgentToken
	if err := tx.DB().WithContext(store.WithoutTenantScope(context.Background())).First(&got, tok.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.LastSeenAt == nil || time.Since(*got.LastSeenAt) > time.Minute {
		t.Errorf("last_seen_at not stamped after flush: %v", got.LastSeenAt)
	}
}
