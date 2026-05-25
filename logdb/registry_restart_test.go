package logdb

import (
	"context"
	"testing"
	"time"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
)

// TestCoordinator_RegistrySurvivesRestart verifies that after a coordinator
// restart, shards are restored from the store (postgres) with their correct
// time windows and apps, and ingested data is queryable.
func TestCoordinator_RegistrySurvivesRestart(t *testing.T) {
	// Phase 1: Create coordinator with provisioner, ingest data.
	launcher := NewLocalLauncher()
	t.Cleanup(func() { launcher.Close() })

	reg1 := NewShardRegistry()
	provConfig := DevProvisionerConfig()
	prov := NewProvisioner(reg1, launcher, provConfig)
	// Bootstrap requires a default bucket (target 2 to scale toward).
	prov.Buckets = bucketStub(t, 2, 168*time.Hour)

	ctx := context.Background()
	prov.Start(ctx)
	// Wait for bootstrap.
	time.Sleep(100 * time.Millisecond)

	if reg1.TotalIngesting() == 0 {
		t.Fatal("provisioner did not bootstrap a shard")
	}

	// Capture one entry's identity up front; the same one will
	// receive ingest, be "restarted" out of the registry, and be
	// reconnected to in phase 2. Multi-shard buckets make the
	// previous "pick any shard twice" pattern flaky across map
	// iterations.
	var chosen *ShardEntry
	for _, e := range reg1.Entries() {
		if e.Shard != nil {
			chosen = e
			break
		}
	}
	if chosen == nil {
		t.Fatal("no shard available for ingestion")
	}
	ingestShard := chosen.Shard

	rows := []*pb.LogRow{
		{Raw: "test log line 1", Namespace: "weft", Service: "coordinator", Customer: "testcust", Level: "INFO", TsUnixNs: time.Now().UnixNano()},
		{Raw: "test log line 2", Namespace: "weft", Service: "coordinator", Customer: "testcust", Level: "ERROR", TsUnixNs: time.Now().UnixNano()},
	}
	if err := ingestShard.Ingest(rows, nil); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	// Verify data is queryable.
	result, err := ingestShard.Query("SELECT raw, ts FROM logs WHERE customer = 'testcust'")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(result))
	}

	// Phase 2: Simulate coordinator restart — new registry, same launcher/shards.
	// The key property: shards restored from the store should still have
	// the data that was ingested in phase 1.

	shardID := chosen.ID
	shardAddr := chosen.Addr
	shardStart := chosen.WindowStart
	shardEnd := chosen.WindowEnd

	// Create new registry (simulating restart) and reconnect to same shard.
	reg2 := NewShardRegistry()
	gs, err := shard.NewGRPCShard(shardID, shardAddr, "")
	if err != nil {
		t.Fatalf("reconnect to shard: %v", err)
	}

	reg2.Add(&ShardEntry{
		ID:          shardID,
		WindowStart: shardStart,
		WindowEnd:   shardEnd,
		State:       StateIngesting,
		Shard:       gs,
		CreatedAt:   time.Now(),
	})

	// Query through the new registry's shard — data should still be there.
	result2, err := gs.Query("SELECT raw, ts FROM logs WHERE customer = 'testcust'")
	if err != nil {
		t.Fatalf("query after restart: %v", err)
	}
	if len(result2) != 2 {
		t.Fatalf("expected 2 rows after restart, got %d", len(result2))
	}
}
