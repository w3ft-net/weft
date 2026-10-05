package store

import (
	"time"
)

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
