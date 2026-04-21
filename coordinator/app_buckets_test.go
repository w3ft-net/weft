package coordinator

import (
	"context"
	"testing"
	"time"

	"github.com/kevlarcon/weft/shard"
)

// stubBuckets is a Buckets implementation for tests that need to
// hand christen()/check() a specific default bucket without
// building a fake store + LoadFromStore. Returns the configured
// bucket from Default()/All(); split/evaluate are inert.
type stubBuckets struct {
	bucket *AppBucket
}

// newDefaultStubBuckets returns a stubBuckets whose default is a
// catch-all bucket — the shape christen() expects when there are no
// dedicated buckets carved out yet.
func newDefaultStubBuckets() *stubBuckets {
	return &stubBuckets{bucket: &AppBucket{ID: "default", Apps: nil, Tier: "small"}}
}

func (s *stubBuckets) Default() *AppBucket { return s.bucket }
func (s *stubBuckets) All() []*AppBucket {
	if s.bucket == nil {
		return nil
	}
	return []*AppBucket{s.bucket}
}
func (s *stubBuckets) EvaluateSplit(_ map[string]int64) string { return "" }
func (s *stubBuckets) Split(_ string) *AppBucket               { return nil }
func (s *stubBuckets) LoadFromStore(_ BucketStore) error       { return nil }
func (s *stubBuckets) AddAppToBucket(id, app string) (*AppBucket, error) {
	if s.bucket == nil || s.bucket.ID != id {
		return nil, nil
	}
	for _, a := range s.bucket.Apps {
		if a == app {
			return s.bucket, nil
		}
	}
	s.bucket.Apps = append(s.bucket.Apps, app)
	return s.bucket, nil
}

// OnboardApp must append the app to the default bucket AND to every
// INGESTING shard bound to that bucket, while leaving READ_ONLY and
// ARCHIVED shards alone (their apps list stays frozen — they never
// accepted writes for the new app). Shards in a different bucket
// also stay untouched.
func TestOnboardApp_OnlyIngestingInBucketGetsAppended(t *testing.T) {
	reg := NewShardRegistry()

	// Two INGESTING shards in the default bucket.
	reg.Add(&ShardEntry{
		ID:       "ingest-1",
		BucketID: "default",
		State:    StateIngesting,
		Apps:     []string{"weft"},
		Shard:    &failingShard{id: "ingest-1"},
	})
	reg.Add(&ShardEntry{
		ID:       "ingest-2",
		BucketID: "default",
		State:    StateIngesting,
		Apps:     []string{"weft"},
		Shard:    &failingShard{id: "ingest-2"},
	})
	// A READ_ONLY shard in the same bucket — must NOT get the app.
	reg.Add(&ShardEntry{
		ID:       "readonly-1",
		BucketID: "default",
		State:    StateReadOnly,
		Apps:     []string{"weft"},
		Shard:    &failingShard{id: "readonly-1"},
	})
	// An INGESTING shard in a *different* bucket — also must NOT.
	reg.Add(&ShardEntry{
		ID:       "other-bucket-ingest",
		BucketID: "dedicated-vimgym",
		State:    StateIngesting,
		Apps:     []string{"vimgym"},
		Shard:    &failingShard{id: "other-bucket-ingest"},
	})

	stub := &stubBuckets{bucket: &AppBucket{ID: "default", Apps: []string{"weft"}, Tier: "small"}}
	prov := &Provisioner{
		registry: reg,
		Buckets:  stub,
	}

	if err := prov.OnboardApp("xylem"); err != nil {
		t.Fatalf("OnboardApp: %v", err)
	}

	// Bucket now has both apps.
	got := stub.Default().Apps
	if len(got) != 2 || got[0] != "weft" || got[1] != "xylem" {
		t.Errorf("default bucket apps: got %v, want [weft xylem]", got)
	}

	// INGESTING shards in default bucket: updated.
	for _, id := range []string{"ingest-1", "ingest-2"} {
		e := reg.Get(id)
		if e == nil {
			t.Fatalf("%s missing from registry", id)
		}
		if len(e.Apps) != 2 || !containsApp(e.Apps, "xylem") {
			t.Errorf("%s apps: got %v, want contains xylem", id, e.Apps)
		}
	}

	// READ_ONLY shard: untouched.
	if got := reg.Get("readonly-1").Apps; len(got) != 1 || got[0] != "weft" {
		t.Errorf("readonly-1 apps changed: got %v, want [weft] (READ_ONLY must stay frozen)", got)
	}

	// Other-bucket shard: untouched.
	if got := reg.Get("other-bucket-ingest").Apps; len(got) != 1 || got[0] != "vimgym" {
		t.Errorf("other-bucket-ingest apps changed: got %v, want [vimgym]", got)
	}

	// Second call is a no-op (idempotent).
	if err := prov.OnboardApp("xylem"); err != nil {
		t.Fatalf("OnboardApp (second): %v", err)
	}
	if got := stub.Default().Apps; len(got) != 2 {
		t.Errorf("second OnboardApp duplicated: %v", got)
	}
}

func containsApp(apps []string, want string) bool {
	for _, a := range apps {
		if a == want {
			return true
		}
	}
	return false
}

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

	prov := NewProvisioner(reg, launcher, ProvisionerConfig{})
	bm := NewBucketManager([]string{"vimgym"})
	// Bucket target lives on the AppBucket itself now.
	bm.All()[0].Target = 3
	prov.Buckets = bm

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
