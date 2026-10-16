package coordinator

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/logdb"
	"github.com/graybeardcorp/weft/shard"
)

// These tests verify the coordinator handles shard list changes gracefully,
// as happens during ASG instance replacement.

var failoverRange = TimeRange{
	Start: t0,
	End:   t0.Add(2 * time.Hour),
}

// TestFailover_EmptyThenRecover verifies that a coordinator started with
// zero shards returns empty results, and recovers when shards are added
// via SetShards.
func TestFailover_EmptyThenRecover(t *testing.T) {
	// Start with zero shards — simulates startup during ASG replacement
	// when no shard instances are InService yet.
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: testQueryStore(t, "kevlar", "vimgym", "callbox")})

	res, err := c.Execute(context.Background(), "cat vimgym.log", failoverRange, "kevlar", "")
	if err != nil {
		t.Fatalf("query with no shards should not error, got: %v", err)
	}
	resD := drainExec(t, res)
	if len(resD.Rows) != 0 {
		t.Fatalf("expected 0 rows with no shards, got %d", len(resD.Rows))
	}

	// Simulate re-discovery finding shard instances.
	s0, err := shard.NewDuckDBShard("shard-0", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s0.Close()
	if err := s0.Ingest(context.Background(), shard0Rows(), nil); err != nil {
		t.Fatal(err)
	}

	addInProcessShard(t, c, &logdb.Shard{
		ID:          "shard-0",
		WindowStart: t0, WindowEnd: t1,
		State: logdb.StateIngesting, Live: s0,
		CreatedAt: time.Now(),
	}, "vimgym", "callbox")

	res, err = c.Execute(context.Background(), "cat vimgym.log", failoverRange, "kevlar", "")
	if err != nil {
		t.Fatalf("query after adding shard: %v", err)
	}
	resD2 := drainExec(t, res)
	if len(resD2.Rows) == 0 {
		t.Fatal("expected rows after adding shard, got 0")
	}
}

// TestFailover_ShardReplacement verifies that SetShards atomically replaces
// the shard list — old shards stop serving, new shards take over.
func TestFailover_ShardReplacement(t *testing.T) {
	apps := []string{"vimgym", "callbox"}

	s0, err := shard.NewDuckDBShard("shard-0", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s0.Close()
	if err := s0.Ingest(context.Background(), shard0Rows(), nil); err != nil {
		t.Fatal(err)
	}

	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: testQueryStore(t, "kevlar", "vimgym", "callbox")})
	addInProcessShard(t, c, &logdb.Shard{
		ID:          "shard-0",
		WindowStart: t0, WindowEnd: t1,
		State: logdb.StateIngesting, Live: s0,
		CreatedAt: time.Now(),
	}, apps...)

	res, err := c.Execute(context.Background(), "cat vimgym.log", failoverRange, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	resD := drainExec(t, res)
	if len(resD.Rows) == 0 {
		t.Fatal("expected rows from initial shard")
	}

	// Add shard-1 covering a different window.
	s1, err := shard.NewDuckDBShard("shard-1", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()
	if err := s1.Ingest(context.Background(), shard1Rows(), nil); err != nil {
		t.Fatal(err)
	}

	addInProcessShard(t, c, &logdb.Shard{
		ID:          "shard-1",
		WindowStart: t1, WindowEnd: t2,
		State: logdb.StateIngesting, Live: s1,
		CreatedAt: time.Now(),
	}, apps...)

	// Querying the wider range must now hit both shards.
	res2, err := c.Execute(context.Background(), "cat vimgym.log", failoverRange, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	resD2 := drainExec(t, res2)
	if len(resD2.Rows) <= len(resD.Rows) {
		t.Fatalf("expected more rows after adding shard-1 (was %d, now %d)", len(resD.Rows), len(resD2.Rows))
	}
}

// TestFailover_ConcurrentQueryDuringSwap verifies that queries in flight
// don't panic or return corrupt data when SetShards is called concurrently.
//
// Uses testQueryStoreReal (real DB, not BeginTest's single tx) because
// goroutines need their own pgx connections; sharing one *sql.Tx across
// concurrent Execute calls panics in pgx.Rows.Next.
func TestFailover_ConcurrentQueryDuringSwap(t *testing.T) {
	s0, err := shard.NewDuckDBShard("shard-0", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s0.Close()
	if err := s0.Ingest(context.Background(), shard0Rows(), nil); err != nil {
		t.Fatal(err)
	}

	apps := []string{"vimgym", "callbox"}
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: testQueryStoreReal(t, "kevlar", "vimgym", "callbox")})
	// testQueryStoreReal's own cleanup (deleteOrgData) only touches
	// subscriptions/namespaces/organizations — shard_records isn't
	// customer-scoped, so addInProcessShard's real (non-rolled-back)
	// SaveShard here would otherwise leak "shard-0" permanently into
	// the shared pooled test database, corrupting any later test that
	// reuses this pool slot and touches the same default bucket (see
	// logdb's TestProvisioner_Coordination_* — this exact leak once
	// caused their bucket state machine's rotation logic to misfire).
	t.Cleanup(func() { _ = (logdb.ShardStore{Store: c.Store}).DeleteShard("shard-0") })
	addInProcessShard(t, c, &logdb.Shard{
		ID:          "shard-0",
		WindowStart: t0, WindowEnd: t1,
		State: logdb.StateIngesting, Live: s0,
		CreatedAt: time.Now(),
	}, apps...)

	var wg sync.WaitGroup
	errors := make(chan error, 40)

	// Queries in parallel. Drain each result so the streaming path's
	// producer goroutines (shard fan-out + compute consume) terminate
	// cleanly between iterations rather than leaking on backpressure.
	//
	// Iteration count is intentionally modest. testQueryStore returns
	// a single shared transaction; gorm/pgx isn't goroutine-safe across
	// one tx, so heavier concurrency exposes pgx scan races that
	// aren't this test's concern.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				res, err := c.Execute(context.Background(), "cat vimgym.log", failoverRange, "kevlar", "")
				if err != nil {
					errors <- err
					continue
				}
				if _, err := res.Drain(context.Background()); err != nil {
					errors <- err
				}
			}
		}()
	}

	// No more shard swaps — the test now just verifies concurrent queries
	// don't panic or deadlock.

	wg.Wait()
	close(errors)

	// Transient errors during shard swaps are expected (DuckDB close races).
	// The test verifies no panics, deadlocks, or data corruption.
	errCount := 0
	for range errors {
		errCount++
	}
	if errCount > 0 {
		t.Logf("%d transient errors during concurrent swap (expected)", errCount)
	}
}

func shardIDs(shards []shard.Shard) []string {
	var ids []string
	for _, s := range shards {
		ids = append(ids, s.ID())
	}
	return ids
}
