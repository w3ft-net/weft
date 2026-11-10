// Package mounts provides weft's virtual filesystem mount implementations.
// Each mount handles one top-level directory (/bin, /cron.d, /etc, /log, /tmp)
// and satisfies shellcore.Mount. The New function composes them into a MountFS.
package mounts

import (
	"context"
	"fmt"
	"io/fs"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/graybeardcorp/weft/store"
)

// denyWrite is the error mounts return when a caller without the
// corresponding OrgPermission flag attempts to mutate a directory. The
// message names the group to ask for, not a role: since migration 072
// the flag is the only authority, so "owner only" would have pointed
// the user at the wrong remedy.
// Wrapping fs.ErrPermission lets callers test with errors.Is.
func denyWrite(dir, group string) error {
	return fmt.Errorf("%s: %w (need the %s group — ask an owner for `adduser <you> %s`)",
		dir, fs.ErrPermission, group, group)
}

// PlanLimitError is a write refused because the org's plan does not allow it
// (too many crons, a schedule shorter than the plan's minimum). It is not a
// permission problem — the caller may write here, the plan may not — so
// callers that map errors to statuses can tell the two apart.
type PlanLimitError struct{ Msg string }

func (e *PlanLimitError) Error() string { return e.Msg }

// NoticeSink is the narrow interface mounts use to signal one-time notices.
type NoticeSink interface {
	Maybe(ctx context.Context, noticeKey string)
}

// Config holds the shared state that mount implementations need.
type Config struct {
	Store  *store.Store
	OrgID  store.OrgID
	Tier   store.Tier
	UserID string
	// IsOwner gates namespace *read* visibility in LogMount. It is not
	// an authority over the four org-scoped bits below — those come from
	// the member's org_permissions row and nowhere else (migration 072),
	// and an owner holds them because their promotion materialized them.
	IsOwner            bool
	CanManageFunctions bool // gates writes to /bin
	CanManageWatches   bool // gates writes to /cron.d
	CanManageEtc       bool // gates writes to /etc (/etc/passwd is read-only — managed via adduser/deluser)
	// CanCreateNamespaces gates namespace creation in the active org.
	CanCreateNamespaces bool
	LogServices         []string
	NoticeQueue         NoticeSink
	// Ctx is the request context the mounts run under. warp's Mount
	// interface is ctx-less, so the per-request Config carries it
	// (detached from cancellation by the embedder) to keep the trace
	// ID on store calls, logs and notice emission. Nil means no request
	// (tests, boot) and falls back to context.Background().
	Ctx context.Context
}

// ctx returns the request context, or Background when none was bound.
func (c Config) ctx() context.Context {
	if c.Ctx == nil {
		return context.Background()
	}
	return c.Ctx
}

// OrgPerms is the org-scoped permission bundle a session carries. The
// values mirror auth.Session's CanManage* flags but the type lives
// here so embedders can build a Config without an auth dependency.
type OrgPerms struct {
	IsOwner             bool
	CanManageFunctions  bool
	CanManageWatches    bool
	CanManageEtc        bool
	CanCreateNamespaces bool
}

// Every mount gates access through Authorize. MountFS probes for it at
// runtime, so a signature that drifts would silently leave the mount open.
var (
	_ shellcore.AuthMount = (*BinMount)(nil)
	_ shellcore.AuthMount = (*CronMount)(nil)
	_ shellcore.AuthMount = (*EtcMount)(nil)
	_ shellcore.AuthMount = (*LogMount)(nil)
)

// New constructs a MountFS with all weft virtual directories registered.
func New(cfg Config) *shellcore.MountFS {
	mfs := shellcore.NewMountFS()
	mfs.Register(&BinMount{cfg: cfg})
	mfs.Register(&CronMount{cfg: cfg})
	mfs.Register(&EtcMount{cfg: cfg})
	mfs.Register(&LogMount{cfg: cfg})
	mfs.Register(&TmpMount{cfg: cfg})
	return mfs
}
