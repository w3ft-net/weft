package sqlite

import (
	"context"
	"testing"

	"github.com/w3ft-net/weft/backends/token"
	"github.com/w3ft-net/weft/backends/token/tokenstoretest"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/ext/backends/sqlite/sqlitedb"
)

// The tokens share the database with the sources: the namespaces are made by the
// sources store, and its deleting one takes the tokens by the foreign key.
func TestTokenConformance(t *testing.T) {
	tokenstoretest.Run(t, func(t *testing.T) tokenstoretest.Subject {
		db, err := sqlitedb.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		ns, err := NewSourceBackend(db)
		if err != nil {
			t.Fatal(err)
		}
		s, err := NewTokenBackend(db)
		if err != nil {
			t.Fatal(err)
		}
		return tokenstoretest.Subject{
			Store: s,
			Namespace: func(t *testing.T, o shape.Added, name string) token.NamespaceRef {
				n, err := ns.FindOrCreateNamespace(o, name)
				if err != nil {
					t.Fatal(err)
				}
				return token.NamespaceRef{ID: n.ID, Name: n.Name}
			},
			DeleteNamespace: func(t *testing.T, o shape.Added, name string) {
				if err := ns.DeleteNamespace(context.Background(), o, name); err != nil {
					t.Fatal(err)
				}
			},
		}
	})
}

func TestTokenNewNeedsTheNamespacesTable(t *testing.T) {
	db, err := sqlitedb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := NewTokenBackend(db); err == nil {
		t.Fatal("tokens were created on a database with no namespaces table")
	}
}
