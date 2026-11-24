// Package shellfs implements the warp warpfs.FS interface for weft by
// delegating to the mount-based FS in the mounts package. ShellFS is
// a thin adapter that satisfies warpfs.FS and exposes extra methods
// (AmendVersionMessage) needed by the coordinator and vim popup.
package shellfs

import (
	"context"
	warpfs "github.com/graybeardcorp/warp/pkg/fs"
	"io/fs"

	shellcore "github.com/graybeardcorp/warp/pkg/shell"
	"github.com/w3ft-net/weft/alerting"
	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/mounts"
)

// Compile-time interface checks.
var _ warpfs.FS = (*ShellFS)(nil)

// ShellFS implements warpfs.FS for the weft shell by delegating to a
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
func (f *ShellFS) Walk(ctx context.Context, path string) ([]string, error) {
	return mounts.Walk(ctx, f.cfg, path)
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
// warpfs.FS interface is ctx-less, so the per-request ShellFS carries it;
// embedders pass a context.WithoutCancel(requestCtx) so the trace ID
// survives while the FS may outlive the request.
func WithContext(ctx context.Context) Option {
	return configure(func(c *mounts.Config) { c.Ctx = ctx })
}

// MountFS returns the underlying MountFS for callers that need
// direct access (e.g., to call ReadDir or Tree).
func (f *ShellFS) MountFS() *shellcore.MountFS { return f.mfs }

// --- warpfs.FS implementation ---

func (f *ShellFS) ReadFile(ctx context.Context, path string) ([]byte, error) {
	return f.mfs.ReadFile(ctx, path)
}

func (f *ShellFS) WriteFile(ctx context.Context, path string, data []byte) error {
	return f.mfs.WriteFile(ctx, path, data)
}

func (f *ShellFS) Stat(ctx context.Context, path string) (fs.FileInfo, error) {
	return f.mfs.Stat(ctx, path)
}

func (f *ShellFS) ReadDir(ctx context.Context, path string) ([]fs.DirEntry, error) {
	return f.mfs.ReadDir(ctx, path)
}

func (f *ShellFS) Remove(ctx context.Context, path string) error {
	return f.mfs.Remove(ctx, path)
}

// --- Optional warpfs.FS interfaces ---

func (f *ShellFS) AppendFile(ctx context.Context, path string, data []byte) error {
	return f.mfs.AppendFile(ctx, path, data)
}

func (f *ShellFS) Mkdir(ctx context.Context, path string) error {
	return f.mfs.Mkdir(ctx, path)
}

func (f *ShellFS) RemoveAll(ctx context.Context, path string) error {
	return f.mfs.RemoveAll(ctx, path)
}

func (f *ShellFS) CopyFile(ctx context.Context, src, dst string) error {
	return f.mfs.CopyFile(ctx, src, dst)
}

func (f *ShellFS) Rename(ctx context.Context, oldPath, newPath string) error {
	return f.mfs.Rename(ctx, oldPath, newPath)
}

func (f *ShellFS) Touch(ctx context.Context, path string) error {
	return f.mfs.Touch(ctx, path)
}

func (f *ShellFS) Glob(ctx context.Context, pattern string) ([]string, error) {
	return f.mfs.Glob(ctx, pattern)
}

func (f *ShellFS) FileLanguage(ctx context.Context, path string) string {
	return f.mfs.FileLanguage(ctx, path)
}

// --- Extra methods not in warpfs.FS ---

// AmendVersionMessage records a commit message for versioned paths.
func (f *ShellFS) AmendVersionMessage(path, message string) error {
	return mounts.AmendVersionMessage(f.cfg, path, message)
}
