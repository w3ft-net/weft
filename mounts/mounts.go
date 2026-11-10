// Package mounts provides weft's virtual filesystem mount implementations.
// Each mount handles one top-level directory (/bin, /cron.d, /etc, /log, /tmp)
// and satisfies shellcore.Mount. The New function composes them into a MountFS.
package mounts

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy"
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
	// Actor is who the mounts act as; Authz decides their writes and Scope
	// what they read. Each is asked on every operation. A Config missing any
	// of them denies everything.
	Actor       tenancy.Actor
	Authz       tenancy.Authorizer
	Scope       tenancy.Scoper
	LogServices []string
	NoticeQueue NoticeSink
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

// allowed reports whether the actor may take act. Any failure denies.
func (c Config) allowed(act tenancy.Action) bool {
	if c.Authz == nil || c.Actor == nil {
		return false
	}
	return c.Authz.Authorize(c.ctx(), c.Actor, act, tenancy.Resource{}) == nil
}

// scope is the namespaces the actor may read, and whether that is every
// namespace in the org. A scope narrower than a namespace is refused: the
// mounts can only enforce namespaces.
func (c Config) scope() (readable []string, all bool, err error) {
	if c.Scope == nil || c.Actor == nil {
		return nil, false, errors.New("mounts: no read scope")
	}
	return tenancy.Readable(c.ctx(), c.Scope, c.Actor, func() ([]string, error) {
		return c.Store.NamespaceNames(c.OrgID, "", true)
	})
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
