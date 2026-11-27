package sqlite

import (
	"testing"

	"github.com/w3ft-net/weft/backends/source/sourcestoretest"
	"github.com/w3ft-net/weft/ext/backends/sqlite/sqlitedb"
)

func TestSourceConformance(t *testing.T) {
	sourcestoretest.Run(t, func(t *testing.T) sourcestoretest.Subject {
		db, err := sqlitedb.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		s, err := NewSourceBackend(db)
		if err != nil {
			t.Fatal(err)
		}
		return sourcestoretest.Subject{Store: s}
	})
}
