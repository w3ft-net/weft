package logdb

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/shardstore"

	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shard"
)

func TestRegistry_PersistAndRestore(t *testing.T) {
	db := openSMTestStore(t)
	seedDefaultBucket(t, db, 1, time.Hour)
	defBucket, ok, err := db.GetBucketByLabel("default")
	if err != nil || !ok {
		t.Fatalf("default bucket lookup: ok=%v err=%v", ok, err)
	}
	shardID := "shard-archived-" + fmt.Sprintf("%d", time.Now().UnixNano())

	// Create registry with store, add an archived shard.
	reg := NewShardRegistry()
	reg.SetStore(db)

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
		ShardRow: ShardRow{
			ID:          shardID,
			BucketID:    defBucket.ID,
			WindowStart: t0,
			WindowEnd:   t1,
			State:       StateArchived,
			ParquetPath: parquetPath,
			RowCount:    1,
			CreatedAt:   time.Now(),
		},
		Services: []shardstore.ShardService{{NamespaceID: 1, Service: "vimgym"}},
		Live:     cold,
	})

	// Create a NEW registry, load from the same store.
	reg2 := NewShardRegistry()
	reg2.SetStore(db)
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

	// LoadFromStore loads every record (live or archived); the reconstruct
	// func only decides which entries get a hot Shard pointer.
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
		ShardRow: ShardRow{
			ID:          "shard-arch",
			WindowStart: t0,
			WindowEnd:   t1,
			State:       StateArchived,
			RowCount:    1,
		},
		Services: []shardstore.ShardService{{NamespaceID: 1, Service: "vimgym"}},
		Live:     ds,
	})

	matching := mustMatch(t, reg, TimeRange{Start: t0, End: t1}, servingFor(reg, "vimgym"), "bolt")
	if len(matching) != 1 {
		t.Fatalf("expected 1 archived shard in results, got %d", len(matching))
	}
}
