// Package shellfs implements the warp bash.FS interface for weft by
// delegating to the mount-based FS in the mounts package. ShellFS is
// a thin adapter that satisfies bash.FS and exposes extra methods
// (AmendVersionMessage) needed by the coordinator and vim popup.
package shellfs

import (
	"fmt"
	"io/fs"
	"strings"

	"github.com/kevlarcon/warp/pkg/bash"
	"github.com/kevlarcon/warp/pkg/shellcore"
	"github.com/kevlarcon/weft/mounts"
	"github.com/kevlarcon/weft/store"
)

// Compile-time interface checks.
var _ bash.FS = (*ShellFS)(nil)

// ShellFS implements bash.FS for the weft shell by delegating to a
// mount-composed filesystem.
type ShellFS struct {
	mfs   *shellcore.MountFS
	store *store.Store
	custID uint
}

// NoticeSink is the narrow interface ShellFS uses to signal events
// that should produce a one-time notice for the invoking user.
type NoticeSink = mounts.NoticeSink

// NoticeKeyTmpExpiry is the notice key for the 48h /tmp expiry warning.
const NoticeKeyTmpExpiry = mounts.NoticeKeyTmpExpiry

// New creates a ShellFS for the given customer.
func New(s *store.Store, customerID uint, logApps []string, opts ...Option) *ShellFS {
	cfg := mounts.Config{
		Store:      s,
		CustomerID: customerID,
		Tier:       store.TierTrial,
		LogApps:    logApps,
	}
	for _, o := range opts {
		o(&cfg)
	}
	return &ShellFS{
		mfs:    mounts.New(cfg),
		store:  s,
		custID: customerID,
	}
}

// Option configures the mounts via Config.
type Option func(*mounts.Config)

// WithTier sets the subscription tier for limit enforcement.
func WithTier(tier store.Tier) Option {
	return func(c *mounts.Config) { c.Tier = tier }
}

// WithCustomerName sets the customer name for usage queries.
func WithCustomerName(name string) Option {
	return func(c *mounts.Config) { c.CustomerName = name }
}

// WithUser sets the user ID for permission checks.
func WithUser(userID string, isOwner bool) Option {
	return func(c *mounts.Config) {
		c.UserID = userID
		c.IsOwner = isOwner
	}
}

// WithNoticeQueue wires a notice sink the FS signals on /tmp writes.
func WithNoticeQueue(q NoticeSink) Option {
	return func(c *mounts.Config) { c.NoticeQueue = q }
}

// MountFS returns the underlying MountFS for callers that need
// direct access (e.g., to call ClassifyPath or ReadDir as []string).
func (f *ShellFS) MountFS() *shellcore.MountFS { return f.mfs }

// --- bash.FS implementation ---

func (f *ShellFS) ReadFile(path string) ([]byte, error) {
	return f.mfs.ReadFile(path)
}

func (f *ShellFS) WriteFile(path string, data []byte) error {
	return f.mfs.WriteFile(path, data)
}

func (f *ShellFS) Stat(path string) (fs.FileInfo, error) {
	return f.mfs.Stat(path)
}

func (f *ShellFS) ReadDir(path string) ([]fs.DirEntry, error) {
	return f.mfs.ReadDirEntries(path)
}

func (f *ShellFS) Remove(path string) error {
	return f.mfs.Remove(path)
}

// --- Optional bash.FS interfaces ---

func (f *ShellFS) AppendFile(path string, data []byte) error {
	return f.mfs.AppendFile(path, data)
}

func (f *ShellFS) Mkdir(path string) error {
	return f.mfs.Mkdir(path)
}

func (f *ShellFS) RemoveAll(path string) error {
	return f.mfs.RemoveAll(path)
}

func (f *ShellFS) CopyFile(src, dst string) error {
	return f.mfs.CopyFile(src, dst)
}

func (f *ShellFS) Rename(oldPath, newPath string) error {
	return f.mfs.Rename(oldPath, newPath)
}

func (f *ShellFS) Touch(path string) error {
	return f.mfs.Touch(path)
}

func (f *ShellFS) Glob(pattern string) ([]string, error) {
	return f.mfs.Glob(pattern)
}

func (f *ShellFS) FileLanguage(path string) string {
	return f.mfs.FileLanguage(path)
}

// --- Extra methods not in bash.FS ---

// AmendVersionMessage records a commit message for versioned paths.
func (f *ShellFS) AmendVersionMessage(path, message string) error {
	dir, name := splitPath(path)
	switch dir {
	case "bin":
		return f.store.AmendShellFunctionMessage(f.custID, name, "HEAD", message)
	case "cron.d":
		return f.store.AmendCronWatchMessage(f.custID, name, "HEAD", message)
	default:
		return fmt.Errorf("amend: %s: not a versioned namespace", path)
	}
}

// splitPath splits "/bin/check_errors" into ("bin", "check_errors").
func splitPath(path string) (dir, name string) {
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, "/")
	if path == "" {
		return "", ""
	}
	if idx := strings.Index(path, "/"); idx >= 0 {
		return path[:idx], path[idx+1:]
	}
	return path, ""
}
