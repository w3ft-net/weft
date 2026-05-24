package logdb

import (
	"context"
	"testing"
	"time"

	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/store"
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
	// Both services live in the default bucket's log_groups (the routing
	// source of truth); the reconciler stamps the missing one onto the
	// INGESTING shards. nsID is threaded into every entry below.
	nsID := seedLogGroups(t, db, "onboard-cust", "onboard-ns", "weft", "xylem")
	bm := NewBucketManager(db)

	reg := NewShardRegistry()

	// Two INGESTING shards in the default bucket.
	reg.Add(&ShardEntry{
		ID:       "ingest-1",
		BucketID: defaultBucketID,
		State:    StateIngesting,
		Services: []store.ShardService{{NamespaceID: nsID, Service: "weft"}},
		Shard:    &failingShard{id: "ingest-1"},
	})
	reg.Add(&ShardEntry{
		ID:       "ingest-2",
		BucketID: defaultBucketID,
		State:    StateIngesting,
		Services: []store.ShardService{{NamespaceID: nsID, Service: "weft"}},
		Shard:    &failingShard{id: "ingest-2"},
	})
	// A READ_ONLY shard in the same bucket — must NOT get the app.
	reg.Add(&ShardEntry{
		ID:       "readonly-1",
		BucketID: defaultBucketID,
		State:    StateReadOnly,
		Services: []store.ShardService{{NamespaceID: nsID, Service: "weft"}},
		Shard:    &failingShard{id: "readonly-1"},
	})
	// An INGESTING shard in a *different* bucket — also must NOT.
	const dedicatedBucketID int64 = 2
	reg.Add(&ShardEntry{
		ID:       "other-bucket-ingest",
		BucketID: dedicatedBucketID,
		State:    StateIngesting,
		Services: []store.ShardService{{NamespaceID: nsID, Service: "vimgym"}},
		Shard:    &failingShard{id: "other-bucket-ingest"},
	})

	prov := &Provisioner{
		registry: reg,
		Buckets:  bm,
	}
	prov.config.ShardOps = db // ServicesForBucket for the reconciler's drift diff

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
		if len(e.Services) != 2 || !servicesContain(e.Services, "xylem") {
			t.Errorf("%s apps: got %v, want contains xylem", id, e.Services)
		}
	}

	// READ_ONLY shard: untouched.
	if got := reg.Get("readonly-1").Services; len(got) != 1 || got[0].Service != "weft" {
		t.Errorf("readonly-1 apps changed: got %v, want [weft] (READ_ONLY must stay frozen)", got)
	}

	// Other-bucket shard: untouched.
	if got := reg.Get("other-bucket-ingest").Services; len(got) != 1 || got[0].Service != "vimgym" {
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

// servicesContain reports whether any of svcs has Service == want,
// the ShardEntry.Services counterpart to containsApp.
func servicesContain(svcs []store.ShardService, want string) bool {
	for _, s := range svcs {
		if s.Service == want {
			return true
		}
	}
	return false
}

// seedLogGroups creates a customer + namespace and a default-bucket
// log_groups row per service, returning the namespace id. Routing now
// reads a bucket's services from log_groups (Store.ServicesForBucket),
// so christen/reconciler-propagation tests seed the service here — the
// stand-in for a real AddSource/SubmitSample — not just on the bucket.
// bucket_id is left NULL, which ServicesForBucket resolves to the
// default bucket. The returned id must be threaded into the entries'
// Services and the IngestingShard/IngestingCount calls so the in-memory
// membership and the DB rows agree.
func seedLogGroups(t *testing.T, db *store.Store, customer, namespace string, services ...string) uint {
	t.Helper()
	cust, err := db.FindOrCreateCustomer(customer)
	if err != nil {
		t.Fatalf("customer: %v", err)
	}
	ns, err := db.FindOrCreateNamespace(cust.ID, namespace)
	if err != nil {
		t.Fatalf("namespace: %v", err)
	}
	for _, svc := range services {
		if err := db.AddLogGroup(ns.ID, nil, store.SourceTypeWeftd, svc, svc, "local", "", "0s"); err != nil {
			t.Fatalf("log group %s: %v", svc, err)
		}
	}
	return ns.ID
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
		ID: "s0", Services: []store.ShardService{{NamespaceID: 1, Service: "vimgym"}},
		WindowStart: t0, WindowEnd: t2, State: StateIngesting, Shard: s0,
	})
	reg.Add(&ShardEntry{
		ID: "s1", Services: []store.ShardService{{NamespaceID: 1, Service: "vimgym"}},
		WindowStart: t0, WindowEnd: t2, State: StateIngesting, Shard: s1,
	})

	// Round-robin should alternate between s0 and s1.
	seen := make(map[string]int)
	for i := 0; i < 100; i++ {
		sh := reg.IngestingShard(1, "vimgym")
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
	// Routing reads the bucket's services from log_groups, so the
	// scale-up shards are stamped from here (not bucket.Apps).
	nsID := seedLogGroups(t, db, "scaleup-cust", "vimgym-ns", "vimgym")

	reg := NewShardRegistry()
	// Store-backed registry like the production sync launcher: Add
	// persists a shard_records row so each launched shard's
	// StampShardServices FK holds, and RefreshFromStore reloads service
	// membership from shard_services.
	reg.SetStore(db)
	reg.Add(&ShardEntry{
		ID:       "shard-only",
		BucketID: defaultBucketID, // reconciler filters snapshot.Entries by BucketID
		Services: []store.ShardService{{NamespaceID: nsID, Service: "vimgym"}},
		// WindowEnd within bucket duration (168h) but in the future
		// so wall-clock rotation doesn't preempt the scale-up test.
		WindowStart: time.Now().UTC(), WindowEnd: time.Now().UTC().Add(24 * time.Hour),
		State: StateIngesting,
		Shard: ds, CreatedAt: time.Now(),
	})
	// Stamp the pre-existing shard's service so check()'s RefreshFromStore
	// reloads it from shard_services rather than wiping it to nil.
	if err := db.StampShardServices("shard-only", defaultBucketID); err != nil {
		t.Fatalf("stamp shard-only: %v", err)
	}

	prov := NewProvisioner(reg, launcher, ProvisionerConfig{})
	prov.Buckets = NewBucketManager(db)
	prov.config.ShardOps = db // ServicesForBucket / StampShardServices for the launched shards

	prov.check(context.Background())

	// Should have scaled up to 3 ingesting shards, each stamped with
	// the bucket's vimgym service from log_groups.
	count := reg.IngestingCount(nsID, "vimgym")
	if count != 3 {
		t.Fatalf("expected 3 ingesting shards, got %d", count)
	}
}

// End-to-end split test removed along with the disabled EvaluateSplit
// path. See docs/plans/bucket-split-policy.md for the regression
// contract the re-enabled policy will need to honour.
