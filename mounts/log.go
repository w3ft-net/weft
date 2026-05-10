package mounts

import (
	"fmt"
	"io/fs"
	"strings"

	"github.com/kevlarcon/warp/pkg/shellcore"
)

type LogMount struct{ cfg Config }

func (m *LogMount) Prefix() string { return "/log" }

func (m *LogMount) Caps() shellcore.MountCaps {
	return shellcore.MountCaps{Kind: shellcore.MountQuery, Writable: false}
}

func (m *LogMount) ReadDir(rel string) ([]fs.DirEntry, error) {
	if rel == "" {
		nss := m.logNamespaces()
		var entries []fs.DirEntry
		for _, ns := range nss {
			if !m.canReadNamespace(ns) {
				continue
			}
			entries = append(entries, &vfsDirEntry{name: ns, dir: true})
		}
		return entries, nil
	}

	segs := strings.Split(rel, "/")
	nsName := segs[0]
	if !m.canReadNamespace(nsName) {
		return nil, fmt.Errorf("/log/%s: permission denied", rel)
	}

	switch len(segs) {
	case 1:
		var entries []fs.DirEntry
		for _, env := range m.logEnvsInNamespace(nsName) {
			entries = append(entries, &vfsDirEntry{name: env, dir: true})
		}
		for _, app := range m.logAppsInNamespace(nsName) {
			entries = append(entries, &vfsDirEntry{name: app + ".log"})
		}
		return entries, nil
	case 2:
		env := segs[1]
		subApps := m.logAppsInNamespaceEnv(nsName, env)
		if len(subApps) > 0 {
			entries := make([]fs.DirEntry, len(subApps))
			for i, app := range subApps {
				entries[i] = &vfsDirEntry{name: app + ".log"}
			}
			return entries, nil
		}
	}

	return nil, fs.ErrNotExist
}

func (m *LogMount) ReadFile(rel string) ([]byte, error) {
	if rel == "" {
		return nil, fs.ErrNotExist
	}
	app := strings.TrimSuffix(rel, ".log")
	if m.hasLogApp(app) {
		ns := namespaceFromApp(app)
		if !m.canReadNamespace(ns) {
			return nil, fmt.Errorf("/log/%s: permission denied", rel)
		}
		return []byte{}, nil
	}
	return nil, fs.ErrNotExist
}

func (m *LogMount) Stat(rel string) (fs.FileInfo, error) {
	if rel == "" {
		return &vfsFileInfo{name: "log", dir: true}, nil
	}
	segs := strings.Split(rel, "/")
	nsName := segs[0]

	if !strings.HasSuffix(rel, ".log") {
		for _, ns := range m.logNamespaces() {
			if ns == nsName {
				if !m.canReadNamespace(ns) {
					return nil, fmt.Errorf("/log/%s: permission denied", rel)
				}
				return &vfsFileInfo{name: lastSegment(rel), dir: true}, nil
			}
		}
		if len(segs) == 2 {
			for _, env := range m.logEnvsInNamespace(nsName) {
				if env == segs[1] {
					return &vfsFileInfo{name: segs[1], dir: true}, nil
				}
			}
		}
	} else {
		app := strings.TrimSuffix(rel, ".log")
		if m.hasLogApp(app) {
			ns := namespaceFromApp(app)
			if !m.canReadNamespace(ns) {
				return nil, fmt.Errorf("/log/%s: permission denied", rel)
			}
			return &vfsFileInfo{name: lastSegment(rel)}, nil
		}
	}
	return nil, fs.ErrNotExist
}

func (m *LogMount) WriteFile(string, []byte) error { return shellcore.ErrReadOnly }
func (m *LogMount) Remove(string) error            { return shellcore.ErrReadOnly }

// --- log tree helpers ---

func (m *LogMount) logNamespaces() []string {
	seen := map[string]bool{}
	var nss []string
	for _, a := range m.cfg.LogApps {
		ns := a
		if idx := strings.Index(a, "/"); idx >= 0 {
			ns = a[:idx]
		}
		if !seen[ns] {
			seen[ns] = true
			nss = append(nss, ns)
		}
	}
	return nss
}

func (m *LogMount) logAppsInNamespace(ns string) []string {
	prefix := ns + "/"
	var apps []string
	for _, a := range m.cfg.LogApps {
		if !strings.HasPrefix(a, prefix) {
			continue
		}
		rest := strings.TrimPrefix(a, prefix)
		if strings.Contains(rest, "/") {
			continue
		}
		apps = append(apps, rest)
	}
	return apps
}

func (m *LogMount) logEnvsInNamespace(ns string) []string {
	prefix := ns + "/"
	seen := map[string]bool{}
	var envs []string
	for _, a := range m.cfg.LogApps {
		if !strings.HasPrefix(a, prefix) {
			continue
		}
		rest := strings.TrimPrefix(a, prefix)
		i := strings.Index(rest, "/")
		if i < 0 {
			continue
		}
		env := rest[:i]
		if !seen[env] {
			seen[env] = true
			envs = append(envs, env)
		}
	}
	return envs
}

func (m *LogMount) logAppsInNamespaceEnv(ns, env string) []string {
	prefix := ns + "/" + env + "/"
	var apps []string
	for _, a := range m.cfg.LogApps {
		if strings.HasPrefix(a, prefix) {
			apps = append(apps, strings.TrimPrefix(a, prefix))
		}
	}
	return apps
}

func (m *LogMount) hasLogApp(app string) bool {
	for _, a := range m.cfg.LogApps {
		if a == app {
			return true
		}
	}
	return false
}

func (m *LogMount) canReadNamespace(ns string) bool {
	if m.cfg.IsOwner || m.cfg.UserID == "" {
		return true
	}
	nsObj, err := m.cfg.Store.FindOrCreateNamespace(m.cfg.CustomerID, ns)
	if err != nil {
		return false
	}
	perm, err := m.cfg.Store.GetNamespacePermission(m.cfg.UserID, nsObj.ID)
	if err != nil {
		return false
	}
	return perm.CanRead
}

func namespaceFromApp(app string) string {
	if idx := strings.Index(app, "/"); idx >= 0 {
		return app[:idx]
	}
	return app
}
