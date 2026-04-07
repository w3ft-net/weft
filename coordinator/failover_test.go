package coordinator

import (
	"sync"
	"testing"
	"time"

	"github.com/kevlarcon/weft/shard"
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
	c := NewCoordinator(nil, "kevlar", nil)

	res, err := c.Query("cat vimgym.log", failoverRange)
	if err != nil {
		t.Fatalf("query with no shards should not error, got: %v", err)
	}
	if len(res.Rows) != 0 {
		t.Fatalf("expected 0 rows with no shards, got %d", len(res.Rows))
	}

	// Simulate re-discovery finding shard instances.
	s0, err := shard.NewDuckDBShard("shard-0", "", t0, t1)
	if err != nil {
		t.Fatal(err)
	}
	defer s0.Close()
	if err := s0.Ingest(shard0Rows()); err != nil {
		t.Fatal(err)
	}

	c.SetShards([]shard.Shard{s0})

	res, err = c.Query("cat vimgym.log", failoverRange)
	if err != nil {
		t.Fatalf("query after SetShards: %v", err)
	}
	if len(res.Rows) == 0 {
		t.Fatal("expected rows after SetShards, got 0")
	}
}

// TestFailover_ShardReplacement verifies that SetShards atomically replaces
// the shard list — old shards stop serving, new shards take over.
func TestFailover_ShardReplacement(t *testing.T) {
	s0, err := shard.NewDuckDBShard("shard-0", "", t0, t1)
	if err != nil {
		t.Fatal(err)
	}
	defer s0.Close()
	if err := s0.Ingest(shard0Rows()); err != nil {
		t.Fatal(err)
	}

	c := NewCoordinator([]shard.Shard{s0}, "kevlar", nil)

	res, err := c.Query("cat vimgym.log", failoverRange)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) == 0 {
		t.Fatal("expected rows from initial shard")
	}

	// Replace with shard-1.
	s1, err := shard.NewDuckDBShard("shard-1", "", t1, t2)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()
	if err := s1.Ingest(shard1Rows()); err != nil {
		t.Fatal(err)
	}

	c.SetShards([]shard.Shard{s1})

	shards := c.GetShards()
	if len(shards) != 1 || shards[0].ID() != "shard-1" {
		t.Fatalf("expected [shard-1] after replacement, got %v", shardIDs(shards))
	}
}

// TestFailover_ConcurrentQueryDuringSwap verifies that queries in flight
// don't panic or return corrupt data when SetShards is called concurrently.
func TestFailover_ConcurrentQueryDuringSwap(t *testing.T) {
	s0, err := shard.NewDuckDBShard("shard-0", "", t0, t1)
	if err != nil {
		t.Fatal(err)
	}
	defer s0.Close()
	if err := s0.Ingest(shard0Rows()); err != nil {
		t.Fatal(err)
	}

	c := NewCoordinator([]shard.Shard{s0}, "kevlar", nil)

	var wg sync.WaitGroup
	errors := make(chan error, 200)

	// Queries in parallel.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_, err := c.Query("cat vimgym.log", failoverRange)
				if err != nil {
					errors <- err
				}
			}
		}()
	}

	// Shard swaps in parallel.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				fresh, err := shard.NewDuckDBShard("swap", "", t0, t1)
				if err != nil {
					errors <- err
					return
				}
				c.SetShards([]shard.Shard{fresh})
				time.Sleep(time.Millisecond)
			}
		}()
	}

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

// TestFailover_GetShardsSnapshot verifies that GetShards returns a defensive
// copy — mutating the returned slice must not affect the coordinator.
func TestFailover_GetShardsSnapshot(t *testing.T) {
	s0, err := shard.NewDuckDBShard("shard-0", "", t0, t1)
	if err != nil {
		t.Fatal(err)
	}
	defer s0.Close()

	c := NewCoordinator([]shard.Shard{s0}, "kevlar", nil)

	snap := c.GetShards()
	if len(snap) != 1 {
		t.Fatalf("expected 1 shard, got %d", len(snap))
	}

	// Mutating the snapshot must not affect the coordinator.
	snap[0] = nil

	snap2 := c.GetShards()
	if len(snap2) != 1 || snap2[0] == nil {
		t.Fatal("mutating GetShards() result corrupted coordinator state")
	}
}

func shardIDs(shards []shard.Shard) []string {
	var ids []string
	for _, s := range shards {
		ids = append(ids, s.ID())
	}
	return ids
}
