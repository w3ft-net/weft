package hosted_test

import (
	"testing"
	"time"

	"github.com/graybeardcorp/weft/coordination"
	"github.com/graybeardcorp/weft/coordination/coordtest"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted"
)

// The hosted coordination is exclusive, TTL-expiring and Postgres-backed. Its
// rows are committed (claims must be visible to every connection to be
// exactly-once), so each subtest forgets what it left under its prefix.
func TestCoordination_Conformance(t *testing.T) {
	coordtest.Run(t, func(t *testing.T) coordtest.Subject {
		db, err := store.Open(store.TestDSN(t))
		if err != nil {
			t.Skipf("postgres unavailable: %v", err)
		}
		t.Cleanup(func() { db.Close() })
		return coordtest.Subject{
			C:               hosted.Coordination(db),
			ExclusiveLeases: true,
			Backdate: func(key string, age time.Duration) error {
				return db.DB().Exec(`UPDATE cron_claims SET claimed_at = ? WHERE key = ?`,
					time.Now().Add(-age).UTC(), key).Error
			},
			Reopen: func() (coordination.Coordination, error) {
				re, err := store.Open(store.TestDSN(t))
				if err != nil {
					return nil, err
				}
				t.Cleanup(func() { re.Close() })
				return hosted.Coordination(re), nil
			},
			Forget: func(prefix string) {
				db.DB().Exec(`DELETE FROM cron_claims WHERE key LIKE ?`, prefix+"%")
				db.DB().Exec(`DELETE FROM leases WHERE name LIKE ?`, prefix+"%")
			},
		}
	})
}
