package mounts

import (
	"fmt"
	"io/fs"
	"strings"

	"github.com/graybeardcorp/warp/pkg/shellcore"
)

type LogMount struct{ cfg Config }

func (m *LogMount) Prefix() string { return "/log" }

func (m *LogMount) Caps() shellcore.MountCaps {
	return shellcore.MountCaps{Writable: false}
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

	nsName := strings.SplitN(rel, "/", 2)[0]
	if !m.canReadNamespace(nsName) {
		return nil, fmt.Errorf("/log/%s: permission denied", rel)
	}

	// Every LogServices entry is a slash path whose last segment is the
	// stream (rendered as <last>.log); everything before it is a
	// directory, whether it is an env or a service.
	prefix := rel + "/"
	var entries []fs.DirEntry
	seen := map[string]bool{}
	found := false
	for _, a := range m.cfg.LogServices {
		if !strings.HasPrefix(a, prefix) {
			continue
		}
		found = true
		name, _, more := strings.Cut(strings.TrimPrefix(a, prefix), "/")
		if !more {
			name += ".log"
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		entries = append(entries, &vfsDirEntry{name: name, dir: more})
	}
	if !found && !m.isNamespace(rel) {
		return nil, fs.ErrNotExist
	}
	return entries, nil
}

func (m *LogMount) ReadFile(rel string) ([]byte, error) {
	if rel == "" {
		return nil, fs.ErrNotExist
	}
	app := strings.TrimSuffix(rel, ".log")
	if m.hasLogService(app) {
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
	nsName := strings.SplitN(rel, "/", 2)[0]

	if !strings.HasSuffix(rel, ".log") {
		if m.isNamespace(rel) || m.hasLogDir(rel) {
			if !m.canReadNamespace(nsName) {
				return nil, fmt.Errorf("/log/%s: permission denied", rel)
			}
			return &vfsFileInfo{name: lastSegment(rel), dir: true}, nil
		}
	} else {
		app := strings.TrimSuffix(rel, ".log")
		if m.hasLogService(app) {
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

// Walk satisfies shellcore.WalkMount: it returns the entire subtree
// rooted at rel in a single pass over LogServices, so MountFS.Tree skips
// the per-directory ReadDir recursion (one ReadDir + two full-slice
// scans + an uncached permission lookup per visited directory). The
// flat list uses mount-relative paths with "/" separators; files
// carry the ".log" suffix exactly as ReadDir emits them, and each
// namespace also yields a bare "ns/" so empty namespaces still show.
//
// Permission is checked at most once per namespace (memoised) instead
// of once per visited directory.
func (m *LogMount) Walk(rel string) ([]string, error) {
	var nsFilter string
	if rel != "" {
		nsFilter = strings.SplitN(rel, "/", 2)[0]
		if !m.canReadNamespace(nsFilter) {
			return nil, fmt.Errorf("/log/%s: permission denied", rel)
		}
	}

	permCache := map[string]bool{}
	canRead := func(ns string) bool {
		if v, ok := permCache[ns]; ok {
			return v
		}
		v := m.canReadNamespace(ns)
		permCache[ns] = v
		return v
	}

	var out []string
	nsSeen := map[string]bool{}
	for _, a := range m.cfg.LogServices {
		parts := strings.Split(a, "/")
		ns := parts[0]
		if nsFilter != "" && ns != nsFilter {
			continue
		}
		if !canRead(ns) {
			continue
		}
		if !nsSeen[ns] {
			nsSeen[ns] = true
			out = append(out, ns+"/")
		}
		if len(parts) == 1 {
			continue // bare namespace, no app — the "ns/" marker is enough
		}
		if rel != "" && a != rel && !strings.HasPrefix(a, rel+"/") {
			continue
		}
		out = append(out, a+".log")
	}
	return out, nil
}

// --- log tree helpers ---

func (m *LogMount) logNamespaces() []string {
	seen := map[string]bool{}
	var nss []string
	for _, a := range m.cfg.LogServices {
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

func (m *LogMount) isNamespace(name string) bool {
	for _, ns := range m.logNamespaces() {
		if ns == name {
			return true
		}
	}
	return false
}

// hasLogDir reports whether some service lives strictly below rel.
func (m *LogMount) hasLogDir(rel string) bool {
	prefix := rel + "/"
	for _, a := range m.cfg.LogServices {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

func (m *LogMount) hasLogService(app string) bool {
	for _, a := range m.cfg.LogServices {
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
	nsObj, err := m.cfg.Store.FindOrCreateNamespace(m.cfg.OrgID, ns)
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
