package defaults

import (
	"testing"

	"github.com/w3ft-net/weft/backends/tmp/tmpstoretest"
	"github.com/w3ft-net/weft/sqlitedb"
)

func TestTmpConformance(t *testing.T) {
	tmpstoretest.Run(t, func(t *testing.T) tmpstoretest.Subject {
		db, err := sqlitedb.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		s, err := NewTmpBackend(db)
		if err != nil {
			t.Fatal(err)
		}
		return tmpstoretest.Subject{Store: s}
	})
}
