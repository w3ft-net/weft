package mounts

import (
	"fmt"
	"io/fs"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/w3ft-net/weft/backends/tmp"
	"github.com/w3ft-net/weft/shape"
)

const NoticeKeyTmpExpiry = "tmp_48h_expiry"

// TmpMount is /tmp over a tmp.Store: the mount only adapts the contract to
// the file system the shell sees. There is no Authorize on purpose: any principal in
// the group may read and write all of its /tmp.
type TmpMount struct{ cfg Config }

func (m *TmpMount) Prefix() string { return "/tmp" }

func (m *TmpMount) Caps() shellcore.MountCaps {
	return shellcore.MountCaps{Writable: true}
}

// tmp is the store the /tmp mount reads and writes, the owner it acts for, and
// the limits its writes are held to.
func (c Config) tmp() (st tmp.Store, owner shape.Added, lim tmp.Limits) {
	return c.Tmp, c.partition(), c.TmpLimits
}

func (m *TmpMount) ReadDir(rel string) ([]fs.DirEntry, error) {
	st, owner, _ := m.cfg.tmp()
	rows, err := st.List(owner, rel)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.CreatedBy != "" {
			ids = append(ids, r.CreatedBy)
		}
	}
	owners := m.cfg.OwnerNames(ids)
	entries := make([]fs.DirEntry, len(rows))
	for i, r := range rows {
		entries[i] = &vfsDirEntry{
			name:  r.Name(),
			dir:   r.IsDir,
			size:  r.Size,
			mtime: r.UpdatedAt,
			owner: owners[r.CreatedBy],
		}
	}
	return entries, nil
}

func (m *TmpMount) ReadFile(rel string) ([]byte, error) {
	st, owner, _ := m.cfg.tmp()
	data, err := st.Read(owner, rel)
	if err != nil {
		return nil, fmt.Errorf("/tmp/%s: %w", rel, err)
	}
	return data, nil
}

func (m *TmpMount) Stat(rel string) (fs.FileInfo, error) {
	if rel == "" {
		return &vfsFileInfo{name: "tmp", dir: true}, nil
	}
	st, owner, _ := m.cfg.tmp()
	f, err := st.Stat(owner, rel)
	if err != nil {
		return nil, fs.ErrNotExist
	}
	return &vfsFileInfo{name: lastSegment(rel), dir: f.IsDir, size: f.Size, mtime: f.UpdatedAt}, nil
}

func (m *TmpMount) WriteFile(rel string, data []byte) error {
	st, owner, lim := m.cfg.tmp()
	created, err := st.Write(owner, lim, m.cfg.UserID, rel, data)
	if err != nil {
		return err
	}
	m.noteFileCreated(created)
	return nil
}

func (m *TmpMount) Remove(rel string) error {
	st, owner, _ := m.cfg.tmp()
	return st.Remove(owner, rel)
}

// --- Optional interfaces ---

func (m *TmpMount) AppendFile(rel string, data []byte) error {
	st, owner, lim := m.cfg.tmp()
	created, err := st.Append(owner, lim, m.cfg.UserID, rel, data)
	if err != nil {
		return err
	}
	m.noteFileCreated(created)
	return nil
}

func (m *TmpMount) Mkdir(rel string) error {
	// No expiry notice here: a directory holds nothing that can be lost,
	// and noteFileCreated counts files only.
	st, owner, _ := m.cfg.tmp()
	return st.Mkdir(owner, m.cfg.UserID, rel)
}

func (m *TmpMount) RemoveAll(rel string) error {
	st, owner, _ := m.cfg.tmp()
	return st.RemoveAll(owner, rel)
}

func (m *TmpMount) CopyFile(srcRel, dstRel string) error {
	st, owner, lim := m.cfg.tmp()
	if err := st.Copy(owner, lim, m.cfg.UserID, srcRel, dstRel); err != nil {
		return err
	}
	// A copy always creates: an existing destination is rejected.
	m.noteFileCreated(true)
	return nil
}

// Rename creates no file, so no expiry notice: the path moved, the retention
// clock it was already on didn't.
func (m *TmpMount) Rename(oldRel, newRel string) error {
	st, owner, _ := m.cfg.tmp()
	return st.Rename(owner, oldRel, newRel)
}

func (m *TmpMount) Touch(rel string) error {
	st, owner, lim := m.cfg.tmp()
	created, err := st.Touch(owner, lim, m.cfg.UserID, rel)
	if err != nil {
		return err
	}
	// Touching an existing file resets its clock but teaches nothing new;
	// only a touch that created a file is a creation.
	m.noteFileCreated(created)
	return nil
}

// noteFileCreated signals the 48h expiry warning on the write that gives
// a user their first /tmp file, and only then. Callers pass whether the
// call actually created a file; overwrites, appends, touches of existing
// files and renames all pass false (or don't call at all).
//
// The gate used to be absent: every mutation signaled, and the notice
// store swallowed the repeats by suppressing per (user, key) forever.
// Migration 071 removed that suppression on purpose — the queue holds
// one row per instance and has no opinion about repeats — so without
// this the user would see the warning on every command touching /tmp.
//
// Both conditions are needed. `created` alone fires again on the second
// and third file; the count alone fires on every write made while the
// user happens to own exactly one file.
//
// Only a build whose /tmp expires has a notice queue to give the Config: the
// notices are the hosted store's, and the open build keeps files for good.
func (m *TmpMount) noteFileCreated(created bool) {
	if !created || m.cfg.NoticeQueue == nil || m.cfg.UserID == "" {
		return
	}
	st, owner, _ := m.cfg.tmp()
	n, err := st.CountByUser(owner, m.cfg.UserID)
	if err != nil || n != 1 {
		return
	}
	m.cfg.NoticeQueue.Maybe(m.cfg.ctx(), NoticeKeyTmpExpiry)
}
