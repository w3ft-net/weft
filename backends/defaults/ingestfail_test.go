package defaults

import (
	"testing"

	"github.com/w3ft-net/weft/backends/ingestfail/ingeststoretest"
	"github.com/w3ft-net/weft/sqlitedb"
)

func TestIngestFailConformance(t *testing.T) {
	ingeststoretest.Run(t, func(t *testing.T) ingeststoretest.Subject {
		db, err := sqlitedb.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		s, err := NewIngestFailBackend(db)
		if err != nil {
			t.Fatal(err)
		}
		return ingeststoretest.Subject{Store: s}
	})
}
