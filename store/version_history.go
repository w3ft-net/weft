package store

import (
	"crypto/sha256"
	"encoding/hex"
)

// computeShellFunctionHash returns the canonical content hash for a
// /bin script's source. Used at write time and by the backfill
// migration. SHA-256 hex; matches `encode(digest(source, 'sha256'),
// 'hex')` in Postgres.
func computeShellFunctionHash(source string) string {
	h := sha256.Sum256([]byte(source))
	return hex.EncodeToString(h[:])
}

// AmendShellFunctionMessage sets the commit message on a script's current
// version. It is a plain update of the live row, so it does not capture
// history.
func (s *Store) AmendShellFunctionMessage(orgID OrgID, name, message string) error {
	return s.scoped(orgID).Model(&ShellFunction{}).
		Where("name = ?", name).
		Update("message", message).Error
}
