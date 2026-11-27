package sqlite

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/coordination"
	"github.com/w3ft-net/weft/backends/coordination/coordtest"
	"github.com/w3ft-net/weft/ext/backends/sqlite/sqlitedb"
)

func TestCoordinationConformance(t *testing.T) {
	coordtest.Run(t, func(t *testing.T) coordtest.Subject {
		path := filepath.Join(t.TempDir(), "weft.db")
		db, err := sqlitedb.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		l, err := NewCoordinationBackend(db)
		if err != nil {
			t.Fatal(err)
		}
		return coordtest.Subject{
			C: l,
			Backdate: func(key string, age time.Duration) error {
				_, err := l.db.Exec(`UPDATE cron_claims SET claimed_at = ? WHERE key = ?`,
					time.Now().Add(-age).UnixNano(), key)
				return err
			},
			Reopen: func() (coordination.Coordination, error) {
				// A restarted process opens the file with nothing else holding
				// it, so close the first handle: the claims must be on disk.
				if err := db.Close(); err != nil {
					return nil, err
				}
				re, err := sqlitedb.Open(path)
				if err != nil {
					return nil, err
				}
				t.Cleanup(func() { re.Close() })
				return NewCoordinationBackend(re)
			},
		}
	})
}
