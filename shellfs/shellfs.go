// Package shellfs implements the warp bash.FS interface for weft by
// delegating to the mount-based FS in the mounts package. ShellFS is
// a thin adapter that satisfies bash.FS and exposes extra methods
// (AmendVersionMessage) needed by the coordinator and vim popup.
package shellfs

import (
	"context"
	"io/fs"

	"github.com/graybeardcorp/warp/pkg/bash"
	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/w3ft-net/weft/alerting"
	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/mounts"
)

// Compile-time interface checks.
var _ bash.FS = (*ShellFS)(nil)

// ShellFS implements bash.FS for the weft shell by delegating to a
// mount-composed filesystem.
type ShellFS struct {
	mfs *shellcore.MountFS
	cfg mounts.Config
}

// NoticeSink is the narrow interface ShellFS uses to signal events
// that should produce a one-time notice for the invoking user.
type NoticeSink = mounts.NoticeSink

// NoticeKeyTmpExpiry is the notice key for the 48h /tmp expiry warning.
const NoticeKeyTmpExpiry = mounts.NoticeKeyTmpExpiry

// New creates a ShellFS over the mounts the Config describes, after the options
// have shaped a copy of it.
func New(cfg mounts.Config, opts ...Option) *ShellFS {
	for _, o := range opts {
		o(&cfg)
	}
	return &ShellFS{mfs: mounts.New(cfg), cfg: cfg}
}

// Walk returns the flat subtree under path as mount-relative paths,
// directories suffixed with "/". It backs the server's tree fast-path
// (one call instead of O(directories) ReadDir round-trips).
func (f *ShellFS) Walk(path string) ([]string, error) {
	return mounts.Walk(f.cfg, path)
}

// Option shapes the Config a ShellFS is made over.
type Option func(*mounts.Config)

func configure(f func(*mounts.Config)) Option { return Option(f) }

// WithAccess sets who the mounts act as, and what that principal may read and
// write. It comes first: without it the mounts deny everything. The principal is
// the system identity until WithUser names a member.
func WithAccess(t identity.Access) Option {
	return configure(func(c *mounts.Config) {
		c.PrincipalOf, c.Authz, c.Scope = t.PrincipalOf, t.Authz, t.Scope
		if t.PrincipalOf != nil {
			c.Principal = t.PrincipalOf(c.Owner, "")
		}
	})
}

// WithAlertKinds sets the alert destination kinds /etc/notify.yaml may name
// when it is saved. Without it, only the core's.
func WithAlertKinds(r *alerting.Registry) Option {
	return configure(func(c *mounts.Config) { c.AlertKinds = r })
}

// WithUser makes the mounts act as a member of the org. What they may read
// and write is the adapter's answer for that member on each operation.
// Without it they act as the system identity, which reads everything and
// writes nothing.
func WithUser(userID string) Option {
	return configure(func(c *mounts.Config) {
		c.UserID = userID
		if c.PrincipalOf != nil {
			c.Principal = c.PrincipalOf(c.Owner, userID)
		}
	})
}

// WithPrincipal makes the mounts act as the given principal, such as the cron user
// a cron fires as.
func WithPrincipal(a identity.Principal) Option {
	return configure(func(c *mounts.Config) { c.Principal = a })
}

// WithNoticeQueue wires a notice sink the FS signals on /tmp writes.
func WithNoticeQueue(q NoticeSink) Option {
	return configure(func(c *mounts.Config) { c.NoticeQueue = q })
}

// WithContext binds the request context the mounts run under. warp's
// bash.FS interface is ctx-less, so the per-request ShellFS carries it;
// embedders pass a context.WithoutCancel(requestCtx) so the trace ID
// survives while the FS may outlive the request.
func WithContext(ctx context.Context) Option {
	return configure(func(c *mounts.Config) { c.Ctx = ctx })
}

// MountFS returns the underlying MountFS for callers that need
// direct access (e.g., to call ReadDir or Tree).
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
	return f.mfs.ReadDir(path)
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
	return mounts.AmendVersionMessage(f.cfg, path, message)
}
