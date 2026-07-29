// Package mounts provides weft's virtual filesystem mount implementations.
// Each mount handles one top-level directory (/bin, /cron.d, /etc, /log, /tmp)
// and satisfies shellcore.Mount. The New function composes them into a MountFS.
package mounts

import (
	"fmt"
	"io/fs"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/graybeardcorp/weft/store"
)

// denyWrite is the error mounts return when a non-owner without the
// corresponding OrgPermission flag attempts to mutate a directory.
// Wrapping fs.ErrPermission lets callers test with errors.Is.
func denyWrite(dir string) error {
	return fmt.Errorf("%s: %w (owner only)", dir, fs.ErrPermission)
}

// NoticeSink is the narrow interface mounts use to signal one-time notices.
type NoticeSink interface {
	Maybe(noticeKey string)
}

// Config holds the shared state that mount implementations need.
type Config struct {
	Store              *store.Store
	OrgID              string
	CustomerName       string
	Tier               store.Tier
	UserID             string
	IsOwner            bool
	CanManageFunctions bool // gates writes to /bin
	CanManageWatches   bool // gates writes to /cron.d
	CanManageEtc       bool // gates writes to /etc (/etc/passwd is read-only — managed via adduser/deluser)
	LogServices        []string
	NoticeQueue        NoticeSink
}

// OrgPerms is the org-scoped permission bundle a session carries. The
// values mirror auth.Session's CanManage* flags but the type lives
// here so embedders can build a Config without an auth dependency.
type OrgPerms struct {
	IsOwner            bool
	CanManageFunctions bool
	CanManageWatches   bool
	CanManageEtc       bool
}

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
