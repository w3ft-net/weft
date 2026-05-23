package store

import (
	"testing"
)

// TestRemoveAndReAddLogGroup reproduces a prod-observed bug: user
// clicks "remove" on a configured source, then later re-configures the
// same source and gets back:
//
//     store error: ERROR: duplicate key value violates unique
//     constraint "idx_ns_app" (SQLSTATE 23505)
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

	cust, err := db.FindOrCreateCustomer("remove-readd-test")
	if err != nil {
		t.Fatal(err)
	}
	ns, err := db.FindOrCreateNamespace(cust.ID, "apinamespace")
	if err != nil {
		t.Fatal(err)
	}

	const app = "apinamespace/api"

	// First configure
	if err := db.AddLogGroup(ns.ID, nil, SourceTypeWeftd, app, app, "local", "schema-v1", "0s"); err != nil {
		t.Fatalf("first AddLogGroup: %v", err)
	}

	// Remove (user clicks "Remove")
	if err := db.DB().Unscoped().Where("namespace_id = ? AND app = ?", ns.ID, app).Delete(&LogGroup{}).Error; err != nil {
		t.Fatalf("remove log group: %v", err)
	}

	// Re-configure (user picks the source up again from pending)
	if err := db.AddLogGroup(ns.ID, nil, SourceTypeWeftd, app, app, "local", "schema-v2", "0s"); err != nil {
		t.Fatalf("second AddLogGroup (after remove) should succeed; got: %v", err)
	}
}
