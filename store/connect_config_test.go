package store

import "testing"

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
