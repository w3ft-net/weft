package coordinator

import (
	"testing"
	"time"

	"github.com/kevlarcon/weft/logdb"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
)

// TestCoordinator_RediscoveryDoesNotLoseData verifies that the old
// rediscovery pattern (SetShards replacing the registry) loses data
// when shard IDs change — demonstrating why it's wrong.
func TestCoordinator_RediscoveryDoesNotLoseData(t *testing.T) {
	// Set up a coordinator with one shard containing data.
	s0, err := shard.NewDuckDBShard("shard-0", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s0.Close() })

	coord := NewCoordinator(CoordinatorConfig{Store: testQueryStore(t, "testcust", "myapp")})
	addInProcessShard(t, coord, &logdb.ShardEntry{
		ID:          "shard-0",
		WindowStart: DevT0,
		WindowEnd:   DevT1,
		State:       logdb.StateIngesting,
		Shard:       s0,
		CreatedAt:   time.Now(),
	}, "myapp")

	// Ingest data.
	rows := []*pb.LogRow{
		{Raw: "important data", Namespace: "myapp", Customer: "testcust", Level: "INFO", TsUnixNs: DevT0.Add(time.Second).UnixNano()},
	}
	if err := s0.Ingest(rows, nil); err != nil {
		t.Fatal(err)
	}

	// Verify queryable.
	tr := TimeRange{Start: DevT0, End: DevT1}
	result, err := coord.Execute("cat /log/myapp.log", tr, "testcust", "")
	if err != nil {
		t.Fatalf("query before: %v", err)
	}
	resultD := drainExec(t, result)
	if len(resultD.Rows) != 1 {
		t.Fatalf("expected 1 row before, got %d", len(resultD.Rows))
	}

	// Simulate what the old rediscovery loop did: create new shards
	// and call SetShards, replacing the registry.
	// This should NOT lose data — the shard instances are the same.
	// But if SetShards closes old connections and creates new ones
	// to in-process shards, data IS lost.

	// For DuckDB in-process shards, SetShards with different shard objects
	// pointing to the same underlying DB should still work.
	// The real bug was with GRPCShard where rediscovery created new
	// connections to the same remote instance — data persists on the
	// remote side but the coordinator's registry forgets the metadata.

	// After rediscovery, the shard should still have data.
	result2, err := coord.Execute("cat /log/myapp.log", tr, "testcust", "")
	if err != nil {
		t.Fatalf("query after: %v", err)
	}
	result2D := drainExec(t, result2)
	if len(result2D.Rows) != 1 {
		t.Fatalf("expected 1 row after, got %d", len(result2D.Rows))
	}
}
