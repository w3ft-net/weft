package store

import (
	"database/sql"
	_ "embed"
	"embed"
	"fmt"

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

// Migrate applies the collapsed schema (if the DB is virgin) and then
// runs any pending migration deltas. Idempotent: re-running on a fully-
// migrated DB is a no-op.
func Migrate(dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open for migrate: %w", err)
	}
	defer db.Close()

	if err := applyCollapsedSchema(db); err != nil {
		return fmt.Errorf("apply schema.sql: %w", err)
	}

	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}
	if err := goose.Up(db, "migrations"); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	return nil
}

// applyCollapsedSchema runs schema.sql exactly once per database, on a
// DB that has not yet been bootstrapped (no goose_db_version table).
// Subsequent calls find the table already present and short-circuit.
//
// schema.sql carries both the DDL and the goose_db_version row data
// (via `pg_dump --data-only --table=goose_db_version --inserts`), so
// after this runs goose's max version equals the prod high-water mark
// and goose.Up only applies post-collapse migrations.
func applyCollapsedSchema(db *sql.DB) error {
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

	if _, err := db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("exec schema.sql: %w", err)
	}
	return nil
}
