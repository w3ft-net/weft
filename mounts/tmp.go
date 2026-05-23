package mounts

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/kevlarcon/warp/pkg/shellcore"
	"github.com/kevlarcon/weft/store"
	"gorm.io/gorm"
)

const NoticeKeyTmpExpiry = "tmp_48h_expiry"

type TmpMount struct{ cfg Config }

func (m *TmpMount) Prefix() string { return "/tmp" }

func (m *TmpMount) Caps() shellcore.MountCaps {
	return shellcore.MountCaps{Kind: shellcore.MountLocal, Writable: true}
}

func (m *TmpMount) ReadDir(rel string) ([]fs.DirEntry, error) {
	path := "/tmp"
	if rel != "" {
		path = "/tmp/" + rel
	}
	rows, err := m.cfg.Store.ListTmpFiles(m.cfg.CustomerID, path)
	if err != nil {
		return nil, err
	}
	ids := collectCreatorIDs(rows)
	owners := resolveOwners(m.cfg.Store, ids)
	entries := make([]fs.DirEntry, len(rows))
	for i, r := range rows {
		owner := ""
		if r.CreatedByUserID != nil {
			owner = owners[*r.CreatedByUserID]
		}
		entries[i] = &vfsDirEntry{
			name:  lastSegment(r.Path),
			dir:   r.IsDir,
			size:  r.ByteSize,
			mtime: r.UpdatedAt,
			owner: owner,
		}
	}
	return entries, nil
}

func (m *TmpMount) ReadFile(rel string) ([]byte, error) {
	path := "/tmp/" + rel
	data, err := func() ([]byte, error) {
		f, err := m.cfg.Store.GetTmpFile(m.cfg.CustomerID, path)
		if err != nil {
			return nil, err
		}
		if f.IsDir {
			return nil, store.ErrTmpIsDir
		}
		if f.LineCount == 0 {
			return []byte{}, nil
		}
		var lines []store.TmpFileLine
		if err := m.cfg.Store.DB().Where("file_id = ?", f.ID).
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
	}()
	if err != nil {
		return nil, fmt.Errorf("/tmp/%s: %w", rel, err)
	}
	return data, nil
}

func (m *TmpMount) Stat(rel string) (fs.FileInfo, error) {
	if rel == "" {
		return &vfsFileInfo{name: "tmp", dir: true}, nil
	}
	path := "/tmp/" + rel
	tf, err := m.cfg.Store.GetTmpFile(m.cfg.CustomerID, path)
	if err != nil {
		return nil, fs.ErrNotExist
	}
	return &vfsFileInfo{
		name:  lastSegment(rel),
		dir:   tf.IsDir,
		size:  tf.ByteSize,
		mtime: tf.UpdatedAt,
	}, nil
}

func (m *TmpMount) WriteFile(rel string, data []byte) error {
	path := "/tmp/" + rel
	if err := m.cfg.Store.WriteTmpFile(m.cfg.CustomerID, m.cfg.Tier, m.cfg.UserID, path, data); err != nil {
		return err
	}
	m.noteFirstWrite()
	return nil
}

func (m *TmpMount) Remove(rel string) error {
	path := "/tmp/" + rel
	return func() error {
		f, err := m.cfg.Store.GetTmpFile(m.cfg.CustomerID, path)
		if err != nil {
			return err
		}
		if f.IsDir {
			// Reject if children exist.
			children, err := m.cfg.Store.ListTmpFiles(m.cfg.CustomerID, f.Path)
			if err != nil {
				return err
			}
			if len(children) > 0 {
				return fmt.Errorf("tmp: %s: directory not empty", f.Path)
			}
		}
		return m.cfg.Store.DB().WithContext(store.WithCustomer(context.Background(), m.cfg.CustomerID)).Transaction(func(tx *gorm.DB) error {
			if err := tx.Where("file_id = ?", f.ID).Delete(&store.TmpFileLine{}).Error; err != nil {
				return err
			}
			return tx.Where("id = ? AND updated_at = ?", f.ID, f.UpdatedAt).
				Delete(&store.TmpFile{}).Error
		})
	}()
}

// --- Optional interfaces ---

func (m *TmpMount) AppendFile(rel string, data []byte) error {
	path := "/tmp/" + rel
	if err := func() error {
		p, err := store.NormalizeTmpPath(path)
		if err != nil {
			return err
		}
		if err := m.cfg.Store.EnsureParentDirs(m.cfg.CustomerID, m.cfg.UserID, p, false); err != nil {
			return err
		}

		existing, err := m.cfg.Store.GetTmpFile(m.cfg.CustomerID, p)
		if err != nil && !errors.Is(err, store.ErrTmpNotFound) {
			return err
		}
		if existing != nil && existing.IsDir {
			return store.ErrTmpIsDir
		}

		var existingSize int64
		var startLine int64
		if existing != nil {
			existingSize = existing.ByteSize
			startLine = existing.LineCount
		}
		newSize := existingSize + int64(len(data))
		if newSize > store.TmpMaxFileBytes {
			return fmt.Errorf("%w (%d + %d > %d)", store.ErrTmpFileTooLarge, existingSize, len(data), store.TmpMaxFileBytes)
		}

		allowance := store.TmpStorageAllowance(m.cfg.Tier)
		used, err := m.cfg.Store.TmpUsageBytes(m.cfg.CustomerID)
		if err != nil {
			return err
		}
		if used+int64(len(data)) > allowance {
			return fmt.Errorf("%w (%d + %d > %d)", store.ErrTmpQuotaExceeded, used, len(data), allowance)
		}

		lines := store.SplitLines(data)
		return m.cfg.Store.DB().WithContext(store.WithCustomer(context.Background(), m.cfg.CustomerID)).Transaction(func(tx *gorm.DB) error {
			now := time.Now()
			var fileID uint64
			if existing != nil {
				fileID = existing.ID
				if err := tx.Model(&store.TmpFile{}).
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
				f := store.TmpFile{
					CustomerID: m.cfg.CustomerID,
					Path:       p,
					IsDir:      false,
					ByteSize:   newSize,
					LineCount:  int64(len(lines)),
					CreatedAt:  now,
					UpdatedAt:  now,
				}
				if m.cfg.UserID != "" {
					uid := m.cfg.UserID
					f.CreatedByUserID = &uid
				}
				if err := tx.Create(&f).Error; err != nil {
					return err
				}
				fileID = f.ID
			}
			if len(lines) == 0 {
				return nil
			}
			rows := make([]store.TmpFileLine, len(lines))
			for i, line := range lines {
				rows[i] = store.TmpFileLine{
					FileID:    fileID,
					LineNum:   startLine + int64(i+1),
					Content:   line,
					UpdatedAt: now,
				}
			}
			return tx.CreateInBatches(rows, 500).Error
		})
	}(); err != nil {
		return err
	}
	m.noteFirstWrite()
	return nil
}

func (m *TmpMount) Mkdir(rel string) error {
	path := "/tmp/" + rel
	if err := m.cfg.Store.Mkdir(m.cfg.CustomerID, m.cfg.UserID, path); err != nil {
		return err
	}
	m.noteFirstWrite()
	return nil
}

func (m *TmpMount) RemoveAll(rel string) error {
	path := "/tmp/" + rel
	return func() error {
		p, err := store.NormalizeTmpPath(path)
		if err != nil {
			return err
		}
		return m.cfg.Store.DB().WithContext(store.WithCustomer(context.Background(), m.cfg.CustomerID)).Transaction(func(tx *gorm.DB) error {
			// Collect every row under this subtree so we can delete their
			// line contents, then the rows themselves.
			var files []store.TmpFile
			err := tx.Where("path = ? OR path LIKE ?",
				p, p+"/%").Find(&files).Error
			if err != nil {
				return err
			}
			if len(files) == 0 {
				return store.ErrTmpNotFound
			}
			ids := make([]uint64, len(files))
			for i, f := range files {
				ids[i] = f.ID
			}
			if err := tx.Where("file_id IN ?", ids).Delete(&store.TmpFileLine{}).Error; err != nil {
				return err
			}
			return tx.Where("id IN ?", ids).Delete(&store.TmpFile{}).Error
		})
	}()
}

func (m *TmpMount) CopyFile(srcRel, dstRel string) error {
	src := "/tmp/" + srcRel
	dst := "/tmp/" + dstRel
	if err := func() error {
		src, err := m.cfg.Store.GetTmpFile(m.cfg.CustomerID, src)
		if err != nil {
			return err
		}
		if src.IsDir {
			return fmt.Errorf("%w: cannot copy a directory", store.ErrTmpIsDir)
		}
		dstNorm, err := store.NormalizeTmpPath(dst)
		if err != nil {
			return err
		}
		if existing, _ := m.cfg.Store.GetTmpFile(m.cfg.CustomerID, dstNorm); existing != nil {
			return store.ErrTmpExists
		}
		if err := m.cfg.Store.EnsureParentDirs(m.cfg.CustomerID, m.cfg.UserID, dstNorm, false); err != nil {
			return err
		}

		// Per-file cap is fixed and applied to the source's size — the
		// copy is identical bytes. Customer quota: the copy adds
		// src.ByteSize to the total.
		if src.ByteSize > store.TmpMaxFileBytes {
			return fmt.Errorf("%w (source %d > %d)", store.ErrTmpFileTooLarge, src.ByteSize, store.TmpMaxFileBytes)
		}
		allowance := store.TmpStorageAllowance(m.cfg.Tier)
		used, err := m.cfg.Store.TmpUsageBytes(m.cfg.CustomerID)
		if err != nil {
			return err
		}
		if used+src.ByteSize > allowance {
			return fmt.Errorf("%w (%d + %d > %d)", store.ErrTmpQuotaExceeded, used, src.ByteSize, allowance)
		}

		return m.cfg.Store.DB().WithContext(store.WithCustomer(context.Background(), m.cfg.CustomerID)).Transaction(func(tx *gorm.DB) error {
			now := time.Now()
			dstRow := store.TmpFile{
				CustomerID: m.cfg.CustomerID,
				Path:       dstNorm,
				IsDir:      false,
				ByteSize:   src.ByteSize,
				LineCount:  src.LineCount,
				CreatedAt:  now,
				UpdatedAt:  now,
			}
			if m.cfg.UserID != "" {
				uid := m.cfg.UserID
				dstRow.CreatedByUserID = &uid
			}
			if err := tx.Create(&dstRow).Error; err != nil {
				return err
			}
			// INSERT…SELECT keeps the line copy entirely server-side.
			return tx.Exec(`
				INSERT INTO tmp_file_lines (file_id, line_num, content, updated_at)
				SELECT ?, line_num, content, ?
				FROM tmp_file_lines
				WHERE file_id = ?
			`, dstRow.ID, now, src.ID).Error
		})
	}(); err != nil {
		return err
	}
	m.noteFirstWrite()
	return nil
}

func (m *TmpMount) Rename(oldRel, newRel string) error {
	oldPath := "/tmp/" + oldRel
	newPath := "/tmp/" + newRel
	if err := func() error {
		src, err := m.cfg.Store.GetTmpFile(m.cfg.CustomerID, oldPath)
		if err != nil {
			return err
		}
		dstNorm, err := store.NormalizeTmpPath(newPath)
		if err != nil {
			return err
		}
		if existing, _ := m.cfg.Store.GetTmpFile(m.cfg.CustomerID, dstNorm); existing != nil {
			return store.ErrTmpExists
		}
		// Rename never auto-creates parents (createMissing=false), so the
		// userID arg is unused here — pass empty.
		if err := m.cfg.Store.EnsureParentDirs(m.cfg.CustomerID, "", dstNorm, false); err != nil {
			return err
		}
		now := time.Now()
		return m.cfg.Store.DB().WithContext(store.WithCustomer(context.Background(), m.cfg.CustomerID)).Model(&store.TmpFile{}).
			Where("id = ? AND updated_at = ?", src.ID, src.UpdatedAt).
			Updates(map[string]any{
				"path":       dstNorm,
				"updated_at": now,
			}).Error
	}(); err != nil {
		return err
	}
	m.noteFirstWrite()
	return nil
}

func (m *TmpMount) Touch(rel string) error {
	path := "/tmp/" + rel
	if err := func() error {
		p, err := store.NormalizeTmpPath(path)
		if err != nil {
			return err
		}
		existing, err := m.cfg.Store.GetTmpFile(m.cfg.CustomerID, p)
		if errors.Is(err, store.ErrTmpNotFound) {
			return m.cfg.Store.WriteTmpFile(m.cfg.CustomerID, m.cfg.Tier, m.cfg.UserID, p, nil)
		}
		if err != nil {
			return err
		}
		now := time.Now()
		return m.cfg.Store.DB().WithContext(store.WithCustomer(context.Background(), m.cfg.CustomerID)).Transaction(func(tx *gorm.DB) error {
			if err := tx.Model(&store.TmpFile{}).
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
	}(); err != nil {
		return err
	}
	m.noteFirstWrite()
	return nil
}

func (m *TmpMount) noteFirstWrite() {
	if m.cfg.NoticeQueue == nil {
		return
	}
	m.cfg.NoticeQueue.Maybe(NoticeKeyTmpExpiry)
}

// --- helpers ---

func collectCreatorIDs(rows []store.TmpFile) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.CreatedByUserID != nil {
			out = append(out, *r.CreatedByUserID)
		}
	}
	return out
}
