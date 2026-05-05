package coordinator

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/store"
)

// fakeBucketStore is an in-memory BucketStore for unit tests that
// want to exercise BucketManager's DB-talking logic (EvaluateSplit,
// Split, AddAppToBucket) without standing up postgres. Assigns
// surrogate ids sequentially on first insert.
type fakeBucketStore struct {
	buckets []store.Bucket
	nextID  int64
}

func newFakeBucketStore() *fakeBucketStore {
	return &fakeBucketStore{nextID: 1}
}

// seedBucket adds a bucket directly, assigning a surrogate id.
// Used by tests to set up initial state before handing the fake
// to a BucketManager.
func (f *fakeBucketStore) seedBucket(label string, apps []string) *store.Bucket {
	appsJSON, _ := json.Marshal(apps)
	b := store.Bucket{
		ID:                   f.nextID,
		Label:                label,
		Apps:                 string(appsJSON),
		TargetIngestingCount: 2,
		Tier:                 "small",
		WindowDuration:       "168h",
		CreatedAt:            "2026-04-21T00:00:00Z",
	}
	f.nextID++
	f.buckets = append(f.buckets, b)
	return &f.buckets[len(f.buckets)-1]
}

func (f *fakeBucketStore) ListBuckets() ([]store.Bucket, error) {
	out := make([]store.Bucket, len(f.buckets))
	copy(out, f.buckets)
	return out, nil
}

func (f *fakeBucketStore) GetBucket(id int64) (store.Bucket, bool, error) {
	for _, b := range f.buckets {
		if b.ID == id {
			return b, true, nil
		}
	}
	return store.Bucket{}, false, nil
}

func (f *fakeBucketStore) GetBucketByLabel(label string) (store.Bucket, bool, error) {
	for _, b := range f.buckets {
		if b.Label == label {
			return b, true, nil
		}
	}
	return store.Bucket{}, false, nil
}

func (f *fakeBucketStore) SaveBucket(b *store.Bucket) error {
	if b.ID == 0 {
		b.ID = f.nextID
		f.nextID++
		f.buckets = append(f.buckets, *b)
		return nil
	}
	for i, existing := range f.buckets {
		if existing.ID == b.ID {
			f.buckets[i] = *b
			return nil
		}
	}
	f.buckets = append(f.buckets, *b)
	return nil
}

// stubBuckets is a Buckets implementation for tests that need to
// hand christen()/check() a specific default bucket without
// standing up a fake store. Returns the configured bucket from
// Default()/All(); split/evaluate are inert.
type stubBuckets struct {
	bucket *AppBucket
}

// defaultBucketID is the surrogate id stub tests use for "the
// default bucket." Any positive number works — tests just need it
// to match what they stamp onto ShardEntry.BucketID. Keeping it
// constant across tests makes cross-test reasoning easier.
const defaultBucketID int64 = 1

// newDefaultStubBuckets returns a stubBuckets whose default is a
// catch-all bucket — the shape christen() expects when there are no
// dedicated buckets carved out yet.
func newDefaultStubBuckets() *stubBuckets {
	return &stubBuckets{bucket: &AppBucket{ID: defaultBucketID, Label: "default", Apps: nil, Tier: "small"}}
}

func (s *stubBuckets) Default() (*AppBucket, error) { return s.bucket, nil }
func (s *stubBuckets) ByID(id int64) (*AppBucket, error) {
	if s.bucket != nil && s.bucket.ID == id {
		return s.bucket, nil
	}
	return nil, nil
}
func (s *stubBuckets) ByLabel(label string) (*AppBucket, error) {
	if s.bucket != nil && s.bucket.Label == label {
		return s.bucket, nil
	}
	return nil, nil
}
func (s *stubBuckets) All() ([]*AppBucket, error) {
	if s.bucket == nil {
		return nil, nil
	}
	return []*AppBucket{s.bucket}, nil
}
func (s *stubBuckets) EvaluateSplit(_ map[string]int64) (string, error) { return "", nil }
func (s *stubBuckets) Split(_ string) (*AppBucket, error)               { return nil, nil }
func (s *stubBuckets) SetStore(_ BucketStore)                           {}
func (s *stubBuckets) AddAppToBucket(id int64, app string) (*AppBucket, error) {
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

	stub := &stubBuckets{bucket: &AppBucket{ID: defaultBucketID, Label: "default", Apps: []string{"weft"}, Tier: "small"}}
	prov := &Provisioner{
		registry: reg,
		Buckets:  stub,
	}

	if err := prov.OnboardApp("xylem"); err != nil {
		t.Fatalf("OnboardApp: %v", err)
	}

	// Bucket now has both apps.
	def, err := stub.Default()
	if err != nil {
		t.Fatalf("stub.Default: %v", err)
	}
	got := def.Apps
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
	def2, _ := stub.Default()
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
// See docs/plans/bucket-split-policy.md.
func TestBucketManager_EvaluateSplit_Disabled(t *testing.T) {
	fake := newFakeBucketStore()
	fake.seedBucket("default", []string{"vimgym", "callbox", "grantcentral"})
	bm := NewBucketManager(fake)

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
	fake := newFakeBucketStore()
	fake.seedBucket("default", []string{"vimgym", "callbox", "grantcentral"})
	bm := NewBucketManager(fake)

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
	def, err := bm.ByLabel("default")
	if err != nil || def == nil {
		t.Fatalf("ByLabel(default): %v / %v", def, err)
	}
	if len(def.Apps) != 2 {
		t.Fatalf("expected 2 apps in default after split, got %v", def.Apps)
	}
	for _, a := range def.Apps {
		if a == "vimgym" {
			t.Errorf("vimgym still in default after split")
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
	fake := newFakeBucketStore()
	fake.seedBucket("default", []string{"vimgym"})
	bm := NewBucketManager(fake)

	// Can't split a single-app bucket.
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

	launcher := newLocalLauncher()
	t.Cleanup(func() { launcher.Close() })

	fake := newFakeBucketStore()
	b := fake.seedBucket("default", []string{"vimgym"})
	b.TargetIngestingCount = 3
	_ = fake.SaveBucket(b)

	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID:       "shard-only",
		BucketID: b.ID, // reconciler filters snapshot.Entries by BucketID
		Apps:     []string{"vimgym"},
		// WindowEnd within bucket duration (168h) but in the future
		// so wall-clock rotation doesn't preempt the scale-up test.
		WindowStart: time.Now().UTC(), WindowEnd: time.Now().UTC().Add(24 * time.Hour),
		State: StateIngesting,
		Shard: ds, CreatedAt: time.Now(),
	})

	prov := NewProvisioner(reg, launcher, ProvisionerConfig{})
	prov.Buckets = NewBucketManager(fake)

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
