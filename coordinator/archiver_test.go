package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/store"
)

func TestCoordinator_QuerySpansLiveAndCold(t *testing.T) {
	// Live shard with recent data.
	live, err := shard.NewDuckDBShard("shard-live", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { live.Close() })

	liveRows := []*pb.LogRow{
		{LineNum: 1, Raw: "live row 1", App: "vimgym", Level: "INFO", Customer: "kevlar", TsUnixNs: t1.Add(1 * time.Second).UnixNano()},
		{LineNum: 2, Raw: "live row 2", App: "vimgym", Level: "ERROR", Customer: "kevlar", TsUnixNs: t1.Add(2 * time.Second).UnixNano()},
	}
	if err := live.Ingest(liveRows); err != nil {
		t.Fatal(err)
	}

	// Cold shard with older data (exported to Parquet).
	coldDS, err := shard.NewDuckDBShard("shard-cold-src", "")
	if err != nil {
		t.Fatal(err)
	}
	coldRows := []*pb.LogRow{
		{LineNum: 10, Raw: "cold row 1", App: "vimgym", Level: "INFO", Customer: "kevlar", TsUnixNs: t0.Add(1 * time.Second).UnixNano()},
		{LineNum: 11, Raw: "cold row 2", App: "vimgym", Level: "ERROR", Customer: "kevlar", TsUnixNs: t0.Add(2 * time.Second).UnixNano()},
		{LineNum: 12, Raw: "cold row 3", App: "vimgym", Level: "WARN", Customer: "kevlar", TsUnixNs: t0.Add(3 * time.Second).UnixNano()},
	}
	if err := coldDS.Ingest(coldRows); err != nil {
		t.Fatal(err)
	}

	parquetPath := filepath.Join(t.TempDir(), "cold.parquet")
	if err := coldDS.ExportToParquet(parquetPath); err != nil {
		t.Fatal(err)
	}
	coldDS.Close()

	cold, err := shard.NewColdShard("shard-cold", []string{"vimgym"}, parquetPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cold.Close() })

	// Build coordinator with both shards.
	c := NewCoordinator(CoordinatorConfig{Store: testQueryStore(t, "kevlar", "vimgym")})
	c.Registry.Add(&ShardEntry{
		ID: "shard-live", Apps: []string{"vimgym"},
		WindowStart: t1, WindowEnd: t2, State: StateIngesting, Shard: live,
	})
	c.Registry.Add(&ShardEntry{
		ID: "shard-cold", Apps: []string{"vimgym"},
		WindowStart: t0, WindowEnd: t1, State: StateArchived, Shard: cold,
	})

	// Query spanning both windows.
	result, err := c.Query("cat vimgym.log", TimeRange{Start: t0, End: t2}, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}

	// Should get all 5 rows (3 cold + 2 live), sorted by timestamp.
	if len(result.Rows) != 5 {
		t.Fatalf("expected 5 rows (3 cold + 2 live), got %d", len(result.Rows))
	}

	// Verify all rows are present (ordering across shards is by line_num,
	// which is per-shard — not globally chronological).
	rawSet := map[string]bool{}
	for _, r := range result.Rows {
		rawSet[r.Raw] = true
	}
	for _, want := range []string{"cold row 1", "cold row 2", "cold row 3", "live row 1", "live row 2"} {
		if !rawSet[want] {
			t.Errorf("missing expected row %q", want)
		}
	}
}

func TestProvisioner_ArchivesExpiredShard(t *testing.T) {
	ds, err := shard.NewDuckDBShard("shard-old", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })

	rows := []*pb.LogRow{
		{LineNum: 1, Raw: "test row", App: "vimgym", Level: "INFO", Customer: "kevlar", TsUnixNs: t0.Add(1 * time.Second).UnixNano()},
	}
	if err := ds.Ingest(rows); err != nil {
		t.Fatal(err)
	}

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID: "shard-old", Apps: []string{"vimgym"}, BucketID: defaultBucketID,
		WindowStart: t0, WindowEnd: t1,
		State:     StateReadOnly,
		Shard:     ds,
		CreatedAt: time.Now().Add(-2 * time.Hour), // past retention
	})

	launcher := newLocalLauncher()
	t.Cleanup(func() { launcher.Close() })

	parquetDir := t.TempDir()
	config := ProvisionerConfig{
		PollInterval: 5 * time.Second,
		ParquetDir:   parquetDir,
	}

	prov := NewProvisioner(reg, launcher, config)
	prov.Buckets = bucketStub(0, time.Hour) // retention 1h; WindowEnd is > 1h past
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
	result, err := entry.Shard.Query("SELECT line_num, raw FROM logs WHERE customer = 'kevlar'")
	if err != nil {
		t.Fatalf("query archived shard: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 row from archived shard, got %d", len(result))
	}
}

func TestRegistry_PersistAndRestore(t *testing.T) {
	// Create a store.
	dsn := os.Getenv("WEFT_TEST_DSN")
	if dsn == "" {
		dsn = "host=localhost port=5433 dbname=weft user=weft password=weft sslmode=disable"
	}
	db, err := store.Open(dsn)
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
	ds.Ingest([]*pb.LogRow{{LineNum: 1, Raw: "persisted", App: "vimgym", Customer: "kevlar", TsUnixNs: t0.UnixNano()}})
	ds.ExportToParquet(parquetPath)
	ds.Close()

	cold, err := shard.NewColdShard(shardID, []string{"vimgym"}, parquetPath, nil)
	if err != nil {
		t.Fatal(err)
	}

	reg.Add(&ShardEntry{
		ID: shardID, Apps: []string{"vimgym"},
		BucketID:    defBucket.ID,
		WindowStart: t0, WindowEnd: t1,
		State:       StateArchived,
		ParquetPath: parquetPath,
		Shard:       cold,
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
			var apps []string
			json.Unmarshal([]byte(rec.Apps), &apps)
			return shard.NewColdShard(rec.ID, apps, rec.ParquetPath, nil)
		}
		return nil, fmt.Errorf("skip live shard %s", rec.ID)
	})
	if err != nil {
		t.Fatal(err)
	}

	// The archived shard should be restored. LoadFromStore skips rows
	// where the reconstruct func returns err, so Count() includes only
	// our test shard — not whatever other shard_records LocalStack /
	// the running coordinator has in the DB.
	if reg2.Count() != 1 {
		t.Fatalf("expected 1 restored shard, got %d", reg2.Count())
	}
	entry := reg2.Get(shardID)
	if entry == nil {
		t.Fatalf("%s not found after restore", shardID)
	}
	if entry.State != StateArchived {
		t.Fatalf("expected ARCHIVED, got %s", entry.State)
	}

	// Query should work on the restored ColdShard.
	rows, err := entry.Shard.Query("SELECT line_num, raw FROM logs WHERE customer = 'kevlar'")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row from restored shard, got %d", len(rows))
	}
}

func TestRegistry_MatchingShards_IncludesArchived(t *testing.T) {
	ds, err := shard.NewDuckDBShard("shard-arch", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID: "shard-arch", Apps: []string{"vimgym"},
		WindowStart: t0, WindowEnd: t1,
		State: StateArchived, Shard: ds,
	})

	matching := reg.MatchingShards(TimeRange{Start: t0, End: t1}, "vimgym", "bolt")
	if len(matching) != 1 {
		t.Fatalf("expected 1 archived shard in results, got %d", len(matching))
	}
}
