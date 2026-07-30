package mounts

import (
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/graybeardcorp/weft/store"
)

type BinMount struct{ cfg Config }

func (m *BinMount) Prefix() string { return "/bin" }

func (m *BinMount) Caps() shellcore.MountCaps {
	return shellcore.MountCaps{Writable: true}
}

func (m *BinMount) ReadDir(rel string) ([]fs.DirEntry, error) {
	if rel != "" {
		return nil, fs.ErrNotExist
	}
	fns, err := m.cfg.Store.ListShellFunctions(m.cfg.OrgID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(fns))
	for _, fn := range fns {
		if fn.CreatedByUserID != nil {
			ids = append(ids, *fn.CreatedByUserID)
		}
	}
	owners := resolveOwners(m.cfg.Store, ids)
	entries := make([]fs.DirEntry, len(fns))
	for i, fn := range fns {
		owner := ""
		if fn.CreatedByUserID != nil {
			owner = owners[*fn.CreatedByUserID]
		}
		entries[i] = &vfsDirEntry{
			name:  fn.Name,
			size:  int64(len(fn.Source)),
			mtime: fn.UpdatedAt,
			owner: owner,
		}
	}
	return entries, nil
}

func (m *BinMount) ReadFile(rel string) ([]byte, error) {
	fn, err := m.cfg.Store.GetShellFunction(m.cfg.OrgID, rel)
	if err != nil {
		return nil, fmt.Errorf("/bin/%s: no such file", rel)
	}
	if fn.Language == "lua" {
		return []byte("#!/usr/bin/env lua\n" + fn.Source), nil
	}
	return []byte(fn.Source), nil
}

func (m *BinMount) Stat(rel string) (fs.FileInfo, error) {
	if rel == "" {
		return &vfsFileInfo{name: "bin", dir: true}, nil
	}
	fn, err := m.cfg.Store.GetShellFunction(m.cfg.OrgID, rel)
	if err != nil {
		return nil, fs.ErrNotExist
	}
	return &vfsFileInfo{name: rel, size: int64(len(fn.Source)), mtime: fn.UpdatedAt}, nil
}

func (m *BinMount) WriteFile(rel string, data []byte) error {
	if !m.cfg.CanManageFunctions {
		return denyWrite("/bin", "binmgr")
	}
	lang, cleanSource := ClassifyBinScript(rel, string(data))
	return m.cfg.Store.SaveShellFunction(m.cfg.OrgID, m.cfg.UserID, rel, cleanSource, lang)
}

func (m *BinMount) Remove(rel string) error {
	if !m.cfg.CanManageFunctions {
		return denyWrite("/bin", "binmgr")
	}
	return m.cfg.Store.DeleteShellFunction(m.cfg.OrgID, rel)
}

// FileLanguage implements shellcore.LangMount.
func (m *BinMount) FileLanguage(rel string) string {
	fn, err := m.cfg.Store.GetShellFunction(m.cfg.OrgID, rel)
	if err != nil {
		return ""
	}
	return fn.Language
}

// AmendVersionMessage allows the vim popup to record a commit message.
func (m *BinMount) AmendVersionMessage(rel, message string) error {
	if !m.cfg.CanManageFunctions {
		return denyWrite("/bin", "binmgr")
	}
	return m.cfg.Store.AmendShellFunctionMessage(m.cfg.OrgID, rel, "HEAD", message)
}

// --- helpers ---

func resolveOwners(s *store.Store, ids []string) map[string]string {
	out := map[string]string{}
	if len(ids) == 0 {
		return out
	}
	uniq := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		uniq = append(uniq, id)
	}
	users, err := s.GetUsersByIDs(uniq)
	if err != nil {
		return out
	}
	for id, u := range users {
		out[id] = ownerDisplayName(u)
	}
	for _, id := range uniq {
		if _, ok := out[id]; !ok {
			out[id] = truncateOwner(id, 12)
		}
	}
	return out
}

func ownerDisplayName(u store.User) string {
	if u.Name != "" {
		return truncateOwner(u.Name, 12)
	}
	if at := strings.IndexByte(u.Email, '@'); at > 0 {
		return truncateOwner(u.Email[:at], 12)
	}
	return truncateOwner(u.ID, 12)
}

func truncateOwner(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// --- vfs types shared across all mounts ---

type vfsFileInfo struct {
	name  string
	dir   bool
	size  int64
	mtime time.Time
	owner string
}

func (fi *vfsFileInfo) Name() string { return fi.name }
func (fi *vfsFileInfo) Size() int64  { return fi.size }
func (fi *vfsFileInfo) Mode() fs.FileMode {
	if fi.dir {
		return fs.ModeDir | 0755
	}
	return 0644
}
func (fi *vfsFileInfo) ModTime() time.Time { return fi.mtime }
func (fi *vfsFileInfo) IsDir() bool        { return fi.dir }
func (fi *vfsFileInfo) Sys() any           { return ownerOnly{fi.owner} }

type ownerOnly struct{ name string }

func (o ownerOnly) Owner() string { return o.name }

type vfsDirEntry struct {
	name  string
	dir   bool
	size  int64
	mtime time.Time
	owner string
}

func (de *vfsDirEntry) Name() string { return de.name }
func (de *vfsDirEntry) IsDir() bool  { return de.dir }
func (de *vfsDirEntry) Type() fs.FileMode {
	if de.dir {
		return fs.ModeDir
	}
	return 0
}
func (de *vfsDirEntry) Info() (fs.FileInfo, error) {
	return &vfsFileInfo{name: de.name, dir: de.dir, size: de.size, mtime: de.mtime, owner: de.owner}, nil
}
