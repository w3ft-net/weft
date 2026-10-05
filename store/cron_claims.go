package store

import (
	"context"
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

// CronTombstones returns every cron in claim mode, keyed by watch ID, with
// the time it was tombstoned. One query per reconcile tick.
func (s *Store) CronTombstones() (map[uint]time.Time, error) {
	var rows []struct {
		Name       string
		AcquiredAt time.Time
	}
	err := s.db.Raw(`SELECT name, acquired_at FROM leases WHERE holder = ? AND name LIKE 'cron:%'`,
		CronClaimsHolder).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make(map[uint]time.Time, len(rows))
	for _, r := range rows {
		var id uint
		if _, err := fmt.Sscanf(r.Name, "cron:%d", &id); err == nil {
			out[id] = r.AcquiredAt
		}
	}
	return out, nil
}

// Cron modes as reported by CronModes.
const (
	CronModeLease    = "lease"
	CronModeClaims   = "claims"
	CronModeRollback = "rollback"
)

// CronMode is one enabled cron and which scheduling path owns it.
type CronMode struct {
	WatchID  uint
	OrgID    OrgID
	Name     string
	Schedule string
	// Mode is CronModeClaims (tombstoned), CronModeRollback (inside the
	// post-rollback window, lease path denied), or CronModeLease.
	Mode string
	// Holder and AcquiredAt describe the cron's lease row; empty/zero when
	// it has none. ExpiresAt is zero for a tombstone (infinity).
	Holder     string
	AcquiredAt time.Time
	ExpiresAt  time.Time
}

// CronModes lists every enabled cron with its scheduling mode, ordered by
// watch ID. Read-only; spans all orgs, like the executor's own listing.
func (s *Store) CronModes() ([]CronMode, error) {
	var rows []struct {
		ID         uint
		OrgID      OrgID
		Name       string
		Schedule   string
		Holder     *string
		AcquiredAt *time.Time
		Infinite   bool
		ExpiresAt  *time.Time
	}
	err := s.db.WithContext(WithoutTenantScope(context.Background())).Raw(`
		SELECT w.id, w.org_id, w.name, w.schedule,
		       l.holder, l.acquired_at,
		       l.expires_at = 'infinity' AS infinite,
		       CASE WHEN l.expires_at = 'infinity' THEN NULL ELSE l.expires_at END AS expires_at
		FROM cron_watches w
		LEFT JOIN leases l ON l.name = 'cron:' || w.id
		WHERE w.enabled AND w.deleted_at IS NULL
		ORDER BY w.id`).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]CronMode, 0, len(rows))
	for _, r := range rows {
		m := CronMode{WatchID: r.ID, OrgID: r.OrgID, Name: r.Name, Schedule: r.Schedule, Mode: CronModeLease}
		if r.Holder != nil {
			m.Holder = *r.Holder
		}
		if r.AcquiredAt != nil {
			m.AcquiredAt = *r.AcquiredAt
		}
		if r.ExpiresAt != nil {
			m.ExpiresAt = *r.ExpiresAt
		}
		switch {
		case m.Holder == CronClaimsHolder && r.Infinite:
			m.Mode = CronModeClaims
		case m.Holder == CronRollbackHolder && m.ExpiresAt.After(now):
			m.Mode = CronModeRollback
		}
		out = append(out, m)
	}
	return out, nil
}
