package store

import (
	"strings"
	"testing"
	"time"
)

// backfillUpSQL extracts the +goose Up body of migration 059 from the
// embedded migrations FS, so the test guards the real migration SQL
// rather than a hand-copied duplicate.
func backfillUpSQL(t *testing.T) string {
	t.Helper()
	raw, err := migrationsFS.ReadFile("migrations/059_backfill_shard_services.sql")
	if err != nil {
		t.Fatalf("read migration 059: %v", err)
	}
	s := string(raw)
	up := s[strings.Index(s, "+goose Up"):]
	up = up[:strings.Index(up, "-- +goose Down")]
	// Drop the marker line itself; keep the SQL body (comment lines are
	// harmless to postgres).
	return up[strings.IndexByte(up, '\n')+1:]
}

// TestBackfillShardServices059 covers migration 059: the bulk re-stamp
// that fans StampShardServices's per-shard join out over every shard via
// shard_records ⋈ log_groups on COALESCE(bucket_id, default) = sr.bucket_id.
// Seeds two buckets' worth of shards and services WITHOUT christen-time
// stamping, runs the real migration SQL, and asserts each shard lands its
// own bucket's services.
func TestBackfillShardServices059(t *testing.T) {
	db := testOpen(t)

	cust, err := db.FindOrCreateCustomer("backfill-test")
	if err != nil {
		t.Fatalf("customer: %v", err)
	}
	ns, err := db.FindOrCreateNamespace(cust.ID, "ns1")
	if err != nil {
		t.Fatalf("namespace: %v", err)
	}
	def, ok, err := db.GetBucketByLabel("default")
	if err != nil || !ok {
		t.Fatalf("default bucket: ok=%v err=%v", ok, err)
	}
	dedicated := &Bucket{
		Label: "dedicated-backfill", Services: []string{"worker"}, TargetIngestingCount: 1,
		Tier: "small", WindowDuration: "168h", CreatedAt: "2026-04-21T00:00:00Z",
	}
	if err := db.SaveBucket(dedicated); err != nil {
		t.Fatalf("dedicated bucket: %v", err)
	}

	// api + ingest stay in the default bucket (NULL bucket_id); worker
	// moves to the dedicated bucket.
	for _, app := range []string{"api", "ingest", "worker"} {
		if err := db.AddLogGroup(ns.ID, nil, SourceTypeWeftd, app, app, "local", "", "0s"); err != nil {
			t.Fatalf("log group %s: %v", app, err)
		}
	}
	if err := db.DB().Model(&LogGroup{}).
		Where("namespace_id = ? AND app = ?", ns.ID, "worker").
		Update("bucket_id", dedicated.ID).Error; err != nil {
		t.Fatalf("move worker: %v", err)
	}

	// Shards exist but are deliberately NOT stamped (simulating shards
	// christened before stamping existed) — two per bucket to prove the
	// join fans out across all shards, not just one.
	mkShard := func(id string, bucketID int64) {
		t.Helper()
		if err := db.SaveShard(&ShardRecord{
			ID: id, State: "READ_ONLY", BucketID: bucketID, Tier: "small",
			WindowStart: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
			WindowEnd:   time.Date(2026, 5, 8, 0, 0, 0, 0, time.UTC),
			CreatedAt:   time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		}); err != nil {
			t.Fatalf("shard %s: %v", id, err)
		}
	}
	mkShard("bf-default-1", def.ID)
	mkShard("bf-default-2", def.ID)
	mkShard("bf-dedicated", dedicated.ID)

	if err := db.DB().Exec(backfillUpSQL(t)).Error; err != nil {
		t.Fatalf("run backfill 059: %v", err)
	}

	servicesOf := func(shardID string) []string {
		t.Helper()
		var rows []ShardService
		if err := db.DB().Where("shard_id = ?", shardID).Order("service").Find(&rows).Error; err != nil {
			t.Fatalf("read shard_services %s: %v", shardID, err)
		}
		out := make([]string, len(rows))
		for i, r := range rows {
			if r.NamespaceID != ns.ID {
				t.Errorf("%s: service %q has namespace_id %d, want %d", shardID, r.Service, r.NamespaceID, ns.ID)
			}
			out[i] = r.Service
		}
		return out
	}

	for _, id := range []string{"bf-default-1", "bf-default-2"} {
		if got := servicesOf(id); len(got) != 2 || got[0] != "api" || got[1] != "ingest" {
			t.Errorf("%s services: got %v, want [api ingest]", id, got)
		}
	}
	if got := servicesOf("bf-dedicated"); len(got) != 1 || got[0] != "worker" {
		t.Errorf("bf-dedicated services: got %v, want [worker]", got)
	}

	// Idempotent (ON CONFLICT DO NOTHING): a shard already stamped at
	// christen isn't duplicated by the bulk backfill.
	if err := db.DB().Exec(backfillUpSQL(t)).Error; err != nil {
		t.Fatalf("re-run backfill 059: %v", err)
	}
	if got := servicesOf("bf-default-1"); len(got) != 2 {
		t.Errorf("re-run duplicated: got %v", got)
	}
}

// TestStampShardServices covers migration 058 / christen stamping: a
// shard is stamped with exactly the services of its bucket, read from
// log_groups (NULL bucket_id = default). A service moved to a dedicated
// bucket lands only on that bucket's shards.
func TestStampShardServices(t *testing.T) {
	db := testOpen(t)

	cust, err := db.FindOrCreateCustomer("shardsvc-test")
	if err != nil {
		t.Fatalf("customer: %v", err)
	}
	ns, err := db.FindOrCreateNamespace(cust.ID, "ns1")
	if err != nil {
		t.Fatalf("namespace: %v", err)
	}
	def, ok, err := db.GetBucketByLabel("default")
	if err != nil || !ok {
		t.Fatalf("default bucket: ok=%v err=%v", ok, err)
	}
	dedicated := &Bucket{
		Label: "dedicated-shardsvc", Services: []string{"worker"}, TargetIngestingCount: 1,
		Tier: "small", WindowDuration: "168h", CreatedAt: "2026-04-21T00:00:00Z",
	}
	if err := db.SaveBucket(dedicated); err != nil {
		t.Fatalf("dedicated bucket: %v", err)
	}

	// Three services: api + ingest in the default bucket (bucket_id
	// NULL), worker moved to the dedicated bucket.
	for _, app := range []string{"api", "ingest", "worker"} {
		if err := db.AddLogGroup(ns.ID, nil, SourceTypeWeftd, app, app, "local", "", "0s"); err != nil {
			t.Fatalf("log group %s: %v", app, err)
		}
	}
	if err := db.DB().Model(&LogGroup{}).
		Where("namespace_id = ? AND app = ?", ns.ID, "worker").
		Update("bucket_id", dedicated.ID).Error; err != nil {
		t.Fatalf("move worker: %v", err)
	}

	// Two shard_records rows (the FK target) — one per bucket.
	mkShard := func(id string, bucketID int64) {
		t.Helper()
		if err := db.SaveShard(&ShardRecord{
			ID: id, State: "INGESTING", BucketID: bucketID, Tier: "small",
			WindowStart: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
			WindowEnd:   time.Date(2026, 5, 8, 0, 0, 0, 0, time.UTC),
			CreatedAt:   time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		}); err != nil {
			t.Fatalf("shard %s: %v", id, err)
		}
	}
	mkShard("shard-default", def.ID)
	mkShard("shard-dedicated", dedicated.ID)

	servicesOf := func(shardID string) []string {
		t.Helper()
		var rows []ShardService
		if err := db.DB().Where("shard_id = ?", shardID).Order("service").Find(&rows).Error; err != nil {
			t.Fatalf("read shard_services %s: %v", shardID, err)
		}
		out := make([]string, len(rows))
		for i, r := range rows {
			if r.NamespaceID != ns.ID {
				t.Errorf("%s: service %q has namespace_id %d, want %d", shardID, r.Service, r.NamespaceID, ns.ID)
			}
			out[i] = r.Service
		}
		return out
	}

	// Default-bucket shard gets api + ingest (the NULL-bucket_id
	// services), not worker.
	if err := db.StampShardServices("shard-default", def.ID); err != nil {
		t.Fatalf("stamp default: %v", err)
	}
	if got := servicesOf("shard-default"); len(got) != 2 || got[0] != "api" || got[1] != "ingest" {
		t.Errorf("shard-default services: got %v, want [api ingest]", got)
	}

	// Dedicated-bucket shard gets only worker.
	if err := db.StampShardServices("shard-dedicated", dedicated.ID); err != nil {
		t.Fatalf("stamp dedicated: %v", err)
	}
	if got := servicesOf("shard-dedicated"); len(got) != 1 || got[0] != "worker" {
		t.Errorf("shard-dedicated services: got %v, want [worker]", got)
	}

	// Idempotent: re-stamping doesn't duplicate.
	if err := db.StampShardServices("shard-default", def.ID); err != nil {
		t.Fatalf("re-stamp: %v", err)
	}
	if got := servicesOf("shard-default"); len(got) != 2 {
		t.Errorf("re-stamp duplicated: got %v", got)
	}
}
