package store

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
)

// TestBootstrapToleratesUncreatablePgCron pins the reproducibility fix.
//
// schema.sql carries an unconditional
// `CREATE EXTENSION IF NOT EXISTS pg_cron`, but pg_cron refuses
// creation in any database other than cron.database_name (prod:
// `weft`). So bootstrapping a virgin DB with any other name —
// weft_test, CI, a scratch DB — hard-failed at that statement under
// psql -v ON_ERROR_STOP=1, which silently skipped every store test
// that depends on a fresh bootstrap.
//
// A virgin database whose name is NOT the cron database must still
// bootstrap cleanly from schema.sql; pg_cron is simply not created
// there (its scheduled jobs are only meaningful in the prod DB).
func TestBootstrapToleratesUncreatablePgCron(t *testing.T) {
	base := os.Getenv("WEFT_TEST_DSN")
	if base == "" {
		base = "host=localhost port=5433 dbname=weft_test user=weft password=weft sslmode=disable"
	}
	// Name deliberately != the cron database ("weft") so pg_cron's
	// own guard rejects CREATE EXTENSION here.
	scratch := fmt.Sprintf("weft_boot_%d", os.Getpid())
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

	t.Setenv(AllowSchemaBootstrapEnv, "1")
	if err := Migrate(replaceDBName(base, scratch)); err != nil {
		t.Fatalf("Migrate on a non-cron database must succeed "+
			"(pg_cron should be skipped, not fatal): %v", err)
	}
}
