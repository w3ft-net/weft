package sqlite

import (
	"context"
	"testing"

	"github.com/w3ft-net/weft/backends/cron/cronstoretest"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/ext/backends/sqlite/sqlitedb"
)

// The crons share the database with the sources: the namespaces a watch is
// homed in are made by the sources store.
func TestCronConformance(t *testing.T) {
	cronstoretest.Run(t, func(t *testing.T) cronstoretest.Subject {
		db, err := sqlitedb.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		ns, err := NewSourceBackend(db)
		if err != nil {
			t.Fatal(err)
		}
		s, err := NewCronBackend(db)
		if err != nil {
			t.Fatal(err)
		}
		return cronstoretest.Subject{
			Store: s,
			Namespace: func(t *testing.T, o shape.Added, name string) uint {
				n, err := ns.FindOrCreateNamespace(o, name)
				if err != nil {
					t.Fatal(err)
				}
				return n.ID
			},
			DeleteNamespace: func(t *testing.T, o shape.Added, name string) {
				if err := ns.DeleteNamespace(context.Background(), o, name); err != nil {
					t.Fatal(err)
				}
			},
		}
	})
}
