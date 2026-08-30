package store

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
)

// weft embeds saaskit's orgs package (store.Store.Orgs) but owns the
// database. saaskit declares the schema its queries need in
// orgs.SchemaSQL, written idempotently (CREATE TABLE IF NOT EXISTS,
// ADD COLUMN IF NOT EXISTS) precisely so a consumer can re-apply it on
// every migrate and pick up new columns on upgrade.
//
// weft used to not apply it at all: the orgs tables were hand-copied
// into schema.sql at collapse time and then frozen. So when saaskit
// added `memberships.seated` for per-seat billing, weft's table did not
// grow the column, and every request through `resolve org role` failed
// with `column "seated" does not exist` — an Unauthenticated 401 on the
// whole API from a dependency bump alone.
//
// Migrate now applies orgs.SchemaSQL after the collapsed schema and the
// goose deltas, so saaskit owns its own schema and this cannot drift
// again. Asserting on `seated` specifically because that is the column
// that caught it; the contract under test is the general one.
func TestMigrateAppliesSaaskitOrgsSchema(t *testing.T) {
	base := os.Getenv("WEFT_TEST_DSN")
	if base == "" {
		base = "host=localhost port=5433 dbname=weft_test user=weft password=weft sslmode=disable"
	}
	scratch := fmt.Sprintf("weft_orgs_schema_%d", os.Getpid())
	maint := replaceDBName(base, "postgres")

	admin, err := sql.Open("pgx", maint)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}

	drop := func() {
		admin.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1`, scratch)
		admin.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS %s", quoteIdent(scratch)))
	}
	drop() // clear any leftover from a previous aborted run
	if _, err := admin.Exec(fmt.Sprintf("CREATE DATABASE %s", quoteIdent(scratch))); err != nil {
		t.Fatalf("create scratch db: %v", err)
	}
	t.Cleanup(drop)

	dsn := replaceDBName(base, scratch)
	t.Setenv(AllowSchemaBootstrapEnv, "1")
	if err := Migrate(dsn); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open scratch db: %v", err)
	}
	defer db.Close()

	var exists bool
	err = db.QueryRow(`
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'memberships' AND column_name = 'seated'
		)`).Scan(&exists)
	if err != nil {
		t.Fatalf("probe memberships.seated: %v", err)
	}
	if !exists {
		t.Fatal("memberships.seated missing after Migrate: " +
			"weft is not applying orgs.SchemaSQL, so saaskit's schema " +
			"can drift out from under its own queries")
	}

	// The column has to be usable, not merely present — a saaskit query
	// filtering on it is what broke, so exercise that shape.
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM memberships WHERE seated`).Scan(&n); err != nil {
		t.Fatalf("query memberships filtering on seated: %v", err)
	}

	// Re-applying must be a no-op: Migrate runs on every boot, so a
	// non-idempotent orgs schema would fail the second deploy, not the
	// first.
	if err := Migrate(dsn); err != nil {
		t.Fatalf("second Migrate must be a no-op: %v", err)
	}
}
