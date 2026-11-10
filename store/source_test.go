package store

import (
	"context"
	"testing"
)

// TestRemoveAndReAddLogGroup reproduces a prod-observed bug: user
// clicks "remove" on a configured source, then later re-configures the
// same source and gets back:
//
//	store error: ERROR: duplicate key value violates unique
//	constraint "idx_ns_app" (SQLSTATE 23505)
//
// Root cause: LogGroup embeds gorm.Model, which adds a DeletedAt
// column. GORM's default Delete is a soft-delete — it sets
// deleted_at = NOW() rather than removing the row. The unique index
// idx_ns_app on (namespace_id, app) is a plain unique index, so the
// still-present row blocks the re-insert.
//
// The user's mental model of "remove" is hard-delete. We either need
// to hard-delete here or make the unique index partial on
// (deleted_at IS NULL) — fixing in code (Unscoped().Delete) is the
// smaller hammer.
func TestRemoveAndReAddLogGroup(t *testing.T) {
	db := testOpen(t)

	cust, err := db.FindOrCreateOrg("remove-readd-test")
	if err != nil {
		t.Fatal(err)
	}
	ns, err := db.FindOrCreateNamespace(cust.ID, "apinamespace")
	if err != nil {
		t.Fatal(err)
	}

	const app = "apinamespace/api"

	// First configure
	if err := db.AddLogGroup(context.Background(), ns.ID, SourceTypeWeftd, app, app, "local", "schema-v1"); err != nil {
		t.Fatalf("first AddLogGroup: %v", err)
	}

	// Remove (user clicks "Remove")
	if err := db.DB().Unscoped().Where("namespace_id = ? AND app = ?", ns.ID, app).Delete(&LogGroup{}).Error; err != nil {
		t.Fatalf("remove log group: %v", err)
	}

	// Re-configure (user picks the source up again from pending)
	if err := db.AddLogGroup(context.Background(), ns.ID, SourceTypeWeftd, app, app, "local", "schema-v2"); err != nil {
		t.Fatalf("second AddLogGroup (after remove) should succeed; got: %v", err)
	}
}

// TestCreatePendingKinesisSource_PendingThenConfigure covers the kinesis
// (Firehose push) onboarding path end to end: `weft source add --type
// kinesis` creates a bare, schema-less pending row, and the existing
// (source-type-agnostic) ConfigureLogGroup finishes it — no kinesis-
// specific configure logic needed.
func TestCreatePendingKinesisSource_PendingThenConfigure(t *testing.T) {
	db := testOpen(t)

	cust, err := db.FindOrCreateOrg("kinesis-pending-test")
	if err != nil {
		t.Fatal(err)
	}
	ns, err := db.FindOrCreateNamespace(cust.ID, "ops")
	if err != nil {
		t.Fatal(err)
	}

	if err := db.CreatePendingKinesisSource(ns.ID, "cold-shard", "/aws/lambda/weft-cold-shard-query", "us-east-2"); err != nil {
		t.Fatalf("CreatePendingKinesisSource: %v", err)
	}

	lg, err := db.FindLogGroup(cust.ID, "ops", "cold-shard")
	if err != nil {
		t.Fatalf("FindLogGroup: %v", err)
	}
	if lg == nil {
		t.Fatal("expected a pending log group row, got nil")
	}
	if lg.SourceType != SourceTypeKinesis {
		t.Errorf("SourceType = %q, want %q", lg.SourceType, SourceTypeKinesis)
	}
	if lg.Status != StatusPending {
		t.Errorf("Status = %q, want %q", lg.Status, StatusPending)
	}
	if lg.LogGroupName != "/aws/lambda/weft-cold-shard-query" {
		t.Errorf("LogGroupName = %q, want the AWS log group name", lg.LogGroupName)
	}
	if lg.SchemaYAML != "" {
		t.Errorf("SchemaYAML = %q, want empty — no schema at creation time", lg.SchemaYAML)
	}

	// Idempotent: a second call (re-running `weft source add`) must not
	// error or clobber the existing row.
	if err := db.CreatePendingKinesisSource(ns.ID, "cold-shard", "/aws/lambda/weft-cold-shard-query", "us-east-2"); err != nil {
		t.Fatalf("second CreatePendingKinesisSource should be a no-op, got: %v", err)
	}

	// ConfigureLogGroup is unmodified and source-type-agnostic — it must
	// finish a kinesis pending row exactly like a weftd one.
	if err := db.ConfigureLogGroup(context.Background(), ns.ID, "cold-shard", "app: cold-shard\nformat: text\n"); err != nil {
		t.Fatalf("ConfigureLogGroup: %v", err)
	}
	lg, err = db.FindLogGroup(cust.ID, "ops", "cold-shard")
	if err != nil {
		t.Fatal(err)
	}
	if lg.Status != StatusConfigured {
		t.Errorf("Status after configure = %q, want %q", lg.Status, StatusConfigured)
	}
	if lg.SourceType != SourceTypeKinesis {
		t.Errorf("SourceType after configure = %q, want it to stay %q", lg.SourceType, SourceTypeKinesis)
	}
}

// TestEnsureServiceLogGroup_StartsSampling: raw producers (Heroku,
// Render) have no agent to sample for them, so the auto-onboarded row
// must come up already asking for a sample (the resample flag) while
// staying configured — rows keep flowing raw, ingestion never waits on
// a schema.
func TestEnsureServiceLogGroup_StartsSampling(t *testing.T) {
	db := testOpen(t)

	cust, err := db.FindOrCreateOrg("ensure-sampling-test")
	if err != nil {
		t.Fatal(err)
	}
	ns, err := db.FindOrCreateNamespace(cust.ID, "ops")
	if err != nil {
		t.Fatal(err)
	}

	if err := db.EnsureServiceLogGroup(ns.ID, "myapp"); err != nil {
		t.Fatalf("EnsureServiceLogGroup: %v", err)
	}
	lg, err := db.FindLogGroup(cust.ID, "ops", "myapp")
	if err != nil || lg == nil {
		t.Fatalf("FindLogGroup = %v, %v", lg, err)
	}
	if lg.Status != StatusConfigured {
		t.Errorf("Status = %q, want %q (ingest must not wait on a schema)", lg.Status, StatusConfigured)
	}
	if lg.ResampleRequestedAt == nil {
		t.Error("ResampleRequestedAt is nil, want set so sample accumulation starts")
	}
}

// TestCountPendingLogGroups_SchemalessDirect: a direct source that has
// collected lines but has no schema yet is awaiting review exactly like
// a pending weftd source; one with a schema, or one that hasn't
// collected anything, is not.
func TestCountPendingLogGroups_SchemalessDirect(t *testing.T) {
	db := testOpen(t)

	cust, err := db.FindOrCreateOrg("count-schemaless-test")
	if err != nil {
		t.Fatal(err)
	}
	ns, err := db.FindOrCreateNamespace(cust.ID, "ops")
	if err != nil {
		t.Fatal(err)
	}

	if err := db.EnsureServiceLogGroup(ns.ID, "myapp"); err != nil {
		t.Fatal(err)
	}
	if n, err := db.CountPendingLogGroups(cust.ID, allNamespaces(db, cust.ID)); err != nil || n != 0 {
		t.Fatalf("before any sample: count = %d, err = %v, want 0", n, err)
	}

	if err := db.UpsertPendingLogGroup(ns.ID, "myapp", "", "", `["a","b"]`, `{}`, 2, true); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.CountPendingLogGroups(cust.ID, allNamespaces(db, cust.ID)); n != 1 {
		t.Errorf("with a partial sample: count = %d, want 1", n)
	}

	if err := db.ConfigureLogGroup(context.Background(), ns.ID, "myapp", "app: myapp\nformat: text\n"); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.CountPendingLogGroups(cust.ID, allNamespaces(db, cust.ID)); n != 0 {
		t.Errorf("after configure: count = %d, want 0", n)
	}
}

// A user-initiated resample of a schemaless direct source resets its sample
// to zero lines. It must stay listed (as sampling) so the user sees their
// request took effect, even if the service is quiet; a brand-new service
// that has not yet received a line stays unlisted (test above).
func TestCountPendingLogGroups_ResampledDirectStaysListedAtZero(t *testing.T) {
	db := testOpen(t)
	cust, err := db.FindOrCreateOrg("count-resampled-test")
	if err != nil {
		t.Fatal(err)
	}
	ns, err := db.FindOrCreateNamespace(cust.ID, "ops")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureServiceLogGroup(ns.ID, "myapp"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertPendingLogGroup(ns.ID, "myapp", "", "", `["a"]`, `{}`, SampleTarget, false); err != nil {
		t.Fatal(err)
	}
	if err := db.RequestResample(ns.ID, "myapp", 0); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.CountPendingLogGroups(cust.ID, allNamespaces(db, cust.ID)); n != 1 {
		t.Errorf("after resample (0 lines collected): count = %d, want 1", n)
	}
}

// allNamespaces is every namespace of an org, the readable set of its owner.
func allNamespaces(db *Store, org OrgID) []string {
	names, _ := db.NamespaceNames(org, "", true)
	return names
}
