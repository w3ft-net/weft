// Package shellfs implements the warp bash.FS interface for weft by
// delegating to the mount-based FS in the mounts package. ShellFS is
// a thin adapter that satisfies bash.FS and exposes extra methods
// (AmendVersionMessage) needed by the coordinator and vim popup.
package shellfs

import (
	"context"
	"fmt"
	"io/fs"
	"strings"

	"github.com/graybeardcorp/warp/pkg/bash"
	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/graybeardcorp/weft/mounts"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy"
	"github.com/graybeardcorp/weft/tenancy/hosted"
)

// Compile-time interface checks.
var _ bash.FS = (*ShellFS)(nil)

// ShellFS implements bash.FS for the weft shell by delegating to a
// mount-composed filesystem.
type ShellFS struct {
	mfs    *shellcore.MountFS
	store  *store.Store
	custID store.OrgID
	cfg    mounts.Config
}

// NoticeSink is the narrow interface ShellFS uses to signal events
// that should produce a one-time notice for the invoking user.
type NoticeSink = mounts.NoticeSink

// NoticeKeyTmpExpiry is the notice key for the 48h /tmp expiry warning.
const NoticeKeyTmpExpiry = mounts.NoticeKeyTmpExpiry

// New creates a ShellFS for the given customer.
func New(s *store.Store, orgID store.OrgID, logServices []string, opts ...Option) *ShellFS {
	cfg := mounts.Config{
		Store:       s,
		OrgID:       orgID,
		Tier:        store.TierTrial,
		LogServices: logServices,
		Actor:       hosted.Actor{Org: orgID},
		Authz:       hosted.Authorizer{Store: s},
		Scope:       hosted.Scoper{Store: s},
	}
	for _, o := range opts {
		o(&cfg)
	}
	return &ShellFS{
		mfs:    mounts.New(cfg),
		store:  s,
		custID: orgID,
		cfg:    cfg,
	}
}

// Walk returns the flat subtree under path as mount-relative paths,
// directories suffixed with "/". It backs the server's tree fast-path
// (one call instead of O(directories) ReadDir round-trips).
func (f *ShellFS) Walk(path string) ([]string, error) {
	return mounts.Walk(f.cfg, path)
}

// Option configures the mounts via Config.
type Option func(*mounts.Config)

// WithTier sets the subscription tier for limit enforcement.
func WithTier(tier store.Tier) Option {
	return func(c *mounts.Config) { c.Tier = tier }
}

// WithUser makes the mounts act as a member of the org. What they may read
// and write is the hosted adapter's answer for that member on each operation.
// Without it they act as the system identity, which reads everything and
// writes nothing.
func WithUser(userID string) Option {
	return func(c *mounts.Config) {
		c.UserID = userID
		c.Actor = hosted.Actor{Org: c.OrgID, User: userID}
	}
}

// WithActor makes the mounts act as the given principal, such as the cron user
// a cron fires as.
func WithActor(a tenancy.Actor) Option {
	return func(c *mounts.Config) { c.Actor = a }
}

// WithNoticeQueue wires a notice sink the FS signals on /tmp writes.
func WithNoticeQueue(q NoticeSink) Option {
	return func(c *mounts.Config) { c.NoticeQueue = q }
}

// WithContext binds the request context the mounts run under. warp's
// bash.FS interface is ctx-less, so the per-request ShellFS carries it;
// embedders pass a context.WithoutCancel(requestCtx) so the trace ID
// survives while the FS may outlive the request.
func WithContext(ctx context.Context) Option {
	return func(c *mounts.Config) { c.Ctx = ctx }
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
	dir, name := splitPath(path)
	switch dir {
	case "bin":
		return f.store.AmendShellFunctionMessage(f.custID, f.cfg.UserID, name, message)
	case "cron.d":
		ns, bare, err := f.store.ResolveCronPath(f.custID, name)
		if err != nil {
			return err
		}
		return f.store.AmendCronWatchMessage(f.custID, ns, f.cfg.UserID, bare, message)
	case "etc":
		if name == "notify.yaml" {
			return f.store.AmendNotifyAliasesMessage(f.custID, f.cfg.UserID, message)
		}
		if dash, ok := strings.CutPrefix(name, "dashboards/"); ok && strings.HasSuffix(dash, ".yaml") {
			return f.store.AmendDashboardMessage(f.custID, f.cfg.UserID, strings.TrimSuffix(dash, ".yaml"), message)
		}
		return fmt.Errorf("amend: %s: not a versioned file", path)
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
