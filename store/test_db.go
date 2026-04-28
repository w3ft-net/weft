package store

import (
	"database/sql"
	"fmt"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// ensureTestDatabase creates the database named in dsn if it doesn't
// already exist, using the postgres maintenance database as a foothold.
// Idempotent: a CREATE DATABASE that fails with "already exists" is
// silently swallowed. Returns an error only on true I/O failures or
// when the DSN is malformed.
//
// The unit-test harness uses a dedicated `weft_test` database so the
// dev/integration stack's writes against `weft` (port 5433) can't
// poison transaction-rollback isolation under concurrent load. This
// helper makes that DB self-provisioning so contributors don't have
// to remember a manual `createdb` step.
func ensureTestDatabase(dsn string) error {
	target, err := dsnDBName(dsn)
	if err != nil {
		return err
	}
	if target == "" || target == "postgres" {
		return nil // nothing to do
	}

	maintenanceDSN := replaceDBName(dsn, "postgres")
	conn, err := sql.Open("pgx", maintenanceDSN)
	if err != nil {
		return fmt.Errorf("open postgres maintenance db: %w", err)
	}
	defer conn.Close()

	if err := conn.Ping(); err != nil {
		return fmt.Errorf("ping postgres maintenance db: %w", err)
	}

	// Postgres doesn't support CREATE DATABASE IF NOT EXISTS, but the
	// "already exists" error (SQLSTATE 42P04) is the natural idempotency
	// signal here. Re-running tests against an existing test DB should
	// be a no-op rather than a fatal.
	_, err = conn.Exec(fmt.Sprintf("CREATE DATABASE %s", quoteIdent(target)))
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("create database %q: %w", target, err)
	}
	return nil
}

// dsnDBName extracts the dbname from a libpq-style DSN ("k=v k=v"
// space-separated). Returns "" if no dbname is set; an error only
// if the DSN cannot be parsed at all.
func dsnDBName(dsn string) (string, error) {
	for _, part := range strings.Fields(dsn) {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return "", fmt.Errorf("malformed DSN field %q (expected k=v)", part)
		}
		if k == "dbname" {
			return v, nil
		}
	}
	return "", nil
}

// replaceDBName swaps the dbname in a libpq-style DSN. If dbname isn't
// present, it's appended.
func replaceDBName(dsn, name string) string {
	parts := strings.Fields(dsn)
	for i, p := range parts {
		if strings.HasPrefix(p, "dbname=") {
			parts[i] = "dbname=" + name
			return strings.Join(parts, " ")
		}
	}
	return strings.Join(append(parts, "dbname="+name), " ")
}

// quoteIdent produces a safely-quoted SQL identifier. Test code only —
// the only thing we ever feed it is an alphanumeric DB name from a
// developer's DSN, so the conservative double-quote wrap is enough.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
