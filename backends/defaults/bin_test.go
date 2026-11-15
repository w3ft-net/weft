package defaults

import (
	"testing"

	"github.com/w3ft-net/weft/backends/bin/binstoretest"
	"github.com/w3ft-net/weft/sqlitedb"
)

func TestBinConformance(t *testing.T) {
	binstoretest.Run(t, func(t *testing.T) binstoretest.Subject {
		db, err := sqlitedb.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		s, err := NewBinBackend(db)
		if err != nil {
			t.Fatal(err)
		}
		return binstoretest.Subject{Store: s}
	})
}
