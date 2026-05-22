package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Errors returned by ref-resolution paths. Callers (the wit
// builtin, mostly) surface these as user-facing messages.
var (
	ErrInvalidRef     = errors.New("wit: invalid ref")
	ErrRefNotFound    = errors.New("wit: ref not found")
	ErrAmbiguousHash  = errors.New("wit: ambiguous hash prefix")
)

// ShellFunctionVersion is one entry in a /bin script's history,
// covering both the live row (HEAD) and history rows uniformly.
//
// AuthoredByUserID is the actor who *created* this version's
// content, derived from the captured_by_user_id of the row that
// displaced it (since the trigger captures the displacing user at
// that moment). For the oldest version in the chain (where no
// further history exists), it falls back to the row's original
// created_by_user_id.
type ShellFunctionVersion struct {
	ContentHash       string
	Source            string
	Language          string
	Message           string
	AuthoredAt        time.Time
	AuthoredByUserID  *string
	DisplacedByUserID *string // nil for HEAD
	IsHead            bool
}

// shellFunctionHistoryRow is the on-disk shape of a history row;
// not exported because callers only see the unified Version type.
type shellFunctionHistoryRow struct {
	ID                 uint64    `gorm:"column:id"`
	FunctionID         uint64    `gorm:"column:function_id"`
	CustomerID         uint      `gorm:"column:customer_id"`
	Name               string    `gorm:"column:name"`
	Source             string    `gorm:"column:source"`
	Language           string    `gorm:"column:language"`
	ContentHash        string    `gorm:"column:content_hash"`
	Message            string    `gorm:"column:message"`
	CapturedAt         time.Time `gorm:"column:captured_at"`
	CapturedByUserID   *string   `gorm:"column:captured_by_user_id"`
	AuthoredByUserID   *string   `gorm:"column:authored_by_user_id"`
	PredecessorID      *uint64   `gorm:"column:predecessor_id"`
}

func (shellFunctionHistoryRow) TableName() string { return "shell_function_history" }

// shellFunctionHistoryView is what LogShellFunction reads: the same
// columns plus a SQL-resolved per-version author derived from the
// trigger's captured_by trail (see LogShellFunction for the rule).
type shellFunctionHistoryView struct {
	ID                 uint64
	Source             string
	Language           string
	ContentHash        string
	Message            string
	CapturedAt         time.Time
	CapturedByUserID   *string
	ResolvedAuthorID   *string
}

// computeShellFunctionHash returns the canonical content hash for a
// /bin script's source. Used at write time and by the backfill
// migration. SHA-256 hex; matches `encode(digest(source, 'sha256'),
// 'hex')` in Postgres.
func computeShellFunctionHash(source string) string {
	h := sha256.Sum256([]byte(source))
	return hex.EncodeToString(h[:])
}

// LogShellFunction returns the version chain newest-first for the
// named function, including the live row as the first entry.
// Returns up to 6 entries (5 history + HEAD) when the cap is full.
// Empty result when the named function doesn't exist.
func (s *Store) LogShellFunction(customerID uint, name string) ([]ShellFunctionVersion, error) {
	fn, err := s.GetShellFunction(customerID, name)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}

	// Each version's author = the user who displaced its predecessor,
	// captured into history's captured_by_user_id at displacement
	// time. SQL LEAD shifts captured_by_user_id one row toward newer
	// (the displacer of THIS row's content is the author of the NEXT
	// row's content); the oldest row in the chain has no successor
	// and COALESCEs to the live row's created_by_user_id (the
	// original author). Tiebreak on id DESC so saves within the same
	// microsecond order deterministically.
	var rows []shellFunctionHistoryView
	err = s.db.Raw(`
		SELECT
			id, source, language, content_hash, message, captured_at,
			captured_by_user_id,
			COALESCE(
				LEAD(captured_by_user_id) OVER (
					ORDER BY captured_at DESC, id DESC
				),
				?
			) AS resolved_author_id
		FROM shell_function_history
		WHERE function_id = ?
		ORDER BY captured_at DESC, id DESC
	`, fn.CreatedByUserID, fn.ID).Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	// HEAD lives in shell_functions, not in history. Its author is the
	// captured_by of the row that would be inserted next — i.e. the
	// most recent history row's captured_by, falling back to the
	// original creator when no history exists.
	headAuthor := fn.CreatedByUserID
	if len(rows) > 0 && rows[0].CapturedByUserID != nil {
		headAuthor = rows[0].CapturedByUserID
	}

	out := []ShellFunctionVersion{{
		ContentHash:      fn.ContentHash,
		Source:           fn.Source,
		Language:         fn.Language,
		Message:          fn.Message,
		AuthoredAt:       fn.UpdatedAt,
		AuthoredByUserID: headAuthor,
		IsHead:           true,
	}}
	for _, r := range rows {
		out = append(out, ShellFunctionVersion{
			ContentHash:       r.ContentHash,
			Source:            r.Source,
			Language:          r.Language,
			Message:           r.Message,
			AuthoredAt:        r.CapturedAt,
			AuthoredByUserID:  r.ResolvedAuthorID,
			DisplacedByUserID: r.CapturedByUserID,
			IsHead:            false,
		})
	}
	return out, nil
}

var headCaretRefRe = regexp.MustCompile(`^HEAD(\^(\d*))?$`)

// ResolveShellFunctionRef parses a ref ("HEAD", "HEAD^", "HEAD^N",
// or a hash prefix ≥ 4 chars) and returns the matching version.
func (s *Store) ResolveShellFunctionRef(customerID uint, name, ref string) (*ShellFunctionVersion, error) {
	if m := headCaretRefRe.FindStringSubmatch(ref); m != nil {
		// HEAD or HEAD^N. Convert to a 0-indexed offset where 0 = HEAD.
		offset := 0
		if m[1] != "" {
			if m[2] == "" {
				offset = 1
			} else {
				n, err := strconv.Atoi(m[2])
				if err != nil || n < 0 {
					return nil, fmt.Errorf("%w: %q", ErrInvalidRef, ref)
				}
				offset = n
			}
		}
		return s.resolveByOffset(customerID, name, offset)
	}

	// Hash prefix. Require ≥ 4 chars to avoid pathological matches.
	if len(ref) < 4 {
		return nil, fmt.Errorf("%w: hash prefix must be at least 4 characters", ErrInvalidRef)
	}
	if !isHexString(ref) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidRef, ref)
	}
	return s.resolveByHashPrefix(customerID, name, ref)
}

func (s *Store) resolveByOffset(customerID uint, name string, offset int) (*ShellFunctionVersion, error) {
	versions, err := s.LogShellFunction(customerID, name)
	if err != nil {
		return nil, err
	}
	if offset >= len(versions) {
		return nil, fmt.Errorf("%w: HEAD^%d (only %d versions exist)", ErrRefNotFound, offset, len(versions))
	}
	v := versions[offset]
	return &v, nil
}

func (s *Store) resolveByHashPrefix(customerID uint, name, prefix string) (*ShellFunctionVersion, error) {
	versions, err := s.LogShellFunction(customerID, name)
	if err != nil {
		return nil, err
	}
	var matches []ShellFunctionVersion
	for _, v := range versions {
		if strings.HasPrefix(v.ContentHash, prefix) {
			matches = append(matches, v)
		}
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrRefNotFound, prefix)
	}
	if len(matches) > 1 {
		// Build the candidate list at short-hash form for the error.
		cands := make([]string, len(matches))
		for i, m := range matches {
			cands[i] = m.ContentHash[:7]
		}
		return nil, fmt.Errorf("%w: %s matches %s", ErrAmbiguousHash, prefix, strings.Join(cands, ", "))
	}
	return &matches[0], nil
}

// ShellFunctionContent returns the source bytes of the named
// function at the given ref — what `wit show <path> <ref>` prints.
func (s *Store) ShellFunctionContent(customerID uint, name, ref string) (string, error) {
	v, err := s.ResolveShellFunctionRef(customerID, name, ref)
	if err != nil {
		return "", err
	}
	return v.Source, nil
}

// CheckoutShellFunction makes the version at `ref` the new HEAD.
// Internally writes via SaveShellFunction so the trigger fires;
// the just-displaced live row drops into history. The content's
// original author is preserved on the new HEAD via
// created_by_user_id; userID is the user performing the checkout
// and ends up in captured_by_user_id of the just-captured row.
func (s *Store) CheckoutShellFunction(customerID uint, userID, name, ref string) error {
	target, err := s.ResolveShellFunctionRef(customerID, name, ref)
	if err != nil {
		return err
	}
	// If HEAD already matches the target, nothing to do.
	if target.IsHead {
		return nil
	}

	// SaveShellFunction sets userID as the new HEAD's author. We want
	// the new HEAD to keep the *original* author from `target`. Do
	// the UPDATE directly with the fields we care about preserving.
	return s.scoped(customerID).Transaction(func(tx *gorm.DB) error {
		// Always set, even for empty — see SaveShellFunction for why.
		if err := tx.Exec("SELECT set_config('weft.acting_user_id', ?, true)", userID).Error; err != nil {
			return err
		}
		updates := map[string]any{
			"source":       target.Source,
			"language":     target.Language,
			"content_hash": target.ContentHash,
			"message":      target.Message,
			"updated_at":   time.Now(),
		}
		if target.AuthoredByUserID != nil {
			updates["created_by_user_id"] = *target.AuthoredByUserID
		}
		return tx.Model(&ShellFunction{}).
			Where("name = ?", name).
			Updates(updates).Error
	})
}

// AmendShellFunctionMessage sets the commit message on the version
// at `ref`. For HEAD this UPDATEs shell_functions.message — the
// trigger is scoped to UPDATE OF source, language so a message-only
// edit doesn't capture a new history row. For history refs (HEAD^N
// or hashes), it UPDATEs the matching shell_function_history row
// directly.
func (s *Store) AmendShellFunctionMessage(customerID uint, name, ref, message string) error {
	v, err := s.ResolveShellFunctionRef(customerID, name, ref)
	if err != nil {
		return err
	}
	if v.IsHead {
		return s.scoped(customerID).Model(&ShellFunction{}).
			Where("name = ?", name).
			Update("message", message).Error
	}
	// History row — find by (function_id, content_hash, captured_at)
	// to disambiguate when two rows share content.
	fn, err := s.GetShellFunction(customerID, name)
	if err != nil {
		return err
	}
	return s.db.Model(&shellFunctionHistoryRow{}).
		Where("function_id = ? AND content_hash = ? AND captured_at = ?",
			fn.ID, v.ContentHash, v.AuthoredAt).
		Update("message", message).Error
}

// isHexString reports whether s is a non-empty lowercase hex string.
func isHexString(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}
