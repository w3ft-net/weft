package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"gorm.io/gorm"
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
// history. A non-empty authorID restricts it to a version that user wrote,
// so a teammate's save that landed after yours is never reworded by you.
func (s *Store) AmendShellFunctionMessage(orgID OrgID, authorID, name, message string) error {
	q := s.scoped(orgID).Model(&ShellFunction{}).Where("name = ?", name)
	return amendMessage(q, authorID, message)
}

// amendMessage applies a message update to the live row q selects. With an
// authorID it only matches a version that user wrote, and reports a miss.
func amendMessage(q *gorm.DB, authorID, message string) error {
	if authorID != "" {
		q = q.Where("authored_by_user_id = ?", authorID)
	}
	res := q.Update("message", message)
	if res.Error != nil {
		return res.Error
	}
	if authorID != "" && res.RowsAffected == 0 {
		return errors.New("the current version was written by someone else")
	}
	return nil
}

// AmendDashboardMessage and AmendNotifyAliasesMessage are the /etc
// counterparts of AmendShellFunctionMessage.
func (s *Store) AmendDashboardMessage(orgID OrgID, authorID, name, message string) error {
	q := s.scoped(orgID).Model(&Dashboard{}).Where("name = ?", name)
	return amendMessage(q, authorID, message)
}

func (s *Store) AmendNotifyAliasesMessage(orgID OrgID, authorID, message string) error {
	return amendMessage(s.scoped(orgID).Model(&NotifyConfig{}), authorID, message)
}
