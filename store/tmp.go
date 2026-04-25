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
// Uniqueness of (CustomerID, Path) is enforced at the store layer,
// not by a unique index — Postgres requires partition-key columns in
// any unique index on a partitioned table, so an index on
// (customer_id, path, updated_at) would allow the same path to exist
// in different partitions. The Get/Save paths below look up the
// single live row via ORDER BY updated_at DESC LIMIT 1.
type TmpFile struct {
	ID              uint64    `gorm:"primaryKey;column:id"`
	CustomerID      uint      `gorm:"column:customer_id;not null"`
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

// tmpFileLine is the per-line row in tmp_file_lines. Not exported
// because nothing outside this package needs to see the schema —
// callers get whole files back as []byte (ReadTmpFile). Partitioned
// by updated_at, in lockstep with the parent tmp_files row, so a
// touch / append updates every line's updated_at to keep the
// retention clock unified.
type tmpFileLine struct {
	FileID    uint64    `gorm:"column:file_id;not null"`
	LineNum   int64     `gorm:"column:line_num;not null"`
	Content   string    `gorm:"column:content;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null;default:CURRENT_TIMESTAMP"`
}

func (tmpFileLine) TableName() string { return "tmp_file_lines" }

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

// normalizeTmpPath strips leading/trailing slashes and rejects obvious
// garbage (empty, "..", embedded nulls). Accepts either a full shell
// path ("/tmp/foo") or a /tmp-relative path ("foo") — shellfs splits
// the namespace off before reaching the store, so callers below
// shellfs pass relative paths, but accepting both forms keeps test
// call sites readable.
func normalizeTmpPath(p string) (string, error) {
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

// GetTmpFile returns the single live row for (customerID, path), or
// ErrTmpNotFound. "Live" = the row with the most recent UpdatedAt —
// a stale row in a soon-to-be-dropped partition loses to a fresh one.
func (s *Store) GetTmpFile(customerID uint, path string) (*TmpFile, error) {
	p, err := normalizeTmpPath(path)
	if err != nil {
		return nil, err
	}
	var f TmpFile
	err = s.db.Where("customer_id = ? AND path = ?", customerID, p).
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
func (s *Store) ListTmpFiles(customerID uint, dirPath string) ([]TmpFile, error) {
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
	q := s.db.Where("customer_id = ?", customerID)
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
func (s *Store) TmpUsageBytes(customerID uint) (int64, error) {
	var total int64
	err := s.db.Table("tmp_files").
		Where("customer_id = ? AND is_dir = FALSE", customerID).
		Select("COALESCE(SUM(byte_size), 0)").Row().Scan(&total)
	return total, err
}

// Mkdir creates a directory row. Errors if the path already exists
// (as file or dir) or if a parent component is missing or is a file.
// userID is recorded as the creator; empty userID stores NULL.
func (s *Store) Mkdir(customerID uint, userID, path string) error {
	p, err := normalizeTmpPath(path)
	if err != nil {
		return err
	}
	if err := s.ensureParentDirs(customerID, userID, p, false); err != nil {
		return err
	}
	if _, err := s.GetTmpFile(customerID, p); err == nil {
		return ErrTmpExists
	}
	now := time.Now()
	row := &TmpFile{
		CustomerID: customerID,
		Path:       p,
		IsDir:      true,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if userID != "" {
		row.CreatedByUserID = &userID
	}
	return s.db.Create(row).Error
}

// ensureParentDirs verifies that every parent component of path exists
// and is a directory. With createMissing=true, missing parents are
// created on the fly (mkdir -p semantics); callers that want strict
// "parent must exist" behaviour pass false. Auto-created parents
// inherit the same userID as the leaf write that triggered them.
func (s *Store) ensureParentDirs(customerID uint, userID, path string, createMissing bool) error {
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
		existing, err := s.GetTmpFile(customerID, cur)
		if errors.Is(err, ErrTmpNotFound) {
			if !createMissing {
				return fmt.Errorf("%w: parent %q does not exist", ErrTmpNotFound, cur)
			}
			now := time.Now()
			row := &TmpFile{
				CustomerID: customerID,
				Path:       cur,
				IsDir:      true,
				CreatedAt:  now,
				UpdatedAt:  now,
			}
			if userID != "" {
				row.CreatedByUserID = &userID
			}
			if err := s.db.Create(row).Error; err != nil {
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
func (s *Store) WriteTmpFile(customerID uint, tier Tier, userID, path string, data []byte) error {
	p, err := normalizeTmpPath(path)
	if err != nil {
		return err
	}
	if int64(len(data)) > TmpMaxFileBytes {
		return fmt.Errorf("%w (%d bytes > %d bytes)", ErrTmpFileTooLarge, len(data), TmpMaxFileBytes)
	}
	if err := s.ensureParentDirs(customerID, userID, p, false); err != nil {
		return err
	}

	existing, err := s.GetTmpFile(customerID, p)
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
	used, err := s.TmpUsageBytes(customerID)
	if err != nil {
		return err
	}
	delta := int64(len(data)) - existingSize
	if delta > 0 && used+delta > allowance {
		return fmt.Errorf("%w (%d + %d > %d)", ErrTmpQuotaExceeded, used, delta, allowance)
	}

	lines := splitLines(data)
	return s.db.Transaction(func(tx *gorm.DB) error {
		var fileID uint64
		now := time.Now()
		if existing != nil {
			if err := tx.Where("file_id = ?", existing.ID).
				Delete(&tmpFileLine{}).Error; err != nil {
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
				CustomerID: customerID,
				Path:       p,
				IsDir:      false,
				ByteSize:   int64(len(data)),
				LineCount:  int64(len(lines)),
				CreatedAt:  now,
				UpdatedAt:  now,
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
		rows := make([]tmpFileLine, len(lines))
		for i, line := range lines {
			rows[i] = tmpFileLine{
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

// AppendTmpFile appends data to an existing file, or creates it if
// absent. Enforces both size limits as with WriteTmpFile. Creator
// attribution: appending to an existing file leaves the creator
// unchanged (the original author still owns it). Creating a new
// file via append records userID as the creator, matching how `>>`
// on a missing path behaves.
func (s *Store) AppendTmpFile(customerID uint, tier Tier, userID, path string, data []byte) error {
	p, err := normalizeTmpPath(path)
	if err != nil {
		return err
	}
	if err := s.ensureParentDirs(customerID, userID, p, false); err != nil {
		return err
	}

	existing, err := s.GetTmpFile(customerID, p)
	if err != nil && !errors.Is(err, ErrTmpNotFound) {
		return err
	}
	if existing != nil && existing.IsDir {
		return ErrTmpIsDir
	}

	var existingSize int64
	var startLine int64
	if existing != nil {
		existingSize = existing.ByteSize
		startLine = existing.LineCount
	}
	newSize := existingSize + int64(len(data))
	if newSize > TmpMaxFileBytes {
		return fmt.Errorf("%w (%d + %d > %d)", ErrTmpFileTooLarge, existingSize, len(data), TmpMaxFileBytes)
	}

	allowance := TmpStorageAllowance(tier)
	used, err := s.TmpUsageBytes(customerID)
	if err != nil {
		return err
	}
	if used+int64(len(data)) > allowance {
		return fmt.Errorf("%w (%d + %d > %d)", ErrTmpQuotaExceeded, used, len(data), allowance)
	}

	lines := splitLines(data)
	return s.db.Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		var fileID uint64
		if existing != nil {
			fileID = existing.ID
			if err := tx.Model(&TmpFile{}).
				Where("id = ? AND updated_at = ?", existing.ID, existing.UpdatedAt).
				Updates(map[string]any{
					"byte_size":  newSize,
					"line_count": startLine + int64(len(lines)),
					"updated_at": now,
				}).Error; err != nil {
				return err
			}
			// Bump every existing line's updated_at to keep the
			// retention clock unified — we don't want partman
			// dropping old lines while new lines stay around.
			// Postgres routes each row to the partition matching
			// the new updated_at automatically (PG11+).
			if err := tx.Exec(
				"UPDATE tmp_file_lines SET updated_at = ? WHERE file_id = ?",
				now, fileID,
			).Error; err != nil {
				return err
			}
		} else {
			f := TmpFile{
				CustomerID: customerID,
				Path:       p,
				IsDir:      false,
				ByteSize:   newSize,
				LineCount:  int64(len(lines)),
				CreatedAt:  now,
				UpdatedAt:  now,
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
		rows := make([]tmpFileLine, len(lines))
		for i, line := range lines {
			rows[i] = tmpFileLine{
				FileID:    fileID,
				LineNum:   startLine + int64(i+1),
				Content:   line,
				UpdatedAt: now,
			}
		}
		return tx.CreateInBatches(rows, 500).Error
	})
}

// ReadTmpFile returns the exact bytes that were written — including a
// trailing newline iff the writer supplied one. ByteSize is the source
// of truth for input length; we compare it against the joined-content
// length to decide whether to append a \n on the way out.
func (s *Store) ReadTmpFile(customerID uint, path string) ([]byte, error) {
	f, err := s.GetTmpFile(customerID, path)
	if err != nil {
		return nil, err
	}
	if f.IsDir {
		return nil, ErrTmpIsDir
	}
	if f.LineCount == 0 {
		return []byte{}, nil
	}
	var lines []tmpFileLine
	if err := s.db.Where("file_id = ?", f.ID).
		Order("line_num").Find(&lines).Error; err != nil {
		return nil, err
	}
	var b strings.Builder
	b.Grow(int(f.ByteSize))
	for i, ln := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(ln.Content)
	}
	// If the writer supplied a trailing newline, byte_size > joined
	// length by exactly one. Restore it so `cat` round-trips cleanly.
	if int64(b.Len()) < f.ByteSize {
		b.WriteByte('\n')
	}
	return []byte(b.String()), nil
}

// DeleteTmpFile removes a single file or empty directory. Errors if
// the target is a non-empty directory (use DeleteTmpSubtree for that).
func (s *Store) DeleteTmpFile(customerID uint, path string) error {
	f, err := s.GetTmpFile(customerID, path)
	if err != nil {
		return err
	}
	if f.IsDir {
		// Reject if children exist.
		children, err := s.ListTmpFiles(customerID, f.Path)
		if err != nil {
			return err
		}
		if len(children) > 0 {
			return fmt.Errorf("tmp: %s: directory not empty", f.Path)
		}
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("file_id = ?", f.ID).Delete(&tmpFileLine{}).Error; err != nil {
			return err
		}
		return tx.Where("id = ? AND updated_at = ?", f.ID, f.UpdatedAt).
			Delete(&TmpFile{}).Error
	})
}

// DeleteTmpSubtree removes a directory and everything under it
// (rm -rf semantics). Target may be a file, in which case it behaves
// like DeleteTmpFile.
func (s *Store) DeleteTmpSubtree(customerID uint, path string) error {
	p, err := normalizeTmpPath(path)
	if err != nil {
		return err
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		// Collect every row under this subtree so we can delete their
		// line contents, then the rows themselves.
		var files []TmpFile
		err := tx.Where("customer_id = ? AND (path = ? OR path LIKE ?)",
			customerID, p, p+"/%").Find(&files).Error
		if err != nil {
			return err
		}
		if len(files) == 0 {
			return ErrTmpNotFound
		}
		ids := make([]uint64, len(files))
		for i, f := range files {
			ids[i] = f.ID
		}
		if err := tx.Where("file_id IN ?", ids).Delete(&tmpFileLine{}).Error; err != nil {
			return err
		}
		return tx.Where("id IN ?", ids).Delete(&TmpFile{}).Error
	})
}

// CopyTmpFile duplicates an existing /tmp file to a new path. Single
// transaction: insert metadata row, then INSERT…SELECT every line.
// Enforces per-file cap and per-customer quota against the source's
// existing size (the copy adds exactly that much). Errors if dst
// already exists or if the source is a directory (no recursive cp).
// userID is recorded as the destination's creator — the user doing
// the copy, not the source's creator.
func (s *Store) CopyTmpFile(customerID uint, tier Tier, userID, srcPath, dstPath string) error {
	src, err := s.GetTmpFile(customerID, srcPath)
	if err != nil {
		return err
	}
	if src.IsDir {
		return fmt.Errorf("%w: cannot copy a directory", ErrTmpIsDir)
	}
	dstNorm, err := normalizeTmpPath(dstPath)
	if err != nil {
		return err
	}
	if existing, _ := s.GetTmpFile(customerID, dstNorm); existing != nil {
		return ErrTmpExists
	}
	if err := s.ensureParentDirs(customerID, userID, dstNorm, false); err != nil {
		return err
	}

	// Per-file cap is fixed and applied to the source's size — the
	// copy is identical bytes. Customer quota: the copy adds
	// src.ByteSize to the total.
	if src.ByteSize > TmpMaxFileBytes {
		return fmt.Errorf("%w (source %d > %d)", ErrTmpFileTooLarge, src.ByteSize, TmpMaxFileBytes)
	}
	allowance := TmpStorageAllowance(tier)
	used, err := s.TmpUsageBytes(customerID)
	if err != nil {
		return err
	}
	if used+src.ByteSize > allowance {
		return fmt.Errorf("%w (%d + %d > %d)", ErrTmpQuotaExceeded, used, src.ByteSize, allowance)
	}

	return s.db.Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		dst := TmpFile{
			CustomerID: customerID,
			Path:       dstNorm,
			IsDir:      false,
			ByteSize:   src.ByteSize,
			LineCount:  src.LineCount,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		if userID != "" {
			dst.CreatedByUserID = &userID
		}
		if err := tx.Create(&dst).Error; err != nil {
			return err
		}
		// INSERT…SELECT keeps the line copy entirely server-side.
		return tx.Exec(`
			INSERT INTO tmp_file_lines (file_id, line_num, content, updated_at)
			SELECT ?, line_num, content, ?
			FROM tmp_file_lines
			WHERE file_id = ?
		`, dst.ID, now, src.ID).Error
	})
}

// RenameTmpFile changes a file's path. Single UPDATE; line rows are
// untouched (they reference file_id, not path). Errors if dst
// already exists or if src is missing.
func (s *Store) RenameTmpFile(customerID uint, srcPath, dstPath string) error {
	src, err := s.GetTmpFile(customerID, srcPath)
	if err != nil {
		return err
	}
	dstNorm, err := normalizeTmpPath(dstPath)
	if err != nil {
		return err
	}
	if existing, _ := s.GetTmpFile(customerID, dstNorm); existing != nil {
		return ErrTmpExists
	}
	// Rename never auto-creates parents (createMissing=false), so the
	// userID arg is unused here — pass empty.
	if err := s.ensureParentDirs(customerID, "", dstNorm, false); err != nil {
		return err
	}
	now := time.Now()
	return s.db.Model(&TmpFile{}).
		Where("id = ? AND updated_at = ?", src.ID, src.UpdatedAt).
		Updates(map[string]any{
			"path":       dstNorm,
			"updated_at": now,
		}).Error
}

// TouchTmpFile resets the 48h retention clock on a file: bump
// updated_at on the metadata row and on every line row. Postgres
// re-routes each row into the partition matching its new updated_at
// (PG11+). If the file doesn't exist, creates an empty one (with
// userID as creator). Touching an existing file leaves the creator
// unchanged.
func (s *Store) TouchTmpFile(customerID uint, tier Tier, userID, path string) error {
	p, err := normalizeTmpPath(path)
	if err != nil {
		return err
	}
	existing, err := s.GetTmpFile(customerID, p)
	if errors.Is(err, ErrTmpNotFound) {
		return s.WriteTmpFile(customerID, tier, userID, p, nil)
	}
	if err != nil {
		return err
	}
	now := time.Now()
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&TmpFile{}).
			Where("id = ? AND updated_at = ?", existing.ID, existing.UpdatedAt).
			Update("updated_at", now).Error; err != nil {
			return err
		}
		// Bump every line in lockstep to keep the retention clock
		// unified. Skip when the row is a directory or has no lines.
		if existing.IsDir || existing.LineCount == 0 {
			return nil
		}
		return tx.Exec(
			"UPDATE tmp_file_lines SET updated_at = ? WHERE file_id = ?",
			now, existing.ID,
		).Error
	})
}

// DeleteAllTmpForCustomer wipes every tmp row for a customer. Used on
// account deletion — partman would eventually drop the partitions, but
// the store hook runs in the same transaction as customer soft-delete
// so a re-signup doesn't briefly see stale files.
func (s *Store) DeleteAllTmpForCustomer(customerID uint) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		var ids []uint64
		if err := tx.Model(&TmpFile{}).
			Where("customer_id = ?", customerID).
			Pluck("id", &ids).Error; err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		if err := tx.Where("file_id IN ?", ids).Delete(&tmpFileLine{}).Error; err != nil {
			return err
		}
		return tx.Where("customer_id = ?", customerID).Delete(&TmpFile{}).Error
	})
}

// splitLines splits data on \n, preserving a trailing empty string if
// the input ended with a newline. Matches the behaviour the shell user
// expects: echo 'a\nb\n' → 2 lines, echo 'a\nb' → 2 lines also (no
// trailing newline means the last line is unterminated).
func splitLines(data []byte) []string {
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
