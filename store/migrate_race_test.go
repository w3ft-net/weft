package store

import (
	"database/sql"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
)

// schemaObjects returns the sorted names of every table and index in the
// public schema — a cheap fingerprint of "is this schema complete".
func schemaObjects(t *testing.T, dsn string) []string {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open %s: %v", dsn, err)
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT 'table:' || table_name FROM information_schema.tables
		 WHERE table_schema = 'public'
		UNION ALL
		SELECT 'index:' || indexname FROM pg_indexes
		 WHERE schemaname = 'public'
	`)
	if err != nil {
		t.Fatalf("list schema objects: %v", err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, s)
	}
	sort.Strings(got)
	return got
}

// createScratchDB makes an empty database and returns its DSN.
func createScratchDB(t *testing.T, base, name string) string {
	t.Helper()
	admin, err := sql.Open("pgx", replaceDBName(base, "postgres"))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	drop := func() {
		admin.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1`, name)
		admin.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS %s", quoteIdent(name)))
	}
	drop()
	if _, err := admin.Exec(fmt.Sprintf("CREATE DATABASE %s", quoteIdent(name))); err != nil {
		t.Fatalf("create scratch db %s: %v", name, err)
	}
	t.Cleanup(drop)
	return replaceDBName(base, name)
}

// TestConcurrentMigrateBootstrapsCompleteSchema pins the cold-start
// bootstrap race.
//
// applyCollapsedSchema treats the existence of goose_db_version as "this
// database is already bootstrapped" — but that table is created by
// schema.sql itself, partway through the file. Two processes starting
// against a virgin database (in the compose stack, api and coordinator
// boot together) therefore both probe, both see no sentinel, and both
// shell out to psql. One wins; the other aborts mid-file under
// ON_ERROR_STOP with "exit status 3".
//
// The damage outlives the crash. The sentinel now exists, so every
// later run short-circuits and the half-applied schema is never
// repaired — it just surfaces much later as a missing constraint, e.g.
// "there is no unique or exclusion constraint matching the ON CONFLICT
// specification" when seeding fixtures.
//
// Concurrency must not change the outcome: every caller succeeds, and
// the resulting schema is identical to a serial bootstrap.
func TestConcurrentMigrateBootstrapsCompleteSchema(t *testing.T) {
	base := os.Getenv("WEFT_TEST_DSN")
	if base == "" {
		base = "host=localhost port=5433 dbname=weft_test user=weft password=weft sslmode=disable"
	}
	t.Setenv(AllowSchemaBootstrapEnv, "1")

	// Reference: one bootstrap, no contention.
	serialDSN := createScratchDB(t, base, fmt.Sprintf("weft_serial_%d", os.Getpid()))
	if err := Migrate(serialDSN); err != nil {
		t.Fatalf("serial Migrate: %v", err)
	}
	want := schemaObjects(t, serialDSN)
	if len(want) == 0 {
		t.Fatal("serial bootstrap produced no schema objects — test cannot discriminate")
	}

	// Same thing, but with several processes racing the way the compose
	// stack does on a cold start.
	raceDSN := createScratchDB(t, base, fmt.Sprintf("weft_race_%d", os.Getpid()))
	const racers = 4
	errs := make([]error, racers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release them together, to actually contend
			errs[i] = Migrate(raceDSN)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent Migrate #%d failed: %v", i, err)
		}
	}

	got := schemaObjects(t, raceDSN)
	if len(got) != len(want) {
		// Report what a racing bootstrap dropped on the floor; that is
		// the failure operators actually hit, long after the crash.
		missing := map[string]bool{}
		for _, w := range want {
			missing[w] = true
		}
		for _, g := range got {
			delete(missing, g)
		}
		var lost []string
		for m := range missing {
			lost = append(lost, m)
		}
		sort.Strings(lost)
		if len(lost) > 10 {
			lost = append(lost[:10], fmt.Sprintf("... and %d more", len(lost)-10))
		}
		t.Fatalf("concurrent bootstrap produced %d schema objects, serial produced %d; missing: %v",
			len(got), len(want), lost)
	}
}
