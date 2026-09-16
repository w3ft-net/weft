package store

import (
	"context"
	"database/sql"
	"embed"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/graybeardcorp/saaskit/orgs"
	_ "github.com/jackc/pgx/v5/stdlib" // register "pgx" driver with database/sql
	"github.com/pressly/goose/v3"
)

// migrationsFS embeds the post-collapse migration deltas. Pre-collapse
// migrations live in store/schema.sql; goose only sees this directory,
// which holds files with version numbers strictly greater than the
// collapsed high-water mark recorded inside schema.sql's
// goose_db_version data.
//
// `all:` includes the .gitkeep placeholder so the embed directive is
// valid in a freshly-collapsed repo with no .sql files. Goose ignores
// non-.sql files in the directory.
//
//go:embed all:migrations
var migrationsFS embed.FS

// schemaSQL is the collapsed snapshot of production's schema +
// goose_db_version ledger. Empty until the first `make collapse`. On a
// virgin DB Migrate applies this before invoking goose; on a non-virgin
// DB the file is ignored so re-runs are idempotent.
//
//go:embed schema.sql
var schemaSQL string

// AllowSchemaBootstrapEnv is the env-var gate on schema.sql apply.
// When the DB has no `goose_db_version` table, applyCollapsedSchema
// refuses to run unless this is "1". Production DBs are always
// already bootstrapped — a missing version table on prod indicates
// corruption, not first-run, and we'd rather fail loud than silently
// restore a possibly-stale snapshot. Dev/CI bootstrap entry points
// (test fixtures, dev/demo/up.sh) set the flag explicitly.
const AllowSchemaBootstrapEnv = "WEFT_ALLOW_SCHEMA_BOOTSTRAP"

// migrateLockKey serializes the whole Migrate sequence (schema.sql
// bootstrap + goose deltas) across processes racing to migrate the same
// database on cold start (the compose stack, or several coordinator
// instances booting together). Arbitrary but stable — every process
// migrating this database must agree on it, so it must never change.
//
// Originally only guarded the schema.sql restore (see
// applyCollapsedSchema's history); widened to also cover goose.Up after
// TestConcurrentMigrateBootstrapsCompleteSchema caught concurrent
// processes racing to apply the same pending goose migration once
// store/migrations/ held real post-collapse deltas again (074, 075) —
// goose.Up has no locking of its own in this codebase, so two callers
// both past the schema.sql probe would both see the same migration as
// pending and both try to CREATE the same object.
const migrateLockKey int64 = 0x7746540B0057

// Migrate applies the collapsed schema (if the DB is virgin and
// AllowSchemaBootstrapEnv is set) and then runs any pending migration
// deltas. Idempotent: re-running on a fully-migrated DB is a no-op.
func Migrate(dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open for migrate: %w", err)
	}
	defer db.Close()

	// Session-scoped lock, so it must be taken and released on one
	// pinned connection, not the pool — held for the whole function so
	// applyCollapsedSchema's psql restore and goose.Up serialize as one
	// unit, not two separately-racy steps.
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migrate lock connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrateLockKey); err != nil {
		return fmt.Errorf("acquire migrate lock: %w", err)
	}
	defer func() {
		// Best effort: closing the connection drops the lock anyway.
		_, _ = conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, migrateLockKey)
	}()

	if err := applyCollapsedSchema(db, dsn); err != nil {
		return fmt.Errorf("apply schema.sql: %w", err)
	}

	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}
	// goose.Up returns ErrNoMigrationFiles when the embedded
	// migrations/ directory holds no .sql files. After a successful
	// `make collapse`, migrations/ is empty (just a .gitkeep) — every
	// historical migration has been folded into schema.sql. Treat the
	// empty case as a no-op: the schema is already at the collapsed
	// high-water mark and there's nothing further to apply.
	if err := goose.Up(db, "migrations"); err != nil && !errors.Is(err, goose.ErrNoMigrationFiles) {
		return fmt.Errorf("goose up: %w", err)
	}

	// saaskit owns the schema its own queries read. We embed its orgs
	// package (Store.Orgs) but own the database, so the tables it needs
	// have to be applied here — and re-applied on every migrate, because
	// a saaskit upgrade can introduce a column that its queries assume.
	//
	// orgs.SchemaSQL is written for exactly this: every statement is
	// CREATE TABLE IF NOT EXISTS / ADD COLUMN IF NOT EXISTS / CREATE
	// INDEX IF NOT EXISTS, so running it against an already-current
	// database is a no-op. It runs last so it can add columns to tables
	// the collapsed schema and the goose deltas have already created.
	//
	// Before this, the orgs tables were hand-copied into schema.sql at
	// collapse time and then frozen: saaskit added memberships.seated
	// for per-seat billing, weft's table never grew it, and every
	// request through `resolve org role` 401'd on `column "seated" does
	// not exist`. Applying the source of truth removes that whole class
	// of drift rather than chasing it one column at a time.
	if _, err := db.Exec(orgs.SchemaSQL); err != nil {
		return fmt.Errorf("apply orgs schema: %w", err)
	}
	return nil
}

// applyCollapsedSchema runs schema.sql via psql exactly once per
// database, on a virgin DB (no goose_db_version table) where
// AllowSchemaBootstrapEnv is set. Subsequent calls find the table
// already present and short-circuit.
//
// We shell out to psql rather than db.Exec(schemaSQL) for two
// semantic reasons:
//  1. schema.sql is pg_dump output. Modern pg_dump (16.x+) emits
//     psql meta-commands like `\restrict <key>` / `\unrestrict <key>`
//     that gate the restore behind a one-shot ACL — those are
//     psql-only and the SQL wire protocol rejects them.
//  2. pg_dump puts `SELECT set_config('search_path', ”, false)` at
//     the top of its output to force fully-qualified DDL. That
//     setting is session-scoped, but in the *sql.DB pool path the
//     poisoned connection goes back to the pool and goose picks it
//     up — failing to find unqualified `goose_db_version`. psql
//     runs in its own subprocess; the SET stays contained.
//
// The env-var guard is the safety net: prod should never hit a virgin
// DB, and if it does, blindly restoring an embedded snapshot is the
// wrong move. Dev/CI explicitly opt in.
//
// Callers must already hold migrateLockKey (Migrate does) — this
// function used to take its own lock here, but the "already
// bootstrapped" sentinel below is goose_db_version, which schema.sql
// creates itself partway through the file, so two processes racing
// this specific probe-and-apply had the same failure mode goose.Up
// turned out to have too; one lock spanning both fixes both.
func applyCollapsedSchema(db *sql.DB, dsn string) error {
	if len(schemaSQL) == 0 {
		return nil // pre-collapse repos have an empty placeholder schema.sql
	}

	var hasVersionTable bool
	err := db.QueryRow(`
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			 WHERE table_schema = 'public' AND table_name = 'goose_db_version'
		)
	`).Scan(&hasVersionTable)
	if err != nil {
		return fmt.Errorf("probe goose_db_version: %w", err)
	}
	if hasVersionTable {
		return nil
	}

	if os.Getenv(AllowSchemaBootstrapEnv) != "1" {
		return fmt.Errorf(
			"DB has no goose_db_version table and %s is not set: "+
				"refusing to apply store/schema.sql. Production DBs are "+
				"always already bootstrapped — a missing version table on "+
				"prod indicates corruption, not first-run. Set %s=1 only "+
				"in dev/CI bootstrap paths (test fixtures, dev/demo/up.sh).",
			AllowSchemaBootstrapEnv, AllowSchemaBootstrapEnv,
		)
	}

	cmd := exec.Command("psql", "-v", "ON_ERROR_STOP=1", "-d", dsn)
	cmd.Stdin = strings.NewReader(stripUnknownPSQLMetaCommands(guardPgCronExtension(schemaSQL)))
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("psql apply schema.sql: %w", err)
	}
	return nil
}

// guardPgCronExtension rewrites schema.sql's
// `CREATE EXTENSION ... pg_cron ...;` into a DO/EXCEPTION block so a
// failure to create pg_cron is non-fatal.
//
// pg_cron refuses CREATE EXTENSION in any database other than the one
// named by cron.database_name (prod: `weft`). pg_dump emits the
// statement unconditionally, so bootstrapping any other database
// (weft_test, CI, a scratch DB) hard-failed here under
// psql -v ON_ERROR_STOP=1 and aborted the whole restore — pg_cron's
// scheduled jobs are only meaningful in the prod DB anyway, so on
// every other database we want the rest of the schema to apply and
// pg_cron to simply be absent. The real prod `weft` DB still creates
// it normally (the EXECUTE succeeds there).
//
// This lives at apply time alongside stripUnknownPSQLMetaCommands for
// the same reason: schema.sql is pg_dump output we normalise for the
// runtime environment rather than hand-editing the generated file.
// pg_dump emits CREATE EXTENSION as a single line.
func guardPgCronExtension(sql string) string {
	lines := strings.Split(sql, "\n")
	out := make([]string, 0, len(lines)+4)
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "CREATE EXTENSION") && strings.Contains(trimmed, "pg_cron") {
			stmt := strings.TrimSuffix(strings.TrimSpace(trimmed), ";")
			lit := strings.ReplaceAll(stmt, "'", "''")
			out = append(out,
				"DO $weft_pgcron_guard$ BEGIN",
				"  EXECUTE '"+lit+"';",
				"EXCEPTION WHEN OTHERS THEN",
				"  RAISE NOTICE 'weft bootstrap: pg_cron unavailable in this database (%), continuing without it', SQLERRM;",
				"END $weft_pgcron_guard$;",
			)
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// stripUnknownPSQLMetaCommands removes psql meta-commands that
// older client versions don't recognise. pg_dump emits `\restrict`
// and `\unrestrict` as ACL gates for the restore session — these
// were added to psql in a recent version (17+ or 18+ depending on
// the source); a Debian-stable psql client (typically 15.x in our
// AMIs, 17.4 on dev macOS) errors with "invalid command \restrict".
// Stripping them is safe: they're access-control wrappers, not DDL,
// and the calling user (the migration runner) already has the
// permissions they're meant to gate.
func stripUnknownPSQLMetaCommands(sql string) string {
	lines := strings.Split(sql, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, `\restrict`) || strings.HasPrefix(trimmed, `\unrestrict`) {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
