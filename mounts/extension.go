package mounts

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"time"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/shape"
)

// EtcExtensions is how a deployment adds files to /etc that not every build has
// (hosted: the dashboards, the team's passwd, the usage report). It is asked for
// each request's mounts, and each mount it returns serves /etc/<name>: its Prefix
// is "/etc/" and the name, and a MountFS prefers the longest prefix, so it takes
// that path and everything under it. The listing of /etc shows each by its
// Stat("").
type EtcExtensions func(Config) []shellcore.Mount

// What an extension needs of the request it serves. The Config is the
// request's: its group, its user, what the principal may do and read.

// Allowed reports whether the principal may take act.
func (c Config) Allowed(act identity.Action) bool { return c.allowed(act) }

// AllowedIn is Allowed for an action on one namespace's resources.
func (c Config) AllowedIn(act identity.Action, namespace string) bool {
	return c.allowedIn(act, namespace)
}

// Readable is the namespaces the principal may read, and whether that is every
// namespace there is.
func (c Config) Readable() (namespaces []string, all bool, err error) { return c.scope() }

// Partition is the group the Config acts in.
func (c Config) Partition() shape.Added { return c.partition() }

// Context is the request's context.
func (c Config) Context() context.Context { return c.ctx() }

// JournalWrite records the file at path in the history after the write that
// returned err, if it happened; a Config without a history does nothing.
func (c Config) JournalWrite(path string, err error, read func() ([]byte, error)) {
	c.journalWrite(path, err, read)
}

// JournalDelete records the removal of path after the removal that returned err.
func (c Config) JournalDelete(path string, err error) { c.journalDelete(path, err) }

// OwnerNames is how to show the users with these ids: by display name where the
// Config has a directory of them (UserNames), by the id otherwise.
func (c Config) OwnerNames(ids []string) map[string]string {
	if c.UserNames != nil {
		return c.UserNames(ids)
	}
	return shortIDs(ids)
}

// DenyWrite is the refusal for a write the principal's groups don't allow.
func DenyWrite(dir, group string) error { return denyWrite(dir, group) }

// NewFileInfo is the FileInfo of a file or directory a mount serves, owner the
// name shown for who made it.
func NewFileInfo(name string, dir bool, size int64, mtime time.Time, owner string) fs.FileInfo {
	return &vfsFileInfo{name: name, dir: dir, size: size, mtime: mtime, owner: owner}
}

// NewDirEntry is the entry of a mount's listing for a file or directory.
func NewDirEntry(name string, dir bool, size int64, mtime time.Time, owner string) fs.DirEntry {
	return &vfsDirEntry{name: name, dir: dir, size: size, mtime: mtime, owner: owner}
}

// AmendVersionMessage records a commit message for the versioned file at p, by
// asking the mount that serves it, which checks that the principal may write there
// and that the version is theirs. A path that no mount versions is an error.
func AmendVersionMessage(cfg Config, p, message string) error {
	p = path.Clean("/" + p)
	candidates := []shellcore.Mount{&BinMount{cfg}, &CronMount{cfg}, &EtcMount{cfg}}
	if cfg.EtcExtensions != nil {
		candidates = append(candidates, cfg.EtcExtensions(cfg)...)
	}
	var (
		best shellcore.Mount
		rel  string
	)
	for _, m := range candidates {
		prefix := m.Prefix()
		var r string
		switch {
		case p == prefix:
		case strings.HasPrefix(p, prefix+"/"):
			r = strings.TrimPrefix(p, prefix+"/")
		default:
			continue
		}
		if best == nil || len(prefix) > len(best.Prefix()) {
			best, rel = m, r
		}
	}
	am, ok := best.(interface {
		AmendVersionMessage(rel, message string) error
	})
	if !ok {
		return fmt.Errorf("amend: %s: not a versioned file", p)
	}
	return am.AmendVersionMessage(rel, message)
}
