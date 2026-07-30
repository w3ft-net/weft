//go:build test

package store

import (
	"database/sql"
	"fmt"
	"os"
	"testing"

	"github.com/pressly/goose/v3"
)

// TestMigration069AcceptsLegacyNonUUIDMemberIDs pins the outage of
// 2026-07-30.
//
// 069 casts team_members.id from TEXT to UUID on the premise, stated in
// its own header comment, that "weft already stored uuid.New().String()
// in a TEXT column, so every existing value casts cleanly." Production
// disagreed: rows seeded by an older weft-demo-seed carry ids shaped
// `demo-tm-<uuid>-<n>`, which is not a UUID. The cast raised
// SQLSTATE 22P02, goose rolled 069 back, and because the ASG gates
// load-balancer entry on migrations succeeding, every coordinator
// instance signaled ABANDON ~30s into boot and was terminated. The ASG
// sat at zero instances and prod was down until 069 stopped assuming.
//
// The migration must therefore re-key values it cannot cast rather than
// aborting. That is safe precisely here: nothing references
// team_members.id — it is a bare primary key, and the table's only
// foreign key points outward at organizations(id).
//
// Runs against a scratch database taken to version 68 (the state prod
// was wedged at), seeded with a legacy id, then advanced.
func TestMigration069AcceptsLegacyNonUUIDMemberIDs(t *testing.T) {
	db, _ := scratchDBAtVersion68(t, "weft_mig069")

	var orgID string
	if err := db.QueryRow(
		`INSERT INTO public.organizations (id, name, display_name)
		 VALUES (gen_random_uuid(), 'mig069-org', 'Mig069 Org')
		 RETURNING id`).Scan(&orgID); err != nil {
		t.Fatalf("seed organization: %v", err)
	}

	// The exact shape prod carried: an id that is not a UUID, alongside
	// one that is, so the fix is shown to re-key only what it must.
	const legacyID = "demo-tm-a5b076b0-f4c7-4b97-8c57-1e2cb66ea9b5-8"
	var castableID string
	if err := db.QueryRow(
		`INSERT INTO public.team_members (id, org_id, email, role, status)
		 VALUES (gen_random_uuid()::text, $1, 'castable@example.com', 'member', 'active')
		 RETURNING id`, orgID).Scan(&castableID); err != nil {
		t.Fatalf("seed castable member: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO public.team_members (id, org_id, email, role, status)
		 VALUES ($1, $2, 'legacy@example.com', 'owner', 'active')`,
		legacyID, orgID); err != nil {
		t.Fatalf("seed legacy member: %v", err)
	}

	if err := goose.Up(db, "migrations"); err != nil {
		t.Fatalf("069 must tolerate a non-UUID team_members.id "+
			"(prod had %q); instead: %v", legacyID, err)
	}

	// Both rows survive, both are now real UUIDs, and the castable one
	// kept the value it already had — only the uncastable id was minted
	// anew.
	var total int
	if err := db.QueryRow(`SELECT count(*) FROM public.memberships`).Scan(&total); err != nil {
		t.Fatalf("count memberships: %v", err)
	}
	if total != 2 {
		t.Errorf("membership rows = %d, want 2 (the legacy row must be re-keyed, not dropped)", total)
	}

	var keptID string
	if err := db.QueryRow(
		`SELECT id::text FROM public.memberships WHERE email = 'castable@example.com'`,
	).Scan(&keptID); err != nil {
		t.Fatalf("read castable member: %v", err)
	}
	if keptID != castableID {
		t.Errorf("already-castable id was rewritten: got %q, want %q", keptID, castableID)
	}

	// The legacy row is addressable and its role came across under the
	// unified vocabulary (069 folds weft's "owner" into orgs' "admin").
	var legacyRole string
	if err := db.QueryRow(
		`SELECT role FROM public.memberships WHERE email = 'legacy@example.com'`,
	).Scan(&legacyRole); err != nil {
		t.Fatalf("read legacy member: %v", err)
	}
	if legacyRole != "admin" {
		t.Errorf("legacy member role = %q, want %q", legacyRole, "admin")
	}
}

// TestMigration069CollapsesDuplicateMemberships covers the other two ways
// 069 can abort on pre-existing data. It makes idx_memberships_org_user and
// idx_memberships_org_email UNIQUE over pairs team_members never enforced,
// so a duplicate user or address in the table stops the migration — and
// therefore the deploy — the same way the uncastable id did.
//
// Each duplicate group must collapse to one row, keeping the row that best
// represents the membership: accepted over pending, admin over member, then
// oldest.
func TestMigration069CollapsesDuplicateMemberships(t *testing.T) {
	db, _ := scratchDBAtVersion68(t, "weft_mig069dup")

	var orgID string
	if err := db.QueryRow(
		`INSERT INTO public.organizations (id, name, display_name)
		 VALUES (gen_random_uuid(), 'mig069-dup-org', 'Mig069 Dup Org')
		 RETURNING id`).Scan(&orgID); err != nil {
		t.Fatalf("seed organization: %v", err)
	}

	// Same user twice in one org: a pending member row and an accepted
	// admin row. The accepted admin is the real membership.
	if _, err := db.Exec(
		`INSERT INTO public.team_members
		    (id, org_id, email, user_id, role, status, accepted_at, created_at)
		 VALUES
		    (gen_random_uuid()::text, $1, 'dup-user-old@example.com', 'user-dup-1',
		     'member', 'pending', NULL, now() - interval '2 days'),
		    (gen_random_uuid()::text, $1, 'dup-user-new@example.com', 'user-dup-1',
		     'owner', 'active', now(), now() - interval '1 day')`,
		orgID); err != nil {
		t.Fatalf("seed duplicate user_id rows: %v", err)
	}

	// Same address in one org differing only in case — the collision the
	// lower(email) index is designed to prevent, arriving before it exists.
	if _, err := db.Exec(
		`INSERT INTO public.team_members
		    (id, org_id, email, user_id, role, status, accepted_at, created_at)
		 VALUES
		    (gen_random_uuid()::text, $1, 'Ada@example.com', NULL,
		     'member', 'pending', NULL, now() - interval '3 days'),
		    (gen_random_uuid()::text, $1, 'ada@example.com', NULL,
		     'member', 'active', now(), now() - interval '1 day')`,
		orgID); err != nil {
		t.Fatalf("seed duplicate email rows: %v", err)
	}

	if err := goose.Up(db, "migrations"); err != nil {
		t.Fatalf("069 must collapse duplicate memberships rather than abort on "+
			"the new unique indexes; instead: %v", err)
	}

	var userDupes int
	if err := db.QueryRow(
		`SELECT count(*) FROM public.memberships WHERE user_id = 'user-dup-1'`,
	).Scan(&userDupes); err != nil {
		t.Fatalf("count user dupes: %v", err)
	}
	if userDupes != 1 {
		t.Errorf("rows for user-dup-1 = %d, want 1", userDupes)
	}

	// The survivor is the accepted admin, not merely the first inserted.
	var survivorEmail, survivorRole string
	if err := db.QueryRow(
		`SELECT email, role FROM public.memberships WHERE user_id = 'user-dup-1'`,
	).Scan(&survivorEmail, &survivorRole); err != nil {
		t.Fatalf("read user dupe survivor: %v", err)
	}
	if survivorEmail != "dup-user-new@example.com" || survivorRole != "admin" {
		t.Errorf("survivor = (%q, %q), want (%q, %q): the accepted admin row "+
			"should win over the older pending one",
			survivorEmail, survivorRole, "dup-user-new@example.com", "admin")
	}

	var emailDupes int
	if err := db.QueryRow(
		`SELECT count(*) FROM public.memberships WHERE lower(email) = 'ada@example.com'`,
	).Scan(&emailDupes); err != nil {
		t.Fatalf("count email dupes: %v", err)
	}
	if emailDupes != 1 {
		t.Errorf("rows for ada@example.com (case-insensitive) = %d, want 1", emailDupes)
	}
}

// scratchDBAtVersion68 provisions a throwaway database bootstrapped from
// schema.sql and migrated to version 68 — the state production was wedged
// at when 069 failed — and returns it with goose already pointed at the
// embedded migrations. The database is dropped at test cleanup.
func scratchDBAtVersion68(t *testing.T, prefix string) (*sql.DB, string) {
	t.Helper()

	base := os.Getenv("WEFT_TEST_DSN")
	if base == "" {
		base = "host=localhost port=5433 dbname=weft_test user=weft password=weft sslmode=disable"
	}
	// Deliberately not the cron database, per TestBootstrapToleratesUncreatablePgCron.
	scratch := fmt.Sprintf("%s_%d", prefix, os.Getpid())
	maint := replaceDBName(base, "postgres")

	admin, err := sql.Open("pgx", maint)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Skipf("postgres unavailable: %v", err)
	}

	drop := func() {
		admin.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1`, scratch)
		admin.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS %s", quoteIdent(scratch)))
	}
	drop() // clear any leftover from a previous aborted run
	if _, err := admin.Exec(fmt.Sprintf("CREATE DATABASE %s", quoteIdent(scratch))); err != nil {
		admin.Close()
		t.Fatalf("create scratch db: %v", err)
	}

	dsn := replaceDBName(base, scratch)
	t.Setenv(AllowSchemaBootstrapEnv, "1")

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		drop()
		admin.Close()
		t.Fatalf("open scratch db: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		drop()
		admin.Close()
	})

	if err := applyCollapsedSchema(db, dsn); err != nil {
		t.Fatalf("bootstrap schema.sql: %v", err)
	}
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}
	if err := goose.UpTo(db, "migrations", 68); err != nil {
		t.Fatalf("migrate to 68: %v", err)
	}
	return db, dsn
}
