package local

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/coordination"
	"github.com/graybeardcorp/weft/coordination/coordtest"
)

func TestConformance(t *testing.T) {
	coordtest.Run(t, func(t *testing.T) coordtest.Subject {
		path := filepath.Join(t.TempDir(), "coordination.db")
		l, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
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
				if err := l.Close(); err != nil {
					return nil, err
				}
				re, err := Open(path)
				if err != nil {
					return nil, err
				}
				t.Cleanup(func() { re.Close() })
				return re, nil
			},
		}
	})
}
