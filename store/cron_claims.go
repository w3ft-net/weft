package store

import (
	"fmt"
	"time"
)

// CronClaimsHolder marks a cron's lease row as "this cron runs under
// per-slot claims". The row has expires_at = infinity, so legacy
// AcquireLease (which only takes a lease whose holder is the caller or
// whose expiry has passed) is denied forever, and the previous holder's
// next renewal loses. See docs/plans/cron-claims-rollout.md.
const (
	CronClaimsHolder   = "claims"
	CronRollbackHolder = "rollback"
	// CronRollbackTTL is how long a rolled-back cron stays unclaimable by
	// the lease path, so claim-mode coordinators have stopped before a
	// lease holder can start.
	CronRollbackTTL = 45 * time.Second
)

// CronLeaseName is the lease row guarding one cron under the legacy path.
func CronLeaseName(watchID uint) string { return fmt.Sprintf("cron:%d", watchID) }

// ClaimCronSlot tries to claim a slot key for holder. Exactly one caller
// per key ever gets true; everyone else gets false. A returned error means
// the claim state is unknown and the caller must not run the slot.
func (s *Store) ClaimCronSlot(key, holder string) (bool, error) {
	res := s.db.Exec(`
		INSERT INTO cron_claims (key, holder) VALUES (?, ?)
		ON CONFLICT (key) DO NOTHING`, key, holder)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// DeleteCronClaimsBefore removes claims older than cutoff and returns how
// many were removed.
func (s *Store) DeleteCronClaimsBefore(cutoff time.Time) (int64, error) {
	res := s.db.Exec(`DELETE FROM cron_claims WHERE claimed_at < ?`, cutoff.UTC())
	return res.RowsAffected, res.Error
}

// TombstoneCronLease switches a cron to claim mode by overwriting its
// lease row with the claims marker, whoever holds it now. Idempotent:
// re-tombstoning keeps the original acquired_at, which the claim scheduler
// uses as the start of the forward quiet period.
func (s *Store) TombstoneCronLease(watchID uint) error {
	return s.db.Exec(`
		INSERT INTO leases (name, holder, expires_at, acquired_at)
		VALUES (?, ?, 'infinity', now())
		ON CONFLICT (name) DO UPDATE SET
			holder = EXCLUDED.holder,
			expires_at = 'infinity',
			acquired_at = CASE WHEN leases.holder = EXCLUDED.holder
				THEN leases.acquired_at
				ELSE EXCLUDED.acquired_at
			END`, CronLeaseName(watchID), CronClaimsHolder).Error
}

// RollbackCronTombstone returns a claim-mode cron to lease mode: the
// tombstone becomes an ordinary lease owned by CronRollbackHolder that
// expires after CronRollbackTTL. The row is never deleted — claim mode
// stops when the tombstone is gone, and the lease path cannot take over
// until the TTL passes, so the two never overlap. Reports whether the cron
// was tombstoned.
func (s *Store) RollbackCronTombstone(watchID uint) (bool, error) {
	res := s.db.Exec(`
		UPDATE leases SET holder = ?, expires_at = ?
		WHERE name = ? AND holder = ?`,
		CronRollbackHolder, time.Now().UTC().Add(CronRollbackTTL),
		CronLeaseName(watchID), CronClaimsHolder)
	return res.RowsAffected > 0, res.Error
}

// CronTombstone reports whether the cron is in claim mode and when it was
// tombstoned.
func (s *Store) CronTombstone(watchID uint) (acquiredAt time.Time, ok bool, err error) {
	var rows []struct{ AcquiredAt time.Time }
	err = s.db.Raw(`SELECT acquired_at FROM leases WHERE name = ? AND holder = ?`,
		CronLeaseName(watchID), CronClaimsHolder).Scan(&rows).Error
	if err != nil || len(rows) == 0 {
		return time.Time{}, false, err
	}
	return rows[0].AcquiredAt, true, nil
}
