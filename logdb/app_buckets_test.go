package logdb

import (
	"testing"

	"github.com/graybeardcorp/weft/shard"
	"github.com/graybeardcorp/weft/store"
)

// defaultBucketID is the surrogate id of the schema-seeded "default"
// bucket (schema.sql sets it to 1). Tests stamp it onto
// Shard.BucketID so the reconciler's per-bucket snapshot walk
// matches those entries to the bucket. seedDefaultBucket updates that
// same row in place (see window_rotation_test.go).
const defaultBucketID int64 = 1

// OnboardService stamps the service onto every INGESTING shard bound to
// the default bucket (via shard_services), while leaving READ_ONLY and
// ARCHIVED shards alone (their stamp stays frozen — they never accepted
// writes for the new service) and shards in a different bucket untouched.
// Buckets don't serve services; shards do — the bucket just scopes which
// shards get stamped.
func TestOnboardService_OnlyIngestingInBucketGetsAppended(t *testing.T) {
	db := openSMTestStore(t)
	seedDefaultBucket(t, db, 0, 0)
	// Both services live in the default bucket's log_groups (the routing
	// source of truth); the reconciler stamps the missing one onto the
	// INGESTING shards. nsID is threaded into every entry below.
	nsID := seedLogGroups(t, db, "onboard-cust", "onboard-ns", "weft", "xylem")
	bm := NewBucketManager(db)

	reg := NewShardRegistry()

	// Two INGESTING shards in the default bucket.
	reg.Add(&Shard{
		ID:       "ingest-1",
		BucketID: defaultBucketID,
		State:    StateIngesting,
		Services: []store.ShardService{{NamespaceID: nsID, Service: "weft"}},
		Live:     &failingShard{id: "ingest-1"},
	})
	reg.Add(&Shard{
		ID:       "ingest-2",
		BucketID: defaultBucketID,
		State:    StateIngesting,
		Services: []store.ShardService{{NamespaceID: nsID, Service: "weft"}},
		Live:     &failingShard{id: "ingest-2"},
	})
	// A READ_ONLY shard in the same bucket — must NOT get the app.
	reg.Add(&Shard{
		ID:       "readonly-1",
		BucketID: defaultBucketID,
		State:    StateReadOnly,
		Services: []store.ShardService{{NamespaceID: nsID, Service: "weft"}},
		Live:     &failingShard{id: "readonly-1"},
	})
	// An INGESTING shard in a *different* bucket — also must NOT.
	const dedicatedBucketID int64 = 2
	reg.Add(&Shard{
		ID:       "other-bucket-ingest",
		BucketID: dedicatedBucketID,
		State:    StateIngesting,
		Services: []store.ShardService{{NamespaceID: nsID, Service: "vimgym"}},
		Live:     &failingShard{id: "other-bucket-ingest"},
	})

	prov := &Provisioner{
		registry: reg,
		Buckets:  bm,
	}
	prov.config.ShardOps = ShardStore{db} // ServicesForBucket for the reconciler's drift diff

	if err := prov.OnboardService(nsID, "xylem"); err != nil {
		t.Fatalf("OnboardService: %v", err)
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

	// Second call is a no-op (idempotent): the shards aren't double-stamped.
	if err := prov.OnboardService(nsID, "xylem"); err != nil {
		t.Fatalf("OnboardService (second): %v", err)
	}
	if got := reg.Get("ingest-1").Services; len(got) != 2 {
		t.Errorf("second OnboardService double-stamped ingest-1: %v", got)
	}
}

func containsService(apps []string, want string) bool {
	for _, a := range apps {
		if a == want {
			return true
		}
	}
	return false
}

// servicesContain reports whether any of svcs has Service == want,
// the Shard.Services counterpart to containsService.
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
	cust, err := db.FindOrCreateOrg(customer)
	if err != nil {
		t.Fatalf("customer: %v", err)
	}
	ns, err := db.FindOrCreateNamespace(cust.ID, namespace)
	if err != nil {
		t.Fatalf("namespace: %v", err)
	}
	for _, svc := range services {
		if err := db.AddLogGroup(ns.ID, store.SourceTypeWeftd, svc, svc, "local", ""); err != nil {
			t.Fatalf("log group %s: %v", svc, err)
		}
	}
	return ns.ID
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
	reg.Add(&Shard{
		ID: "s0", Services: []store.ShardService{{NamespaceID: 1, Service: "vimgym"}},
		WindowStart: t0, WindowEnd: t2, State: StateIngesting, Live: s0,
	})
	reg.Add(&Shard{
		ID: "s1", Services: []store.ShardService{{NamespaceID: 1, Service: "vimgym"}},
		WindowStart: t0, WindowEnd: t2, State: StateIngesting, Live: s1,
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

// End-to-end split test removed along with the disabled EvaluateSplit
// path. See docs/plans/bucket-split-policy.md for the regression
// contract the re-enabled policy will need to honour.
