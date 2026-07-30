package store

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// TmpFile is a row in the tmp_files partitioned table. It represents
// either a regular file (IsDir=false, content in tmp_file_lines) or a
// directory marker (IsDir=true, no lines). Partitioned by UpdatedAt,
// with pg_partman dropping partitions past 48h. Editing a file
// (touch/overwrite/append) bumps updated_at, which routes the row
// into a fresh partition and resets the 48h clock.
//
// Uniqueness of (OrgID, Path) is enforced at the store layer,
// not by a unique index — Postgres requires partition-key columns in
// any unique index on a partitioned table, so an index on
// (org_id, path, updated_at) would allow the same path to exist
// in different partitions. The Get/Save paths below look up the
// single live row via ORDER BY updated_at DESC LIMIT 1.
type TmpFile struct {
	ID              uint64    `gorm:"primaryKey;column:id"`
	OrgID           string    `gorm:"column:org_id;not null"`
	Path            string    `gorm:"column:path;not null"`
	IsDir           bool      `gorm:"column:is_dir;not null;default:false"`
	ByteSize        int64     `gorm:"column:byte_size;not null;default:0"`
	LineCount       int64     `gorm:"column:line_count;not null;default:0"`
	CreatedAt       time.Time `gorm:"column:created_at;not null;default:CURRENT_TIMESTAMP"`
	UpdatedAt       time.Time `gorm:"column:updated_at;not null;default:CURRENT_TIMESTAMP"`
	CreatedByUserID *string   `gorm:"column:created_by_user_id"`
}

// TableName pins the table name; GORM's default pluralizer would
// produce "tmp_files" already but being explicit avoids surprise.
func (TmpFile) TableName() string { return "tmp_files" }

// TmpFileLine is the per-line row in tmp_file_lines. Not exported
// because nothing outside this package needs to see the schema —
// callers get whole files back as []byte (ReadTmpFile). Partitioned
// by updated_at, in lockstep with the parent tmp_files row, so a
// touch / append updates every line's updated_at to keep the
// retention clock unified.
type TmpFileLine struct {
	FileID    uint64    `gorm:"column:file_id;not null"`
	LineNum   int64     `gorm:"column:line_num;not null"`
	Content   string    `gorm:"column:content;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null;default:CURRENT_TIMESTAMP"`
}

func (TmpFileLine) TableName() string { return "tmp_file_lines" }

// Errors returned by /tmp operations. Callers upstream (shellfs) surface
// these directly to the shell user as the error text of a failed write.
var (
	ErrTmpFileTooLarge  = errors.New("tmp: file exceeds per-file size limit")
	ErrTmpQuotaExceeded = errors.New("tmp: storage quota exceeded")
	ErrTmpIsDir         = errors.New("tmp: is a directory")
	ErrTmpNotDir        = errors.New("tmp: not a directory")
	ErrTmpNotFound      = errors.New("tmp: no such file or directory")
	ErrTmpExists        = errors.New("tmp: already exists")
	ErrTmpInvalidPath   = errors.New("tmp: invalid path")
)

// NormalizeTmpPath strips leading/trailing slashes and rejects obvious
// garbage (empty, "..", embedded nulls). Accepts either a full shell
// path ("/tmp/foo") or a /tmp-relative path ("foo") — shellfs splits
// the namespace off before reaching the store, so callers below
// shellfs pass relative paths, but accepting both forms keeps test
// call sites readable.
func NormalizeTmpPath(p string) (string, error) {
	p = strings.TrimPrefix(p, "/")
	p = strings.TrimSuffix(p, "/")
	p = strings.TrimPrefix(p, "tmp/")
	if p == "tmp" {
		p = ""
	}
	if p == "" {
		return "", ErrTmpInvalidPath
	}
	if strings.Contains(p, "\x00") {
		return "", ErrTmpInvalidPath
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return "", ErrTmpInvalidPath
		}
	}
	return p, nil
}

// GetTmpFile returns the single live row for (orgID, path), or
// ErrTmpNotFound. "Live" = the row with the most recent UpdatedAt —
// a stale row in a soon-to-be-dropped partition loses to a fresh one.
func (s *Store) GetTmpFile(orgID string, path string) (*TmpFile, error) {
	p, err := NormalizeTmpPath(path)
	if err != nil {
		return nil, err
	}
	var f TmpFile
	err = s.scoped(orgID).Where("path = ?", p).
		Order("updated_at DESC").First(&f).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrTmpNotFound
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// ListTmpFiles returns the direct children of dirPath (one level
// deep). dirPath="" or "/" or "/tmp" lists the top-level /tmp contents.
// Children that appear in multiple partitions (extremely rare —
// possible if updated_at crosses a partition boundary during edit)
// are deduplicated, keeping the most recent row.
func (s *Store) ListTmpFiles(orgID string, dirPath string) ([]TmpFile, error) {
	dirPath = strings.TrimPrefix(dirPath, "/")
	dirPath = strings.TrimSuffix(dirPath, "/")
	dirPath = strings.TrimPrefix(dirPath, "tmp/")
	if dirPath == "tmp" {
		dirPath = ""
	}
	prefix := ""
	if dirPath != "" {
		prefix = dirPath + "/"
	}

	var rows []TmpFile
	q := s.scoped(orgID)
	if prefix == "" {
		q = q.Where("path NOT LIKE ?", "%/%")
	} else {
		q = q.Where("path LIKE ?", prefix+"%").
			Where("path NOT LIKE ?", prefix+"%/%")
	}
	if err := q.Order("path, updated_at DESC").Find(&rows).Error; err != nil {
		return nil, err
	}

	// Dedup by path (keep the most recent, already first due to order).
	seen := map[string]bool{}
	out := rows[:0]
	for _, r := range rows {
		if seen[r.Path] {
			continue
		}
		seen[r.Path] = true
		out = append(out, r)
	}
	return out, nil
}

// TmpUsageBytes returns the current total byte footprint of a
// customer's /tmp contents (files only — directory rows contribute 0).
func (s *Store) TmpUsageBytes(orgID string) (int64, error) {
	var total int64
	err := s.db.Table("tmp_files").
		Where("org_id = ? AND is_dir = FALSE", orgID).
		Select("COALESCE(SUM(byte_size), 0)").Row().Scan(&total)
	return total, err
}

// CountTmpFilesByUser returns how many /tmp files a user has created in
// an org. Directory rows don't count — a directory holds nothing that
// can expire, so it isn't what the 48h warning is about.
//
// The one caller is the expiry notice: it fires on the write that takes
// a user from zero files to one, which is the only moment the warning is
// new information.
func (s *Store) CountTmpFilesByUser(orgID, userID string) (int64, error) {
	var n int64
	err := s.db.Table("tmp_files").
		Where("org_id = ? AND created_by_user_id = ? AND is_dir = FALSE", orgID, userID).
		Count(&n).Error
	return n, err
}

// Mkdir creates a directory row. Errors if the path already exists
// (as file or dir) or if a parent component is missing or is a file.
// userID is recorded as the creator; empty userID stores NULL.
func (s *Store) Mkdir(orgID string, userID, path string) error {
	p, err := NormalizeTmpPath(path)
	if err != nil {
		return err
	}
	if err := s.EnsureParentDirs(orgID, userID, p, false); err != nil {
		return err
	}
	if _, err := s.GetTmpFile(orgID, p); err == nil {
		return ErrTmpExists
	}
	now := time.Now()
	row := &TmpFile{
		OrgID:     orgID,
		Path:      p,
		IsDir:     true,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if userID != "" {
		row.CreatedByUserID = &userID
	}
	return s.scoped(orgID).Create(row).Error
}

// EnsureParentDirs verifies that every parent component of path exists
// and is a directory. With createMissing=true, missing parents are
// created on the fly (mkdir -p semantics); callers that want strict
// "parent must exist" behaviour pass false. Auto-created parents
// inherit the same userID as the leaf write that triggered them.
func (s *Store) EnsureParentDirs(orgID string, userID, path string, createMissing bool) error {
	parts := strings.Split(path, "/")
	if len(parts) <= 1 {
		return nil // top-level; no parent to check
	}
	cur := ""
	for _, p := range parts[:len(parts)-1] {
		if cur == "" {
			cur = p
		} else {
			cur += "/" + p
		}
		existing, err := s.GetTmpFile(orgID, cur)
		if errors.Is(err, ErrTmpNotFound) {
			if !createMissing {
				return fmt.Errorf("%w: parent %q does not exist", ErrTmpNotFound, cur)
			}
			now := time.Now()
			row := &TmpFile{
				OrgID:     orgID,
				Path:      cur,
				IsDir:     true,
				CreatedAt: now,
				UpdatedAt: now,
			}
			if userID != "" {
				row.CreatedByUserID = &userID
			}
			if err := s.scoped(orgID).Create(row).Error; err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !existing.IsDir {
			return fmt.Errorf("%w: %q is not a directory", ErrTmpNotDir, cur)
		}
	}
	return nil
}

// WriteTmpFile replaces the contents of path with data. Creates the
// file if it doesn't exist. Enforces both TmpMaxFileBytes (per-file)
// and the caller's TmpStorageAllowance (per-customer). userID is
// recorded as the creator on first write; subsequent overwrites
// preserve the original creator (editing isn't authoring).
//
// Caller supplies tier so this layer doesn't need to know about
// subscription lookup. shellfs resolves tier once per request.
func (s *Store) WriteTmpFile(orgID string, tier Tier, userID, path string, data []byte) error {
	p, err := NormalizeTmpPath(path)
	if err != nil {
		return err
	}
	if int64(len(data)) > TmpMaxFileBytes {
		return fmt.Errorf("%w (%d bytes > %d bytes)", ErrTmpFileTooLarge, len(data), TmpMaxFileBytes)
	}
	if err := s.EnsureParentDirs(orgID, userID, p, false); err != nil {
		return err
	}

	existing, err := s.GetTmpFile(orgID, p)
	var existingSize int64
	if err == nil {
		if existing.IsDir {
			return ErrTmpIsDir
		}
		existingSize = existing.ByteSize
	} else if !errors.Is(err, ErrTmpNotFound) {
		return err
	}

	// Quota: the write replaces the existing file, so the delta is
	// (new - old), clamped at zero (a shrink doesn't count against quota).
	allowance := TmpStorageAllowance(tier)
	used, err := s.TmpUsageBytes(orgID)
	if err != nil {
		return err
	}
	delta := int64(len(data)) - existingSize
	if delta > 0 && used+delta > allowance {
		return fmt.Errorf("%w (%d + %d > %d)", ErrTmpQuotaExceeded, used, delta, allowance)
	}

	lines := SplitLines(data)
	return s.scoped(orgID).Transaction(func(tx *gorm.DB) error {
		var fileID uint64
		now := time.Now()
		if existing != nil {
			if err := tx.Where("file_id = ?", existing.ID).
				Delete(&TmpFileLine{}).Error; err != nil {
				return err
			}
			fileID = existing.ID
			if err := tx.Model(&TmpFile{}).
				Where("id = ? AND updated_at = ?", existing.ID, existing.UpdatedAt).
				Updates(map[string]any{
					"byte_size":  int64(len(data)),
					"line_count": int64(len(lines)),
					"updated_at": now,
				}).Error; err != nil {
				return err
			}
		} else {
			f := TmpFile{
				OrgID:     orgID,
				Path:      p,
				IsDir:     false,
				ByteSize:  int64(len(data)),
				LineCount: int64(len(lines)),
				CreatedAt: now,
				UpdatedAt: now,
			}
			if userID != "" {
				f.CreatedByUserID = &userID
			}
			if err := tx.Create(&f).Error; err != nil {
				return err
			}
			fileID = f.ID
		}
		if len(lines) == 0 {
			return nil
		}
		rows := make([]TmpFileLine, len(lines))
		for i, line := range lines {
			rows[i] = TmpFileLine{
				FileID:    fileID,
				LineNum:   int64(i + 1),
				Content:   line,
				UpdatedAt: now,
			}
		}
		// Batch to keep individual INSERTs manageable — 500 is
		// empirically the sweet spot for GORM on Postgres.
		return tx.CreateInBatches(rows, 500).Error
	})
}

// SplitLines splits data on \n, preserving a trailing empty string if
// the input ended with a newline. Matches the behaviour the shell user
// expects: echo 'a\nb\n' → 2 lines, echo 'a\nb' → 2 lines also (no
// trailing newline means the last line is unterminated).
func SplitLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	s := string(data)
	// Drop a single trailing \n so we don't synthesise an empty final
	// line for the common "echo ...\n" case. If the writer wanted a
	// blank last line they can write "\n\n".
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}
