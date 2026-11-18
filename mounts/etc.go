package mounts

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/w3ft-net/weft/alerting"
	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/notifycfg"
)

// EtcMount is /etc: the notification config every build has, and the files a
// deployment adds through Config.EtcExtensions, which are mounts of their own.
type EtcMount struct{ cfg Config }

func (m *EtcMount) Prefix() string { return "/etc" }

func (m *EtcMount) Caps() shellcore.MountCaps {
	return shellcore.MountCaps{Writable: true}
}

// Authorize is the mount's one access check, called by MountFS before every
// operation: every member can read /etc, and only the configmgr group can change
// it. An extension's files check for themselves.
func (m *EtcMount) Authorize(ctx context.Context, rel string, want shellcore.Perm) error {
	if want&shellcore.PermWrite == 0 {
		return nil
	}
	if !m.cfg.allowed(identity.ActionManageEtc) {
		return denyWrite("/etc", "configmgr")
	}
	return nil
}

// notifyInfo is notify.yaml as a listing shows it.
func (m *EtcMount) notifyInfo() fs.FileInfo {
	if doc, err := m.cfg.notify().Aliases.Aliases(m.cfg.partition()); err == nil {
		return &vfsFileInfo{name: "notify.yaml", size: int64(len(doc.Text)), mtime: doc.UpdatedAt}
	}
	return &vfsFileInfo{name: "notify.yaml"}
}

func (m *EtcMount) ReadDir(ctx context.Context, rel string) ([]fs.DirEntry, error) {
	if rel != "" {
		return nil, fs.ErrNotExist
	}
	info := m.notifyInfo()
	entries := []fs.DirEntry{&vfsDirEntry{name: "notify.yaml", size: info.Size(), mtime: info.ModTime()}}
	if m.cfg.EtcExtensions != nil {
		for _, ext := range m.cfg.EtcExtensions(m.cfg) {
			fi, err := ext.Stat(ctx, "")
			if err != nil {
				continue // an extension with nothing to show is not listed
			}
			entries = append(entries, &vfsDirEntry{name: path.Base(ext.Prefix()), dir: fi.IsDir(), size: fi.Size(), mtime: fi.ModTime()})
		}
	}
	// A listing is by name, wherever its entries came from.
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && entries[j].Name() < entries[j-1].Name(); j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
	return entries, nil
}

func (m *EtcMount) ReadFile(ctx context.Context, rel string) ([]byte, error) {
	if rel == "notify.yaml" {
		doc, err := m.cfg.notify().Aliases.Aliases(m.cfg.partition())
		if err != nil {
			return []byte(""), nil
		}
		// Webhook header values are secrets by reference (${secrets.NAME}), so the
		// document holds none.
		return []byte(doc.Text), nil
	}
	return nil, fmt.Errorf("/etc/%s: no such file", rel)
}

func (m *EtcMount) Stat(ctx context.Context, rel string) (fs.FileInfo, error) {
	switch rel {
	case "":
		return &vfsFileInfo{name: "etc", dir: true}, nil
	case "notify.yaml":
		return m.notifyInfo(), nil
	}
	return nil, fs.ErrNotExist
}

func (m *EtcMount) writeFile(rel string, data []byte) error {
	if rel != "notify.yaml" {
		return fmt.Errorf("/etc/%s: read-only", rel)
	}
	content := string(data)
	if err := notifycfg.Validate(content); err != nil {
		return fmt.Errorf("/etc/notify.yaml: %w", err)
	}
	aliases, _ := notifycfg.Parse(content) // parsed by the validation above
	kinds := m.cfg.AlertKinds
	if kinds == nil {
		kinds = alerting.NewRegistry(nil)
	}
	if err := kinds.Validate(aliases); err != nil {
		return fmt.Errorf("/etc/notify.yaml: %w", err)
	}
	return m.cfg.notify().Save(m.cfg.partition(), m.cfg.UserID, content)
}

func (m *EtcMount) remove(rel string) error {
	return fmt.Errorf("/etc/%s: operation not permitted", rel)
}

// AmendVersionMessage records a commit message for notify.yaml, the one
// versioned file here. It is not a MountFS operation, so it checks write access
// itself.
func (m *EtcMount) AmendVersionMessage(rel, message string) error {
	if err := m.Authorize(m.cfg.ctx(), rel, shellcore.PermWrite); err != nil {
		return err
	}
	if rel != "notify.yaml" {
		return fmt.Errorf("/etc/%s: not a versioned file", rel)
	}
	if err := m.cfg.notify().Aliases.AmendMessage(m.cfg.partition(), m.cfg.UserID, message); err != nil {
		return err
	}
	return m.cfg.RewordHistory("etc/notify.yaml", message)
}

// FileLanguage implements shellcore.LangMount: notify.yaml highlights as YAML.
func (m *EtcMount) FileLanguage(ctx context.Context, rel string) string {
	if strings.HasSuffix(rel, ".yaml") {
		return "yaml"
	}
	return ""
}

func (m *EtcMount) WriteFile(ctx context.Context, rel string, data []byte) error {
	err := m.writeFile(rel, data)
	if rel == "notify.yaml" {
		m.cfg.journalWrite("etc/notify.yaml", err, func() ([]byte, error) { return m.ReadFile(ctx, rel) })
	}
	return err
}

func (m *EtcMount) Remove(ctx context.Context, rel string) error { return m.remove(rel) }
