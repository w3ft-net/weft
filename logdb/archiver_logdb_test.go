package logdb

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/store"
)

func TestProvisioner_ArchivesExpiredShard(t *testing.T) {
	ds, err := shard.NewDuckDBShard("shard-old", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })

	rows := []*pb.LogRow{
		{Raw: "test row", Namespace: "vimgym", Level: "INFO", Customer: "kevlar", TsUnixNs: t0.Add(1 * time.Second).UnixNano()},
	}
	if err := ds.Ingest(rows, nil); err != nil {
		t.Fatal(err)
	}

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID: "shard-old", Services: []store.ShardService{{NamespaceID: 1, Service: "vimgym"}}, BucketID: 1,
		WindowStart: t0, WindowEnd: t1,
		State:     StateReadOnly,
		Shard:     ds,
		RowCount:  1,
		CreatedAt: time.Now().Add(-2 * time.Hour), // past retention
	})

	launcher := NewLocalLauncher()
	t.Cleanup(func() { launcher.Close() })

	parquetDir := t.TempDir()
	config := ProvisionerConfig{
		PollInterval: 5 * time.Second,
		ParquetDir:   parquetDir,
	}

	prov := NewProvisioner(reg, launcher, config)
	prov.Buckets = bucketStub(t, 0, time.Hour) // retention 1h; WindowEnd is > 1h past
	prov.check(context.Background())

	// Verify state changed to ARCHIVED.
	entry := reg.Get("shard-old")
	if entry == nil {
		t.Fatal("shard-old entry missing from registry")
	}
	if entry.State != StateArchived {
		t.Fatalf("expected ARCHIVED, got %s", entry.State)
	}

	// Verify the ColdShard is queryable.
	result, err := entry.Shard.Query("SELECT raw, ts FROM logs WHERE customer = 'kevlar'")
	if err != nil {
		t.Fatalf("query archived shard: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 row from archived shard, got %d", len(result))
	}
}

func TestRegistry_PersistAndRestore(t *testing.T) {
	// Create a store.
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	defer db.Close()

	// The shard_records table has a NOT NULL FK on bucket_id since
	// migration 020. Look up the default bucket (seeded by that
	// migration) so the test entry stays valid.
	defBucket, ok, err := db.GetBucketByLabel("default")
	if err != nil || !ok {
		t.Fatalf("default bucket lookup: ok=%v err=%v", ok, err)
	}
	shardID := "shard-archived-" + fmt.Sprintf("%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = db.DeleteShard(shardID) })

	// Create registry with store, add an archived shard.
	reg := NewShardRegistry()
	reg.SetStore(db)

	parquetPath := filepath.Join(t.TempDir(), "test.parquet")
	// Create a Parquet file so ColdShard can open it.
	ds, err := shard.NewDuckDBShard("tmp", "")
	if err != nil {
		t.Fatal(err)
	}
	ds.Ingest([]*pb.LogRow{{Raw: "persisted", Namespace: "vimgym", Customer: "kevlar", TsUnixNs: t0.UnixNano()}}, nil)
	ds.ExportToParquet(parquetPath)
	ds.Close()

	cold, err := shard.NewColdShard(shardID, []string{"vimgym"}, parquetPath, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	reg.Add(&ShardEntry{
		ID: shardID, Services: []store.ShardService{{NamespaceID: 1, Service: "vimgym"}},
		BucketID:    defBucket.ID,
		WindowStart: t0, WindowEnd: t1,
		State:       StateArchived,
		ParquetPath: parquetPath,
		Shard:       cold,
		RowCount:    1,
		CreatedAt:   time.Now(),
	})

	// Create a NEW registry, load from the same store.
	reg2 := NewShardRegistry()
	reg2.SetStore(db)
	err = reg2.LoadFromStore(func(rec store.ShardRecord) (shard.Shard, error) {
		if rec.ID != shardID {
			return nil, fmt.Errorf("skip unrelated shard %s", rec.ID)
		}
		if rec.State == string(StateArchived) && rec.ParquetPath != "" {
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
	if entry.Shard == nil {
		t.Fatalf("%s loaded without a Shard pointer; reconstruct should attach for ARCHIVED rows", shardID)
	}

	// Query should work on the restored ColdShard.
	rows, err := entry.Shard.Query("SELECT raw, ts FROM logs WHERE customer = 'kevlar'")
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
	reg.Add(&ShardEntry{
		ID: "shard-arch", Services: []store.ShardService{{NamespaceID: 1, Service: "vimgym"}},
		WindowStart: t0, WindowEnd: t1,
		State: StateArchived, Shard: ds, RowCount: 1,
	})

	matching := reg.MatchingEntries(TimeRange{Start: t0, End: t1}, servingFor(reg, "vimgym"), "bolt")
	if len(matching) != 1 {
		t.Fatalf("expected 1 archived shard in results, got %d", len(matching))
	}
}
