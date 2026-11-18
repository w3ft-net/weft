package mounts

import (
	"context"
	"errors"
	"fmt"
	"github.com/w3ft-net/weft/notifycfg"
	"io/fs"
	"strings"
	"time"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/w3ft-net/weft/backends/bin"
	"github.com/w3ft-net/weft/binscript"
)

type BinMount struct{ cfg Config }

// bin is the /bin service the Config was given.
func (c Config) bin() binscript.Service { return c.BinService }

func (m *BinMount) Prefix() string { return "/bin" }

func (m *BinMount) Caps() shellcore.MountCaps {
	return shellcore.MountCaps{Writable: true}
}

// Authorize is the mount's one access check, called by MountFS before every
// operation: every member can read and run /bin. A script no cron runs can be
// changed by any member; one that crons run needs write on each of their
// namespaces, since changing it changes what they do.
func (m *BinMount) Authorize(ctx context.Context, rel string, want shellcore.Perm) error {
	if want&shellcore.PermWrite != 0 {
		return m.cfg.mayWriteBin(rel)
	}
	return nil
}

func (m *BinMount) ReadDir(ctx context.Context, rel string) ([]fs.DirEntry, error) {
	if rel != "" {
		return nil, fs.ErrNotExist
	}
	fns, err := m.cfg.bin().Bins.List(m.cfg.partition())
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(fns))
	for _, fn := range fns {
		if fn.CreatedBy != "" {
			ids = append(ids, fn.CreatedBy)
		}
	}
	owners := m.cfg.OwnerNames(ids)
	entries := make([]fs.DirEntry, len(fns))
	for i, fn := range fns {
		owner := ""
		if fn.CreatedBy != "" {
			owner = owners[fn.CreatedBy]
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

func (m *BinMount) ReadFile(ctx context.Context, rel string) ([]byte, error) {
	fn, err := m.cfg.bin().Bins.Get(m.cfg.partition(), rel)
	if err != nil {
		return nil, fmt.Errorf("/bin/%s: no such file", rel)
	}
	if fn.Language == bin.LangLua {
		return []byte("#!/usr/bin/env lua\n" + fn.Source), nil
	}
	return []byte(fn.Source), nil
}

func (m *BinMount) Stat(ctx context.Context, rel string) (fs.FileInfo, error) {
	if rel == "" {
		return &vfsFileInfo{name: "bin", dir: true}, nil
	}
	fn, err := m.cfg.bin().Bins.Get(m.cfg.partition(), rel)
	if err != nil {
		return nil, fs.ErrNotExist
	}
	return &vfsFileInfo{name: rel, size: int64(len(fn.Source)), mtime: fn.UpdatedAt}, nil
}

func (m *BinMount) writeFile(rel string, data []byte) error {
	lang, cleanSource := ClassifyBinScript(rel, string(data))
	if err := m.cfg.authorizeBinEdit(rel, cleanSource, lang); err != nil {
		return err
	}
	return m.cfg.bin().Save(m.cfg.partition(), m.cfg.UserID, rel, cleanSource, lang)
}

func (m *BinMount) remove(rel string) error {
	err := m.cfg.bin().Delete(m.cfg.partition(), rel)
	var inUse *binscript.InUseError
	if errors.As(err, &inUse) {
		paths, perr := m.cfg.cronPaths(inUse.Crons)
		if perr != nil {
			return perr
		}
		return fmt.Errorf("/bin/%s: %w (crons run it: %s)", rel, fs.ErrPermission, strings.Join(paths, ", "))
	}
	return err
}

// FileLanguage implements shellcore.LangMount.
func (m *BinMount) FileLanguage(ctx context.Context, rel string) string {
	fn, err := m.cfg.bin().Bins.Get(m.cfg.partition(), rel)
	if err != nil {
		return ""
	}
	return fn.Language
}

// AmendVersionMessage allows the vim popup to record a commit message. It is
// not a MountFS operation, so it checks write access itself.
func (m *BinMount) AmendVersionMessage(rel, message string) error {
	if err := m.cfg.mayWriteBin(rel); err != nil {
		return err
	}
	if err := m.cfg.bin().Bins.AmendMessage(m.cfg.partition(), m.cfg.UserID, rel, message); err != nil {
		return err
	}
	return m.cfg.RewordHistory("bin/"+rel, message)
}

// --- helpers ---

// shortIDs shows each distinct user id as itself, cut to fit a listing: how a
// build with no user directory names the people who made things.
func shortIDs(ids []string) map[string]string {
	out := map[string]string{}
	for _, id := range ids {
		if id != "" {
			out[id] = truncateOwner(id, 12)
		}
	}
	return out
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

// AmendBinMessage sets the commit message on the current version of /bin/<name>,
// only if authorID wrote it (empty: whoever did). It does not check that the
// caller may write: callers that act for a user go through the mount.
func (c Config) AmendBinMessage(name, authorID, message string) error {
	if err := c.bin().Bins.AmendMessage(c.partition(), authorID, name, message); err != nil {
		return err
	}
	return c.RewordHistory("bin/"+name, message)
}

func (m *BinMount) WriteFile(ctx context.Context, rel string, data []byte) error {
	err := m.writeFile(rel, data)
	m.cfg.journalWrite("bin/"+rel, err, func() ([]byte, error) { return m.ReadFile(ctx, rel) })
	return err
}

func (m *BinMount) Remove(ctx context.Context, rel string) error {
	err := m.remove(rel)
	m.cfg.journalDelete("bin/"+rel, err)
	return err
}

// notify is the notification config service the Config was given.
func (c Config) notify() notifycfg.Service { return c.Notify }
