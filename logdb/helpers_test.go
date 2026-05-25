package logdb

import (
	"testing"

	"github.com/kevlarcon/weft/store"
)

// openSMTestStore opens a transaction-scoped test store, skipping when
// postgres is unavailable. Shared by the bucket/shard registry tests.
func openSMTestStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tx, rollback := db.BeginTest()
	t.Cleanup(rollback)
	return tx
}
