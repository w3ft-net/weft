// Package mounts provides weft's virtual filesystem mount implementations.
// Each mount handles one top-level directory (/bin, /cron.d, /etc, /log, /tmp)
// and satisfies shellcore.Mount. The New function composes them into a MountFS.
package mounts

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/w3ft-net/weft/cronauthor"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/w3ft-net/weft/alerting"
	"github.com/w3ft-net/weft/backends/history"
	"github.com/w3ft-net/weft/backends/source"
	"github.com/w3ft-net/weft/backends/tmp"
	"github.com/w3ft-net/weft/binscript"
	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/notifycfg"
	"github.com/w3ft-net/weft/shape"
)

// denyWrite is the error mounts return when a caller without the
// corresponding OrgPermission flag attempts to mutate a directory. The
// message names the group to ask for, not a role: since migration 072
// the flag is the only authority, so "owner only" would have pointed
// the user at the wrong remedy.
// Wrapping fs.ErrPermission lets callers test with errors.Is.
func denyWrite(dir, group string) error {
	return fmt.Errorf("%s: %w (need the %s group — ask an owner for `usermod -aG %s <you>`)",
		dir, fs.ErrPermission, group, group)
}

// denyNamespaceWrite is the refusal for a write to a namespace's crons.
func denyNamespaceWrite(dir, ns string) error {
	return fmt.Errorf("%s: %w (need the %s-w group — ask an owner for `usermod -aG %s-w <you>`)",
		dir, fs.ErrPermission, ns, ns)
}

// NoticeSink is the narrow interface mounts use to signal one-time notices.
type NoticeSink interface {
	Maybe(ctx context.Context, noticeKey string)
}

// Config holds the shared state that mount implementations need: who the
// mounts act for, and the services they read and write through. A Config the
// caller built from one build's stores is that build's filesystem; the mounts
// themselves know of no store but the contracts.
type Config struct {
	// Owner is the added columns of the group the mounts act in: what PrincipalOf
	// names a principal by.
	Owner  shape.Added
	UserID string
	// Principal is who the mounts act as; Authz decides their writes and Scope
	// what they read. Each is asked on every operation. A Config missing any
	// of them denies everything.
	Principal identity.Principal
	// PrincipalOf names the principal for a user of the org, so a Config can be
	// re-bound to one (shellfs.WithUser).
	PrincipalOf func(owner shape.Added, user string) identity.Principal
	Authz       identity.Authorizer
	Scope       identity.Scoper
	LogServices []string

	// The services the mounts go through, each over the build's own stores. A
	// mount that needs one it was not given fails when it is used.
	//
	// Sources holds the namespaces; BinService judges and stores /bin scripts,
	// Notify the notification config, CronAuthor crons (and holds a group to its
	// plan limits); Tmp is where /tmp lives, and holds its own writes to limits.
	Sources    source.Store
	BinService binscript.Service
	Notify     notifycfg.Service
	CronAuthor cronauthor.Service
	Tmp        tmp.Store

	NoticeQueue NoticeSink
	// History is where writes to tracked files are recorded; nil records nothing
	// (the hosted build, whose history is its version rows). Identity names a
	// user for attribution there; nil is the user id.
	History  history.Store
	Identity func(userID string) history.Identity
	// EtcExtensions adds files to /etc a build serves itself (see the type).
	// UserNames says how to show a user by id; nil shows the id. CronRenamed is
	// called after an owner-level cron is moved, with its old name and its new
	// path, and may answer with a warning to show (hosted: dashboards that still
	// name it); nil says nothing.
	EtcExtensions EtcExtensions
	UserNames     func(ids []string) map[string]string
	CronRenamed   func(c Config, name, newRel string) error
	// AlertKinds are the alert destination kinds /etc/notify.yaml may name,
	// checked when it is saved. Nil is the core's kinds alone.
	AlertKinds *alerting.Registry
	// Ctx is the request context the mounts run under. warp's Mount
	// interface is ctx-less, so the per-request Config carries it
	// (detached from cancellation by the embedder) to keep the trace
	// ID on store calls, logs and notice emission. Nil means no request
	// (tests, boot) and falls back to context.Background().
	Ctx context.Context
}

// Backends is what a deployment's mounts read and write through, built once
// when the process starts. Every call a mount makes names the owner, so one
// Backends serves every group; what differs between groups (their limits, their
// org) is the backends' own business. A mount that needs a backend it was not
// given fails when it is used.
type Backends struct {
	// Sources holds the namespaces; BinService judges and stores /bin scripts,
	// Notify the notification config, CronAuthor crons (and holds a group to its
	// limits); Tmp is where /tmp lives and holds its own writes to limits.
	Sources    source.Store
	BinService binscript.Service
	Notify     notifycfg.Service
	CronAuthor cronauthor.Service
	Tmp        tmp.Store
	// History is where writes to tracked files are recorded; nil records nothing
	// (the hosted build, whose history is its version rows). Identity names a
	// user for attribution there; nil is the user id.
	History  history.Store
	Identity func(userID string) history.Identity
	// EtcExtensions, UserNames and CronRenamed are the Config fields of the same
	// names.
	EtcExtensions EtcExtensions
	UserNames     func(ids []string) map[string]string
	CronRenamed   func(c Config, name, newRel string) error
}

// Session is what one request's filesystem differs by: the group it acts in and
// the services whose logs it reads.
type Session struct {
	Owner       shape.Added
	LogServices []string
}

// Config is the Config of a filesystem over these backends for the session.
// Who acts is still to be set: the Config denies everything until it has a
// Principal, an Authz and a Scope.
func (b Backends) Config(s Session) Config {
	return Config{
		Owner:         s.Owner,
		LogServices:   s.LogServices,
		Sources:       b.Sources,
		BinService:    b.BinService,
		Notify:        b.Notify,
		CronAuthor:    b.CronAuthor,
		Tmp:           b.Tmp,
		History:       b.History,
		Identity:      b.Identity,
		EtcExtensions: b.EtcExtensions,
		UserNames:     b.UserNames,
		CronRenamed:   b.CronRenamed,
	}
}

// ctx returns the request context, or Background when none was bound.
func (c Config) ctx() context.Context {
	if c.Ctx == nil {
		return context.Background()
	}
	return c.Ctx
}

// allowed reports whether the principal may take act. Any failure denies.
func (c Config) allowed(act identity.Action) bool {
	return c.allowedIn(act, "")
}

// allowedIn is allowed for an action on one namespace's resources.
func (c Config) allowedIn(act identity.Action, namespace string) bool {
	if c.Authz == nil || c.Principal == nil {
		return false
	}
	return c.Authz.Authorize(c.ctx(), c.Principal, act, identity.Resource{Namespace: namespace}) == nil
}

// scope is the namespaces the principal may read, and whether that is every
// namespace in the org. A scope narrower than a namespace is refused: the
// mounts can only enforce namespaces.
func (c Config) scope() (readable []string, all bool, err error) {
	if c.Scope == nil || c.Principal == nil {
		return nil, false, errors.New("mounts: no read scope")
	}
	return identity.Readable(c.ctx(), c.Scope, c.Principal, func() ([]string, error) {
		return c.namespaceNames()
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
	if cfg.EtcExtensions != nil {
		for _, m := range cfg.EtcExtensions(cfg) {
			mfs.Register(m)
		}
	}
	mfs.Register(&BinMount{cfg: cfg})
	mfs.Register(&CronMount{cfg: cfg})
	mfs.Register(&EtcMount{cfg: cfg})
	mfs.Register(&LogMount{cfg: cfg})
	mfs.Register(&TmpMount{cfg: cfg})
	return mfs
}

// partition is the added columns of the group the mounts act in; a Config with
// no principal names none (and the Config denies everything anyway).
func (c Config) partition() shape.Added {
	if c.Principal == nil {
		return shape.Added{}
	}
	return c.Principal.Partition()
}

func lastSegment(p string) string {
	if idx := strings.LastIndex(p, "/"); idx >= 0 {
		return p[idx+1:]
	}
	return p
}

// namespaceNames is every namespace the group has.
func (c Config) namespaceNames() ([]string, error) {
	nss, err := c.Sources.ListNamespaces(c.partition())
	if err != nil {
		return nil, err
	}
	names := make([]string, len(nss))
	for i, n := range nss {
		names[i] = n.Name
	}
	return names, nil
}

// namespaceName is the name of the namespace with this id.
func (c Config) namespaceName(id uint) (string, error) {
	name, ok, err := c.Sources.NamespaceName(c.partition(), id)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("no namespace with id %d", id)
	}
	return name, nil
}
