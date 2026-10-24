package store

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
)

// TestMigrateRestoresMissingIngestionErrors pins the prod state found on
// 2026-10-24: weft-postgres was at the collapsed goose high-water mark but had
// no ingestion_errors table (schema.sql defines it, nothing drops it), so
// every DLQ'd batch's record failed to persist and `weft-admin audit` could
// not count them. A database in that state must get the table back from the
// migration deltas, not only a virgin bootstrap.
func TestMigrateRestoresMissingIngestionErrors(t *testing.T) {
	base := os.Getenv("WEFT_TEST_DSN")
	if base == "" {
		base = "host=localhost port=5433 dbname=weft_test user=weft password=weft sslmode=disable"
	}
	scratch := fmt.Sprintf("weft_ie_%d", os.Getpid())
	admin, err := sql.Open("pgx", replaceDBName(base, "postgres"))
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
	drop()
	if _, err := admin.Exec(fmt.Sprintf("CREATE DATABASE %s", quoteIdent(scratch))); err != nil {
		t.Fatalf("create scratch db: %v", err)
	}
	t.Cleanup(drop)

	dsn := replaceDBName(base, scratch)
	t.Setenv(AllowSchemaBootstrapEnv, "1")
	if err := Migrate(dsn); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Prod's state: goose says current, the table is gone. Forget any delta
	// that recreated it so the next Migrate has it to apply, as prod does.
	if _, err := db.Exec(`DROP TABLE ingestion_errors`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM goose_db_version WHERE version_id > 105`); err != nil {
		t.Fatal(err)
	}

	if err := Migrate(dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM ingestion_errors`).Scan(&n); err != nil {
		t.Fatalf("ingestion_errors still missing after Migrate: %v", err)
	}
}
