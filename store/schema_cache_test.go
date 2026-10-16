package store

import (
	"context"
	"os"
	"testing"
)

// testSchemaCache connects to a local Valkey/Redis (the docker-compose
// dev instance, or WEFT_TEST_REDIS_ADDR override) and skips the test if
// unreachable — same "skip, don't fail, when the dependency isn't up"
// posture as testOpen's Postgres skip.
func testSchemaCache(t *testing.T) *SchemaCache {
	t.Helper()
	addr := os.Getenv("WEFT_TEST_REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6380" // docker-compose.yml's redis: service
	}
	c := NewSchemaCache(addr, false)
	if _, err := c.client.Ping(t.Context()).Result(); err != nil {
		t.Skipf("valkey/redis unavailable at %s: %v", addr, err)
	}
	return c
}

// TestFindLogGroupSchema_CacheAsideAndInvalidation covers the whole
// point of the shared cache: a configured service's schema comes back
// on both a cold (Postgres) and warm (Valkey) lookup, and
// ConfigureLogGroup's write-time invalidation makes the very next
// lookup after a schema edit see the new schema rather than a stale
// cached one.
func TestFindLogGroupSchema_CacheAsideAndInvalidation(t *testing.T) {
	db := testOpen(t)
	cache := testSchemaCache(t)
	db.SetSchemaCache(cache)
	t.Cleanup(func() { db.SetSchemaCache(nil) })

	cust, err := db.FindOrCreateOrg("schema-cache-test")
	if err != nil {
		t.Fatal(err)
	}
	ns, err := db.FindOrCreateNamespace(cust.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	const service = "cacheme"

	if err := db.AddLogGroup(context.Background(), ns.ID, SourceTypeDirect, service, "", "local", "app: cacheme\nformat: text\n"); err != nil {
		t.Fatalf("AddLogGroup: %v", err)
	}

	// Cold lookup: cache miss, falls through to Postgres, then warms
	// the cache as a side effect.
	yaml1, ok, err := db.FindLogGroupSchema(context.Background(), cust.ID, "test", service)
	if err != nil || !ok {
		t.Fatalf("cold FindLogGroupSchema: ok=%v err=%v", ok, err)
	}
	if yaml1 != "app: cacheme\nformat: text\n" {
		t.Fatalf("cold schema_yaml = %q", yaml1)
	}

	// Warm lookup: same answer, this time from Valkey. We can't observe
	// "from Valkey" directly through the public method, so prove it the
	// other way — mutate Postgres directly (bypassing invalidation) and
	// confirm the cached (now stale) answer still comes back, which is
	// only possible if this read hit Valkey rather than Postgres.
	if err := db.DB().Model(&LogGroup{}).
		Where("namespace_id = ? AND app = ?", ns.ID, service).
		Update("schema_yaml", "app: cacheme\nformat: json\n").Error; err != nil {
		t.Fatalf("direct postgres mutation: %v", err)
	}
	yaml2, ok, err := db.FindLogGroupSchema(context.Background(), cust.ID, "test", service)
	if err != nil || !ok {
		t.Fatalf("warm FindLogGroupSchema: ok=%v err=%v", ok, err)
	}
	if yaml2 != yaml1 {
		t.Fatalf("warm schema_yaml = %q, want stale cached %q (this read should have hit Valkey, not Postgres)", yaml2, yaml1)
	}

	// ConfigureLogGroup invalidates — the next lookup must see the new
	// schema immediately, not wait out the cache's TTL.
	if err := db.ConfigureLogGroup(context.Background(), ns.ID, service, "app: cacheme\nformat: prefix_json\n"); err != nil {
		t.Fatalf("ConfigureLogGroup: %v", err)
	}
	yaml3, ok, err := db.FindLogGroupSchema(context.Background(), cust.ID, "test", service)
	if err != nil || !ok {
		t.Fatalf("post-invalidation FindLogGroupSchema: ok=%v err=%v", ok, err)
	}
	if yaml3 != "app: cacheme\nformat: prefix_json\n" {
		t.Fatalf("post-invalidation schema_yaml = %q, want the freshly configured schema", yaml3)
	}
}

// TestFindLogGroupSchema_NegativeCache: a service with no log_groups
// row at all comes back ok=false, cheaply, from both a cold and warm
// lookup — never an error, never blocking ingestion.
func TestFindLogGroupSchema_NegativeCache(t *testing.T) {
	db := testOpen(t)
	cache := testSchemaCache(t)
	db.SetSchemaCache(cache)
	t.Cleanup(func() { db.SetSchemaCache(nil) })

	cust, err := db.FindOrCreateOrg("schema-cache-negative-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.FindOrCreateNamespace(cust.ID, "test"); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ { // cold, then warm
		_, ok, err := db.FindLogGroupSchema(context.Background(), cust.ID, "test", "never-onboarded")
		if err != nil {
			t.Fatalf("iteration %d: unexpected error: %v", i, err)
		}
		if ok {
			t.Fatalf("iteration %d: ok=true for a service with no log_groups row", i)
		}
	}
}

// TestFindLogGroupSchema_NilCacheFallsThroughToPostgres: a *Store with
// no SchemaCache attached (the common case — tests, weft-admin,
// weft-migrate) must behave exactly as it did before this cache
// existed, not error or panic.
func TestFindLogGroupSchema_NilCacheFallsThroughToPostgres(t *testing.T) {
	db := testOpen(t) // no SetSchemaCache call

	cust, err := db.FindOrCreateOrg("schema-cache-nil-test")
	if err != nil {
		t.Fatal(err)
	}
	ns, err := db.FindOrCreateNamespace(cust.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddLogGroup(context.Background(), ns.ID, SourceTypeDirect, "nocache", "", "local", "app: nocache\nformat: text\n"); err != nil {
		t.Fatalf("AddLogGroup: %v", err)
	}

	yaml, ok, err := db.FindLogGroupSchema(context.Background(), cust.ID, "test", "nocache")
	if err != nil || !ok || yaml != "app: nocache\nformat: text\n" {
		t.Fatalf("FindLogGroupSchema with nil cache: yaml=%q ok=%v err=%v", yaml, ok, err)
	}
}
