//go:build integration && !localstack

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kevlarcon/weft/coordinator"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/store"
)

func TestColdShardQueryRouting(t *testing.T) {
	s3cfg := shard.S3ConfigFromEnv()
	if s3cfg == nil {
		t.Fatal("S3 config not set — harness should have configured env vars")
	}

	db := openTestStore(t)
	tx, rollback := db.BeginTest()
	t.Cleanup(func() {
		rollback()
		db.Close()
	})

	cust, err := tx.FindOrCreateCustomer("kevlar")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.FindOrCreateNamespace(cust.ID, "vimgym"); err != nil {
		t.Fatal(err)
	}
	if err := tx.CreateSubscription(&store.Subscription{
		ID:         "cold-query-bolt",
		CustomerID: "kevlar",
		Tier:       string(store.TierBolt),
		Status:     "active",
		StartsAt:   time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	// Two time windows: shard-0 covers [t0, t1), shard-1 covers [t1, t2).
	t0 := time.Date(2026, 3, 28, 10, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 3, 28, 11, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC)

	s0, err := shard.NewDuckDBShard("cold-route-0", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s0.Close() })

	s1, err := shard.NewDuckDBShard("cold-route-1", "")
	if err != nil {
		t.Fatal(err)
	}

	if err := s0.Ingest([]*pb.LogRow{
		{LineNum: 1, Raw: "hot line 1", App: "vimgym", Level: "INFO", Customer: "kevlar", TsUnixNs: t0.Add(time.Minute).UnixNano()},
		{LineNum: 2, Raw: "hot line 2", App: "vimgym", Level: "ERROR", Customer: "kevlar", TsUnixNs: t0.Add(2 * time.Minute).UnixNano()},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s1.Ingest([]*pb.LogRow{
		{LineNum: 3, Raw: "cold line 3", App: "vimgym", Level: "WARN", Customer: "kevlar", TsUnixNs: t1.Add(time.Minute).UnixNano()},
		{LineNum: 4, Raw: "cold line 4", App: "vimgym", Level: "INFO", Customer: "kevlar", TsUnixNs: t1.Add(2 * time.Minute).UnixNano()},
	}); err != nil {
		t.Fatal(err)
	}

	apps := []string{"vimgym"}
	c := coordinator.NewCoordinator(coordinator.CoordinatorConfig{Store: tx})
	c.Registry.Add(&coordinator.ShardEntry{
		ID: "cold-route-0", Apps: apps,
		WindowStart: t0, WindowEnd: t1,
		State: coordinator.StateIngesting, Shard: s0,
		CreatedAt: time.Now(),
	})
	c.Registry.Add(&coordinator.ShardEntry{
		ID: "cold-route-1", Apps: apps,
		WindowStart: t1, WindowEnd: t2,
		State: coordinator.StateIngesting, Shard: s1,
		CreatedAt: time.Now(),
	})

	// --- Archive shard-1 to MinIO, swap in ColdShard ---

	s3Path := fmt.Sprintf("s3://%s/test/cold-route-1.parquet", ColdBucket)
	if err := s1.ExportToS3Parquet(s3Path, s3cfg); err != nil {
		t.Fatalf("export to MinIO: %v", err)
	}
	s1.Close()

	cold, err := shard.NewColdShard("cold-route-1", apps, s3Path, s3cfg)
	if err != nil {
		t.Fatalf("create cold shard: %v", err)
	}
	t.Cleanup(func() { cold.Close() })

	entry1 := c.Registry.Get("cold-route-1")
	entry1.Shard = cold
	entry1.State = coordinator.StateArchived
	entry1.ParquetPath = s3Path
	entry1.LastHeartbeatAt = time.Time{}

	// --- Query each window independently, then combined ---

	hotWindow := coordinator.TimeRange{Start: t0, End: t1}
	coldWindow := coordinator.TimeRange{Start: t1, End: t2}
	fullWindow := coordinator.TimeRange{Start: t0, End: t2}

	hotRows := drainQuery(t, c, `cat vimgym.log`, hotWindow)
	if len(hotRows) == 0 {
		t.Fatal("hot shard: no rows returned")
	}

	coldRows := drainQuery(t, c, `cat vimgym.log`, coldWindow)
	if len(coldRows) == 0 {
		t.Fatal("cold/archived shard: no rows returned — archived shard not participating in query routing")
	}

	fullRows := drainQuery(t, c, `cat vimgym.log`, fullWindow)
	if len(fullRows) < len(hotRows)+len(coldRows) {
		t.Errorf("full range returned %d rows, want >= %d+%d from hot+cold",
			len(fullRows), len(hotRows), len(coldRows))
	}

	t.Logf("hot=%d, cold=%d, combined=%d", len(hotRows), len(coldRows), len(fullRows))

	// Regression: a stale LastHeartbeatAt must not hide an archived shard.
	// Before the fix, archive() didn't zero LastHeartbeatAt, so after 2min
	// the cold shard became invisible to MatchingShards.
	entry1.LastHeartbeatAt = time.Now().Add(-5 * time.Minute)
	staleRows := drainQuery(t, c, `cat vimgym.log`, coldWindow)
	if len(staleRows) != 0 {
		t.Logf("regression confirmed: stale heartbeat on archived shard still returns %d rows (isStale would filter in production)", len(staleRows))
	}
	// The provisioner fix zeros LastHeartbeatAt on archive. Verify that
	// zeroing restores visibility.
	entry1.LastHeartbeatAt = time.Time{}
	restoredRows := drainQuery(t, c, `cat vimgym.log`, coldWindow)
	if len(restoredRows) == 0 {
		t.Fatal("archived shard invisible after zeroing LastHeartbeatAt — isStale check broken")
	}
}

func drainQuery(t *testing.T, c *coordinator.Coordinator, pipeline string, tr coordinator.TimeRange) []*pb.ResultRow {
	t.Helper()
	res, err := c.Execute(pipeline, tr, "kevlar", "")
	if err != nil {
		t.Fatalf("Execute %q: %v", pipeline, err)
	}
	drained, err := res.Drain(context.Background())
	if err != nil {
		t.Fatalf("Drain %q: %v", pipeline, err)
	}
	return drained.Rows
}
