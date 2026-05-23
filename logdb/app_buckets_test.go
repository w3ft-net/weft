package logdb

import (
	"context"
	"testing"
	"time"

	"github.com/kevlarcon/weft/shard"
)

// defaultBucketID is the surrogate id of the schema-seeded "default"
// bucket (schema.sql sets it to 1). Tests stamp it onto
// ShardEntry.BucketID so the reconciler's per-bucket snapshot walk
// matches those entries to the bucket. seedDefaultBucket updates that
// same row in place (see window_rotation_test.go).
const defaultBucketID int64 = 1

// OnboardApp must append the app to the default bucket AND to every
// INGESTING shard bound to that bucket, while leaving READ_ONLY and
// ARCHIVED shards alone (their apps list stays frozen — they never
// accepted writes for the new app). Shards in a different bucket
// also stay untouched.
func TestOnboardApp_OnlyIngestingInBucketGetsAppended(t *testing.T) {
	db := openSMTestStore(t)
	seedDefaultBucket(t, db, []string{"weft"}, 0, 0)
	bm := NewBucketManager(db)

	reg := NewShardRegistry()

	// Two INGESTING shards in the default bucket.
	reg.Add(&ShardEntry{
		ID:       "ingest-1",
		BucketID: defaultBucketID,
		State:    StateIngesting,
		Apps:     []string{"weft"},
		Shard:    &failingShard{id: "ingest-1"},
	})
	reg.Add(&ShardEntry{
		ID:       "ingest-2",
		BucketID: defaultBucketID,
		State:    StateIngesting,
		Apps:     []string{"weft"},
		Shard:    &failingShard{id: "ingest-2"},
	})
	// A READ_ONLY shard in the same bucket — must NOT get the app.
	reg.Add(&ShardEntry{
		ID:       "readonly-1",
		BucketID: defaultBucketID,
		State:    StateReadOnly,
		Apps:     []string{"weft"},
		Shard:    &failingShard{id: "readonly-1"},
	})
	// An INGESTING shard in a *different* bucket — also must NOT.
	const dedicatedBucketID int64 = 2
	reg.Add(&ShardEntry{
		ID:       "other-bucket-ingest",
		BucketID: dedicatedBucketID,
		State:    StateIngesting,
		Apps:     []string{"vimgym"},
		Shard:    &failingShard{id: "other-bucket-ingest"},
	})

	prov := &Provisioner{
		registry: reg,
		Buckets:  bm,
	}

	if err := prov.OnboardApp("xylem"); err != nil {
		t.Fatalf("OnboardApp: %v", err)
	}

	// Bucket now has both apps.
	def, err := bm.Default()
	if err != nil {
		t.Fatalf("bm.Default: %v", err)
	}
	if got := def.Apps; len(got) != 2 || got[0] != "weft" || got[1] != "xylem" {
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
	def2, _ := bm.Default()
	if got := def2.Apps; len(got) != 2 {
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

// EvaluateSplit is stubbed to "" while the split policy is redesigned.
// See docs/plans/bucket-split-policy.md. The disabled path doesn't
// touch the store, so this stays a DB-free unit test.
func TestBucketManager_EvaluateSplit_Disabled(t *testing.T) {
	bm := NewBucketManager(nil)

	// Even with a clearly dominant app, EvaluateSplit must return ""
	// until the policy is re-enabled. If this test fails, the stub
	// has been removed — update the regression tests in the plan doc.
	stats := map[string]int64{"vimgym": 700, "callbox": 150, "grantcentral": 150}
	app, err := bm.EvaluateSplit(stats)
	if err != nil {
		t.Fatalf("EvaluateSplit: %v", err)
	}
	if app != "" {
		t.Fatalf("EvaluateSplit must be disabled; got %q", app)
	}
}

func TestBucketManager_Split(t *testing.T) {
	db := openSMTestStore(t)
	src := seedDedicatedBucket(t, db, "split-src", []string{"vimgym", "callbox", "grantcentral"})
	bm := NewBucketManager(db)

	bucket, err := bm.Split("vimgym")
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	if bucket == nil {
		t.Fatal("expected new bucket")
	}
	if len(bucket.Apps) != 1 || bucket.Apps[0] != "vimgym" {
		t.Fatalf("expected [vimgym], got %v", bucket.Apps)
	}
	if bucket.Label != "dedicated-vimgym" {
		t.Errorf("expected label dedicated-vimgym, got %q", bucket.Label)
	}

	// Original bucket now has 2 apps.
	updatedSrc, err := bm.ByLabel(src.Label)
	if err != nil || updatedSrc == nil {
		t.Fatalf("ByLabel(%s): %v / %v", src.Label, updatedSrc, err)
	}
	if len(updatedSrc.Apps) != 2 {
		t.Fatalf("expected 2 apps in source after split, got %v", updatedSrc.Apps)
	}
	for _, a := range updatedSrc.Apps {
		if a == "vimgym" {
			t.Errorf("vimgym still in source after split")
		}
	}

	// The dedicated bucket is queryable by its label.
	dedicated, err := bm.ByLabel("dedicated-vimgym")
	if err != nil || dedicated == nil {
		t.Fatalf("ByLabel(dedicated-vimgym): %v / %v", dedicated, err)
	}
	if dedicated.ID != bucket.ID {
		t.Errorf("dedicated bucket id mismatch: %d vs %d", dedicated.ID, bucket.ID)
	}
}

func TestBucketManager_NoDuplicateSplit(t *testing.T) {
	// EvaluateSplit is disabled, so a single-app bucket (or any) yields
	// no split. DB-free: the disabled path never reads the store.
	bm := NewBucketManager(nil)

	app, err := bm.EvaluateSplit(map[string]int64{"vimgym": 1000})
	if err != nil {
		t.Fatalf("EvaluateSplit: %v", err)
	}
	if app != "" {
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

	launcher := NewLocalLauncher()
	t.Cleanup(func() { launcher.Close() })

	db := openSMTestStore(t)
	seedDefaultBucket(t, db, []string{"vimgym"}, 3, 168*time.Hour)

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID:       "shard-only",
		BucketID: defaultBucketID, // reconciler filters snapshot.Entries by BucketID
		Apps:     []string{"vimgym"},
		// WindowEnd within bucket duration (168h) but in the future
		// so wall-clock rotation doesn't preempt the scale-up test.
		WindowStart: time.Now().UTC(), WindowEnd: time.Now().UTC().Add(24 * time.Hour),
		State: StateIngesting,
		Shard: ds, CreatedAt: time.Now(),
	})

	prov := NewProvisioner(reg, launcher, ProvisionerConfig{})
	prov.Buckets = NewBucketManager(db)

	prov.check(context.Background())

	// Should have scaled up to 3 ingesting shards.
	count := reg.IngestingCount("vimgym")
	if count != 3 {
		t.Fatalf("expected 3 ingesting shards, got %d", count)
	}
}

// End-to-end split test removed along with the disabled EvaluateSplit
// path. See docs/plans/bucket-split-policy.md for the regression
// contract the re-enabled policy will need to honour.
