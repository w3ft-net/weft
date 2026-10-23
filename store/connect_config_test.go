package store

import (
	"fmt"
	"testing"
	"time"
)

// GORM wraps every single-statement Create/Update/Delete in BEGIN … COMMIT
// unless told not to: three round trips per write through PgBouncer, for
// statements that are already atomic. Multi-statement work uses explicit
// Transaction blocks, which are unaffected by this setting.
func TestConnectSkipsGormDefaultTransaction(t *testing.T) {
	s, err := Connect(TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if !s.db.Config.SkipDefaultTransaction {
		t.Fatal("gorm.Config.SkipDefaultTransaction is false: every single-statement write pays for BEGIN and COMMIT")
	}
}

// A migration that changes a table's columns must not break statements the
// running process already sent. With pgx's default exec mode every query
// becomes a named prepared statement, and Postgres refuses to reuse one whose
// result type has changed ("cached plan must not change result type"), which
// broke every coordinator after the 2026-10-23 deploy. Behind PgBouncer the
// stale statements also outlive the process on the pooler's server
// connections, so nothing may be cached.
func TestConnectSurvivesColumnAddedUnderRunningQueries(t *testing.T) {
	s, err := Connect(TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	sqlDB, err := s.db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1) // one connection, so the second query reuses the first's

	table := fmt.Sprintf("exec_mode_probe_%d", time.Now().UnixNano())
	if err := s.db.Exec("CREATE TABLE " + table + " (id int)").Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.db.Exec("DROP TABLE IF EXISTS " + table) })
	if err := s.db.Exec("INSERT INTO " + table + " VALUES (1)").Error; err != nil {
		t.Fatal(err)
	}

	read := func() error {
		var rows []map[string]any
		return s.db.Raw("SELECT * FROM " + table).Scan(&rows).Error
	}
	if err := read(); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Exec("ALTER TABLE " + table + " ADD COLUMN extra int").Error; err != nil {
		t.Fatal(err)
	}
	if err := read(); err != nil {
		t.Fatalf("the same query after a column was added: %v", err)
	}
}
