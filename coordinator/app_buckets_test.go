package coordinator

import (
	"context"
	"testing"
	"time"

	"github.com/kevlarcon/weft/shard"
)

func TestBucketManager_EvaluateSplit(t *testing.T) {
	bm := NewBucketManager([]string{"vimgym", "callbox", "grantcentral"})

	// No split when traffic is balanced.
	stats := map[string]int64{
		"vimgym":       100,
		"callbox":      100,
		"grantcentral": 100,
	}
	if app := bm.EvaluateSplit(stats); app != "" {
		t.Fatalf("expected no split, got %s", app)
	}

	// Split when vimgym dominates (70% > 60% threshold).
	stats = map[string]int64{
		"vimgym":       700,
		"callbox":      150,
		"grantcentral": 150,
	}
	if app := bm.EvaluateSplit(stats); app != "vimgym" {
		t.Fatalf("expected vimgym split, got %q", app)
	}
}

func TestBucketManager_Split(t *testing.T) {
	bm := NewBucketManager([]string{"vimgym", "callbox", "grantcentral"})

	bucket := bm.Split("vimgym")
	if bucket == nil {
		t.Fatal("expected new bucket")
	}
	if len(bucket.Apps) != 1 || bucket.Apps[0] != "vimgym" {
		t.Fatalf("expected [vimgym], got %v", bucket.Apps)
	}

	// Shared bucket should have 2 remaining apps.
	shared := bm.BucketFor("callbox")
	if shared == nil {
		t.Fatal("expected shared bucket for callbox")
	}
	if len(shared.Apps) != 2 {
		t.Fatalf("expected 2 apps in shared, got %d", len(shared.Apps))
	}

	// vimgym should be in the dedicated bucket.
	vim := bm.BucketFor("vimgym")
	if vim == nil || vim.ID != "dedicated-vimgym" {
		t.Fatalf("expected dedicated-vimgym bucket, got %v", vim)
	}
}

func TestBucketManager_NoDuplicateSplit(t *testing.T) {
	bm := NewBucketManager([]string{"vimgym"})

	// Can't split a single-app bucket.
	if app := bm.EvaluateSplit(map[string]int64{"vimgym": 1000}); app != "" {
		t.Fatalf("expected no split for single-app bucket, got %s", app)
	}
}

func TestRegistry_RoundRobinIngestion(t *testing.T) {
	s0, err := shard.NewDuckDBShard("s0", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s0.Close() })

	s1, err := shard.NewDuckDBShard("s1", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s1.Close() })

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID: "s0", Apps: []string{"vimgym"},
		WindowStart: t0, WindowEnd: t2, State: StateIngesting, Shard: s0,
	})
	reg.Add(&ShardEntry{
		ID: "s1", Apps: []string{"vimgym"},
		WindowStart: t0, WindowEnd: t2, State: StateIngesting, Shard: s1,
	})

	// Round-robin should alternate between s0 and s1.
	seen := make(map[string]int)
	for i := 0; i < 100; i++ {
		sh := reg.IngestingShard("vimgym")
		if sh == nil {
			t.Fatal("nil shard")
		}
		seen[sh.ID()]++
	}

	if len(seen) != 2 {
		t.Fatalf("expected 2 distinct shards, got %d: %v", len(seen), seen)
	}
	// Each should get roughly 50 (allow some variance from map iteration order).
	for id, count := range seen {
		if count < 30 || count > 70 {
			t.Errorf("shard %s got %d/100 writes — expected ~50", id, count)
		}
	}
}

func TestProvisioner_ScalesUpToConfiguredCount(t *testing.T) {
	ds, err := shard.NewDuckDBShard("shard-only", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID: "shard-only", Apps: []string{"vimgym"},
		// Future WindowEnd keeps the wall-clock rotation from
		// preempting the bucket-scale-up this test exercises.
		WindowStart: time.Now().UTC(), WindowEnd: time.Now().UTC().Add(100 * 365 * 24 * time.Hour),
		State: StateIngesting,
		Shard: ds, CreatedAt: time.Now(),
	})

	launcher := NewLocalLauncher()
	t.Cleanup(func() { launcher.Close() })

	config := ProvisionerConfig{
		IngestingPerBucket: 3, // want 3, have 1
	}

	prov := NewProvisioner(reg, launcher, config)
	prov.Buckets = NewBucketManager([]string{"vimgym"})

	prov.check(context.Background())

	// Should have scaled up to 3 ingesting shards.
	count := reg.IngestingCount("vimgym")
	if count != 3 {
		t.Fatalf("expected 3 ingesting shards, got %d", count)
	}
}

func TestProvisioner_SplitCreatesNewShard(t *testing.T) {
	ds, err := shard.NewDuckDBShard("shard-shared", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID:   "shard-shared",
		Apps: []string{"vimgym", "callbox", "grantcentral"},
		// Future WindowEnd — this test exercises bucket splitting,
		// not window-boundary rotation.
		WindowStart: time.Now().UTC(),
		WindowEnd:   time.Now().UTC().Add(100 * 365 * 24 * time.Hour),
		State:       StateIngesting,
		Shard:       ds,
		RowCount:    10,
		CreatedAt:   time.Now(),
	})

	launcher := NewLocalLauncher()
	t.Cleanup(func() { launcher.Close() })

	config := ProvisionerConfig{}

	prov := NewProvisioner(reg, launcher, config)
	prov.Buckets = NewBucketManager([]string{"vimgym", "callbox", "grantcentral"})

	// Simulate vimgym dominating throughput.
	prov.Stats().Record("vimgym", 700)
	prov.Stats().Record("callbox", 150)
	prov.Stats().Record("grantcentral", 150)

	prov.check(context.Background())

	// Should have 2 shards now: shared + dedicated vimgym.
	if reg.Count() != 2 {
		t.Fatalf("expected 2 shards after split, got %d", reg.Count())
	}

	// vimgym should have a dedicated shard.
	vimShard := reg.IngestingShard("vimgym")
	if vimShard == nil {
		t.Fatal("no ingesting shard for vimgym after split")
	}

	// callbox should still be on the shared shard.
	cbShard := reg.IngestingShard("callbox")
	if cbShard == nil {
		t.Fatal("no ingesting shard for callbox after split")
	}
}
