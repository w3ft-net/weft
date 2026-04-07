package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/kevlarcon/weft/configstore"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
)

func TestCoordinator_QuerySpansLiveAndCold(t *testing.T) {
	// Live shard with recent data.
	live, err := shard.NewDuckDBShard("shard-live", "", t1, t2)
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
	coldDS, err := shard.NewDuckDBShard("shard-cold-src", "", t0, t1)
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

	cold, err := shard.NewColdShard("shard-cold", []string{"vimgym"}, parquetPath, t0, t1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cold.Close() })

	// Build coordinator with both shards.
	c := NewCoordinator(nil, "kevlar", nil)
	c.Registry.Add(&ShardEntry{
		ID: "shard-live", Apps: []string{"vimgym"},
		WindowStart: t1, WindowEnd: t2, State: StateIngesting, Shard: live,
	})
	c.Registry.Add(&ShardEntry{
		ID: "shard-cold", Apps: []string{"vimgym"},
		WindowStart: t0, WindowEnd: t1, State: StateArchived, Shard: cold,
	})

	// Query spanning both windows.
	result, err := c.Query("cat vimgym.log", TimeRange{Start: t0, End: t2})
	if err != nil {
		t.Fatal(err)
	}

	// Should get all 5 rows (3 cold + 2 live), sorted by timestamp.
	if len(result.Rows) != 5 {
		t.Fatalf("expected 5 rows (3 cold + 2 live), got %d", len(result.Rows))
	}

	// Verify ordering — cold rows first (earlier timestamps).
	if result.Rows[0].Raw != "cold row 1" {
		t.Errorf("expected first row to be cold, got %s", result.Rows[0].Raw)
	}
	if result.Rows[4].Raw != "live row 2" {
		t.Errorf("expected last row to be live, got %s", result.Rows[4].Raw)
	}
}

func TestProvisioner_ArchivesExpiredShard(t *testing.T) {
	ds, err := shard.NewDuckDBShard("shard-old", "", t0, t1)
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
		ID: "shard-old", Apps: []string{"vimgym"},
		WindowStart: t0, WindowEnd: t1,
		State:     StateReadOnly,
		Shard:     ds,
		CreatedAt: time.Now().Add(-2 * time.Hour), // past retention
	})

	launcher := NewLocalLauncher()
	t.Cleanup(func() { launcher.Close() })

	parquetDir := t.TempDir()
	config := ProvisionerConfig{
		MaxRows:         5_000_000,
		MaxDuration:     24 * time.Hour,
		PollInterval:    5 * time.Second,
		RetentionPeriod: 1 * time.Hour, // shard is 2h old → should archive
		ParquetDir:      parquetDir,
	}

	prov := NewProvisioner(reg, launcher, config)
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
	result, err := entry.Shard.Query("SELECT * FROM logs WHERE customer = 'kevlar'")
	if err != nil {
		t.Fatalf("query archived shard: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 row from archived shard, got %d", len(result))
	}
}

func TestRegistry_PersistAndRestore(t *testing.T) {
	// Create a store.
	store, err := configstore.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Create registry with store, add an archived shard.
	reg := NewShardRegistry()
	reg.SetStore(store)

	parquetPath := filepath.Join(t.TempDir(), "test.parquet")
	// Create a Parquet file so ColdShard can open it.
	ds, err := shard.NewDuckDBShard("tmp", "", t0, t1)
	if err != nil {
		t.Fatal(err)
	}
	ds.Ingest([]*pb.LogRow{{LineNum: 1, Raw: "persisted", App: "vimgym", Customer: "kevlar", TsUnixNs: t0.UnixNano()}})
	ds.ExportToParquet(parquetPath)
	ds.Close()

	cold, err := shard.NewColdShard("shard-archived", []string{"vimgym"}, parquetPath, t0, t1)
	if err != nil {
		t.Fatal(err)
	}

	reg.Add(&ShardEntry{
		ID: "shard-archived", Apps: []string{"vimgym"},
		WindowStart: t0, WindowEnd: t1,
		State:       StateArchived,
		ParquetPath: parquetPath,
		Shard:       cold,
		CreatedAt:   time.Now(),
	})

	// Create a NEW registry, load from the same store.
	reg2 := NewShardRegistry()
	reg2.SetStore(store)
	err = reg2.LoadFromStore(func(rec configstore.ShardRecord) (shard.Shard, error) {
		if rec.State == string(StateArchived) && rec.ParquetPath != "" {
			var apps []string
			json.Unmarshal([]byte(rec.Apps), &apps)
			ws, _ := time.Parse(time.RFC3339, rec.WindowStart)
			we, _ := time.Parse(time.RFC3339, rec.WindowEnd)
			return shard.NewColdShard(rec.ID, apps, rec.ParquetPath, ws, we)
		}
		return nil, fmt.Errorf("skip live shard %s", rec.ID)
	})
	if err != nil {
		t.Fatal(err)
	}

	// The archived shard should be restored.
	if reg2.Count() != 1 {
		t.Fatalf("expected 1 restored shard, got %d", reg2.Count())
	}
	entry := reg2.Get("shard-archived")
	if entry == nil {
		t.Fatal("shard-archived not found after restore")
	}
	if entry.State != StateArchived {
		t.Fatalf("expected ARCHIVED, got %s", entry.State)
	}

	// Query should work on the restored ColdShard.
	rows, err := entry.Shard.Query("SELECT * FROM logs WHERE customer = 'kevlar'")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row from restored shard, got %d", len(rows))
	}
}

func TestRegistry_MatchingShards_IncludesArchived(t *testing.T) {
	ds, err := shard.NewDuckDBShard("shard-arch", "", t0, t1)
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

	matching := reg.MatchingShards(TimeRange{Start: t0, End: t1}, "vimgym")
	if len(matching) != 1 {
		t.Fatalf("expected 1 archived shard in results, got %d", len(matching))
	}
}
