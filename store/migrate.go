package store

import (
	"database/sql"
	"embed"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib" // register "pgx" driver with database/sql
	"github.com/pressly/goose/v3"
)

// Migrations embeds every .sql file under deploy/migrations at build time.
// The coordinator runs these on startup; the integration harness does the
// same to keep its dev postgres in sync.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate applies all pending migrations to the given DSN. Idempotent:
// re-running is safe, and goose records applied versions in
// `goose_db_version`.
func Migrate(dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open for migrate: %w", err)
	}
	defer db.Close()

	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}
	if err := goose.Up(db, "migrations"); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	return nil
}
