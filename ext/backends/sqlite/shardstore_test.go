package sqlite

import (
	"testing"

	"github.com/w3ft-net/weft/backends/shardstore/shardstoretest"
	"github.com/w3ft-net/weft/ext/backends/sqlite/sqlitedb"
)

func TestShardConformance(t *testing.T) {
	shardstoretest.Run(t, func(t *testing.T) shardstoretest.Subject {
		db, err := sqlitedb.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		s, err := NewShardBackend(db)
		if err != nil {
			t.Fatal(err)
		}
		next := uint(0)
		return shardstoretest.Subject{
			Store:      s,
			Concurrent: true,
			Namespace:  func(*testing.T) uint { next++; return next },
		}
	})
}
