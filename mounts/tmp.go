package mounts

import (
	"fmt"
	"io/fs"

	"github.com/kevlarcon/warp/pkg/shellcore"
	"github.com/kevlarcon/weft/store"
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
	data, err := m.cfg.Store.ReadTmpFile(m.cfg.CustomerID, path)
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
	return m.cfg.Store.DeleteTmpFile(m.cfg.CustomerID, path)
}

// --- Optional interfaces ---

func (m *TmpMount) AppendFile(rel string, data []byte) error {
	path := "/tmp/" + rel
	if err := m.cfg.Store.AppendTmpFile(m.cfg.CustomerID, m.cfg.Tier, m.cfg.UserID, path, data); err != nil {
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
	return m.cfg.Store.DeleteTmpSubtree(m.cfg.CustomerID, path)
}

func (m *TmpMount) CopyFile(srcRel, dstRel string) error {
	src := "/tmp/" + srcRel
	dst := "/tmp/" + dstRel
	if err := m.cfg.Store.CopyTmpFile(m.cfg.CustomerID, m.cfg.Tier, m.cfg.UserID, src, dst); err != nil {
		return err
	}
	m.noteFirstWrite()
	return nil
}

func (m *TmpMount) Rename(oldRel, newRel string) error {
	oldPath := "/tmp/" + oldRel
	newPath := "/tmp/" + newRel
	if err := m.cfg.Store.RenameTmpFile(m.cfg.CustomerID, oldPath, newPath); err != nil {
		return err
	}
	m.noteFirstWrite()
	return nil
}

func (m *TmpMount) Touch(rel string) error {
	path := "/tmp/" + rel
	if err := m.cfg.Store.TouchTmpFile(m.cfg.CustomerID, m.cfg.Tier, m.cfg.UserID, path); err != nil {
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
