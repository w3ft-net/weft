package logdb

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
	"github.com/graybeardcorp/weft/store"
)

func TestRegistry_PersistAndRestore(t *testing.T) {
	// Create a store.
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	// t.Cleanup, not defer: a plain defer here runs (closing db) before
	// t.Cleanup callbacks fire, so a t.Cleanup(DeleteShard) registered
	// below would run its query against an already-closed connection —
	// DeleteShard's error is silently discarded (`_ =`), and the row
	// (plus its cascade-linked shard_services row) leaks into the
	// shared pooled test database forever, corrupting any later test
	// that touches the same "default" bucket over a real (non-
	// transactional) connection. t.Cleanup runs LIFO, so registering
	// this before DeleteShard's cleanup below makes DeleteShard run
	// first and Close second, in the correct order.
	t.Cleanup(func() { _ = db.Close() })

	// The shard_records table has a NOT NULL FK on bucket_id since
	// migration 020. Look up the default bucket (seeded by that
	// migration) so the test entry stays valid.
	defBucket, ok, err := db.GetBucketByLabel("default")
	if err != nil || !ok {
		t.Fatalf("default bucket lookup: ok=%v err=%v", ok, err)
	}
	shardID := "shard-archived-" + fmt.Sprintf("%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = ShardStore{db}.DeleteShard(shardID) })

	// Create registry with store, add an archived shard.
	reg := NewShardRegistry()
	reg.SetStore(ShardStore{db})

	parquetPath := filepath.Join(t.TempDir(), "test.parquet")
	// Create a Parquet file so ColdShard can open it.
	ds, err := shard.NewDuckDBShard("tmp", "")
	if err != nil {
		t.Fatal(err)
	}
	ds.Ingest(context.Background(), []*pb.LogRow{{Raw: "persisted", Namespace: "vimgym", CustomerId: 1, TsUnixNs: t0.UnixNano()}}, nil)
	ds.ExportToParquet(parquetPath)
	ds.Close()

	cold, err := shard.NewColdShard(shardID, []string{"vimgym"}, parquetPath, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	reg.Add(&Shard{
		ID: shardID, Services: []store.ShardService{{NamespaceID: 1, Service: "vimgym"}},
		BucketID:    defBucket.ID,
		WindowStart: t0, WindowEnd: t1,
		State:       StateArchived,
		ParquetPath: parquetPath,
		Live:        cold,
		RowCount:    1,
		CreatedAt:   time.Now(),
	})

	// Create a NEW registry, load from the same store.
	reg2 := NewShardRegistry()
	reg2.SetStore(ShardStore{db})
	err = reg2.LoadFromStore(func(rec *Shard) (shard.Shard, error) {
		if rec.ID != shardID {
			return nil, fmt.Errorf("skip unrelated shard %s", rec.ID)
		}
		if rec.State == StateArchived && rec.ParquetPath != "" {
			return shard.NewColdShard(rec.ID, nil, rec.ParquetPath, "", nil)
		}
		return nil, fmt.Errorf("skip live shard %s", rec.ID)
	})
	if err != nil {
		t.Fatal(err)
	}

	// LoadFromStore now loads every record (live or archived); the
	// reconstruct func only decides which entries get a hot Shard
	// pointer. We can't assert reg2.Count() == 1 anymore — the
	// shared dev DB legitimately has other live rows from concurrent
	// integration tests / a running coordinator. Just check our row
	// landed and was reconstructed correctly.
	entry := reg2.Get(shardID)
	if entry == nil {
		t.Fatalf("%s not found after restore", shardID)
	}
	if entry.State != StateArchived {
		t.Fatalf("expected ARCHIVED, got %s", entry.State)
	}
	if entry.Live == nil {
		t.Fatalf("%s loaded without a Shard pointer; reconstruct should attach for ARCHIVED rows", shardID)
	}

	// Query should work on the restored ColdShard.
	rows, err := entry.Live.Query(context.Background(), "SELECT raw, ts FROM logs WHERE customer_id = 1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row from restored shard, got %d", len(rows))
	}
}

func TestRegistry_MatchingEntries_IncludesArchived(t *testing.T) {
	ds, err := shard.NewDuckDBShard("shard-arch", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })

	reg := NewShardRegistry()
	reg.Add(&Shard{
		ID: "shard-arch", Services: []store.ShardService{{NamespaceID: 1, Service: "vimgym"}},
		WindowStart: t0, WindowEnd: t1,
		State: StateArchived, Live: ds, RowCount: 1,
	})

	matching := reg.MatchingEntries(TimeRange{Start: t0, End: t1}, servingFor(reg, "vimgym"), "bolt")
	if len(matching) != 1 {
		t.Fatalf("expected 1 archived shard in results, got %d", len(matching))
	}
}
