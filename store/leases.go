package store

import (
	"context"
	"time"
)

// The lease and slot-claim methods below implement coordination.Coordination
// for the hosted build, over its Postgres tables, unchanged. The TTL constants
// and the logging helpers moved to package coordination with the interface.

// AcquireLease attempts to acquire or renew the named lease for holder.
// Returns true if the caller now holds it: freshly acquired, renewed
// because it already held it, or taken over from a holder whose lease
// had expired. Returns false if someone else currently holds a
// non-expired lease — the caller does not hold it and must not act as
// if it does.
//
// Single atomic statement, so there's no read-then-write race window
// between checking who holds the lease and claiming it.
//
// Expiry is decided by the database's clock (clock_timestamp(), not the
// transaction's now()), never the caller's: every coordinator judges every
// lease against the same clock, so skew between coordinators cannot shorten
// or lengthen a lease. The holder's own deadline (logdb leaseUntil) is the one
// place a caller's clock still matters, and it is a local, conservative bound.
func (s *Store) AcquireLease(name, holder string, ttl time.Duration) (bool, error) {
	res := s.db.Exec(`
		INSERT INTO leases (name, holder, expires_at, acquired_at)
		VALUES (?, ?, clock_timestamp() + ? * interval '1 second', clock_timestamp())
		ON CONFLICT (name) DO UPDATE SET
			holder = EXCLUDED.holder,
			expires_at = EXCLUDED.expires_at,
			acquired_at = CASE WHEN leases.holder = EXCLUDED.holder
				THEN leases.acquired_at
				ELSE EXCLUDED.acquired_at
			END
		WHERE leases.holder = ? OR leases.expires_at < clock_timestamp()`,
		name, holder, ttl.Seconds(), holder)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// ReleaseLease gives up a lease this holder currently owns. Best-effort
// and optional to call — an unreleased lease simply expires on its own,
// so a crash or missed release is never a correctness problem, only a
// slightly slower handoff.
func (s *Store) ReleaseLease(name, holder string) error {
	return s.db.Exec(`DELETE FROM leases WHERE name = ? AND holder = ?`, name, holder).Error
}

// HeldLeases is the names of the unexpired leases holder holds.
func (s *Store) HeldLeases(ctx context.Context, holder string) ([]string, error) {
	var names []string
	err := s.db.WithContext(ctx).Raw(
		`SELECT name FROM leases WHERE holder = ? AND expires_at > clock_timestamp() ORDER BY name`,
		holder).Scan(&names).Error
	return names, err
}
