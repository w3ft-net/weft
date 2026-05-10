// Package mounts provides weft's virtual filesystem mount implementations.
// Each mount handles one top-level directory (/bin, /cron.d, /etc, /log, /tmp)
// and satisfies shellcore.Mount. The New function composes them into a MountFS.
package mounts

import (
	"github.com/kevlarcon/warp/pkg/shellcore"
	"github.com/kevlarcon/weft/store"
)

// NoticeSink is the narrow interface mounts use to signal one-time notices.
type NoticeSink interface {
	Maybe(noticeKey string)
}

// Config holds the shared state that mount implementations need.
type Config struct {
	Store        *store.Store
	CustomerID   uint
	CustomerName string
	Tier         store.Tier
	UserID       string
	IsOwner      bool
	LogApps      []string
	NoticeQueue  NoticeSink
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
