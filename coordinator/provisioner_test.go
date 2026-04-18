package coordinator

import (
	"context"
	"testing"
	"time"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
)

func TestProvisioner_RotatesOnRowCount(t *testing.T) {
	// Set up a shard with some data.
	ds, err := shard.NewDuckDBShard("shard-test", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID:          "shard-test",
		Apps:        []string{"vimgym"},
		// Future WindowEnd so the wall-clock-window rotation in
		// check() doesn't preempt the row-count / MaxDuration
		// trigger this test is exercising.
		WindowStart: time.Now().UTC(),
		WindowEnd:   time.Now().UTC().Add(100 * 365 * 24 * time.Hour),
		State:       StateIngesting,
		Shard:       ds,
		RowCount:    100, // will be set above threshold
		CreatedAt:   time.Now(),
	})

	launcher := NewLocalLauncher()
	t.Cleanup(func() { launcher.Close() })

	config := ProvisionerConfig{
		MaxRows:      50, // low threshold for test
		MaxDuration:  1 * time.Hour,
		PollInterval: 100 * time.Millisecond,
	}

	prov := NewProvisioner(reg, launcher, config)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Manually trigger check.
	prov.check(ctx)

	// The old shard should be READ_ONLY and a new one should exist.
	entries := reg.Entries()
	if len(entries) != 2 {
		t.Fatalf("expected 2 shards after rotation, got %d", len(entries))
	}

	oldFound, newFound := false, false
	for _, e := range entries {
		if e.ID == "shard-test" && e.State == StateReadOnly {
			oldFound = true
		}
		if e.ID != "shard-test" && e.State == StateIngesting {
			newFound = true
		}
	}
	if !oldFound {
		t.Error("old shard not transitioned to READ_ONLY")
	}
	if !newFound {
		t.Error("new INGESTING shard not created")
	}
}

func TestProvisioner_NoRotateBelowThreshold(t *testing.T) {
	ds, err := shard.NewDuckDBShard("shard-ok", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID:          "shard-ok",
		Apps:        []string{"vimgym"},
		// Future WindowEnd so the wall-clock-window rotation in
		// check() doesn't preempt the row-count / MaxDuration
		// trigger this test is exercising.
		WindowStart: time.Now().UTC(),
		WindowEnd:   time.Now().UTC().Add(100 * 365 * 24 * time.Hour),
		State:       StateIngesting,
		Shard:       ds,
		RowCount:    10, // below threshold
		CreatedAt:   time.Now(),
	})

	launcher := NewLocalLauncher()
	t.Cleanup(func() { launcher.Close() })

	config := ProvisionerConfig{
		MaxRows:     50,
		MaxDuration: 1 * time.Hour,
	}

	prov := NewProvisioner(reg, launcher, config)
	prov.check(context.Background())

	if reg.Count() != 1 {
		t.Fatalf("expected 1 shard (no rotation), got %d", reg.Count())
	}
}

func TestProvisioner_RotatesOnAge(t *testing.T) {
	ds, err := shard.NewDuckDBShard("shard-old", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID:          "shard-old",
		Apps:        []string{"callbox"},
		// Future WindowEnd so the wall-clock-window rotation in
		// check() doesn't preempt the row-count / MaxDuration
		// trigger this test is exercising.
		WindowStart: time.Now().UTC(),
		WindowEnd:   time.Now().UTC().Add(100 * 365 * 24 * time.Hour),
		State:       StateIngesting,
		Shard:       ds,
		RowCount:    0,
		CreatedAt:   time.Now().Add(-2 * time.Hour), // older than MaxDuration
	})

	launcher := NewLocalLauncher()
	t.Cleanup(func() { launcher.Close() })

	config := ProvisionerConfig{
		MaxRows:     5_000_000,
		MaxDuration: 1 * time.Hour, // shard is 2h old
	}

	prov := NewProvisioner(reg, launcher, config)
	prov.check(context.Background())

	if reg.Count() != 2 {
		t.Fatalf("expected 2 shards after age rotation, got %d", reg.Count())
	}
}

func TestProvisioner_NewShardAcceptsIngestion(t *testing.T) {
	// Verify the new shard created by rotation can actually accept data.
	ds, err := shard.NewDuckDBShard("shard-full", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID:          "shard-full",
		Apps:        []string{"vimgym"},
		// Future WindowEnd so the wall-clock-window rotation in
		// check() doesn't preempt the row-count / MaxDuration
		// trigger this test is exercising.
		WindowStart: time.Now().UTC(),
		WindowEnd:   time.Now().UTC().Add(100 * 365 * 24 * time.Hour),
		State:       StateIngesting,
		Shard:       ds,
		RowCount:    100,
		CreatedAt:   time.Now(),
	})

	launcher := NewLocalLauncher()
	t.Cleanup(func() { launcher.Close() })

	config := ProvisionerConfig{
		MaxRows:     50,
		MaxDuration: 1 * time.Hour,
	}

	prov := NewProvisioner(reg, launcher, config)
	prov.check(context.Background())

	// Find the new ingesting shard and ingest some data.
	newShard := reg.IngestingShard("vimgym")
	if newShard == nil {
		t.Fatal("no ingesting shard for vimgym after rotation")
	}

	rows := []*pb.LogRow{{
		LineNum:  1,
		Raw:      "test row",
		App:      "vimgym",
		Customer: "kevlar",
		TsUnixNs: time.Now().UnixNano(),
	}}
	if err := newShard.Ingest(rows); err != nil {
		t.Fatalf("ingest into new shard: %v", err)
	}

	// Query should return the row.
	result, err := newShard.Query("SELECT line_num, raw FROM logs WHERE app = 'vimgym'")
	if err != nil {
		t.Fatalf("query new shard: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 row, got %d", len(result))
	}
}
