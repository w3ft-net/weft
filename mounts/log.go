package mounts

import (
	"io/fs"
	"slices"
	"sort"
	"strings"

	"github.com/graybeardcorp/warp/pkg/shellcore"
)

type LogMount struct{ cfg Config }

// A mount whose Authorize drifts from the interface silently stops being
// gated, because MountFS probes for it at runtime.
var _ shellcore.AuthMount = (*LogMount)(nil)

func (m *LogMount) Prefix() string { return "/log" }

func (m *LogMount) Caps() shellcore.MountCaps {
	return shellcore.MountCaps{Writable: false}
}

// Authorize is the mount's one access check, called by MountFS before every
// operation: the first path segment must be a namespace the caller can read.
// A namespace that is missing and one that is unreadable are
// indistinguishable, so probing names reveals nothing.
func (m *LogMount) Authorize(rel string, _ shellcore.Perm) error {
	if rel == "" {
		return nil
	}
	nss, _, err := m.cfg.scope()
	if err != nil {
		return err
	}
	if !slices.Contains(nss, strings.SplitN(rel, "/", 2)[0]) {
		return fs.ErrNotExist
	}
	return nil
}

func (m *LogMount) ReadDir(rel string) ([]fs.DirEntry, error) {
	if rel == "" {
		nss, _, err := m.cfg.scope()
		if err != nil {
			return nil, err
		}
		var entries []fs.DirEntry
		for _, ns := range nss {
			entries = append(entries, &vfsDirEntry{name: ns, dir: true})
		}
		return entries, nil
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
	if !found {
		if kinds := m.mergedKinds(rel); len(kinds) > 0 {
			for _, k := range kinds {
				entries = append(entries, &vfsDirEntry{name: k + ".log"})
			}
			return entries, nil
		}
	}
	// Authorize has vouched for the first segment, so a bare one is a
	// namespace with no services yet: an empty directory.
	if !found && strings.Contains(rel, "/") {
		return nil, fs.ErrNotExist
	}
	return entries, nil
}

func (m *LogMount) ReadFile(rel string) ([]byte, error) {
	if rel == "" {
		return nil, fs.ErrNotExist
	}
	app := strings.TrimSuffix(rel, ".log")
	if m.hasLogService(app) || m.hasMergedKind(app) {
		return []byte{}, nil
	}
	return nil, fs.ErrNotExist
}

func (m *LogMount) Stat(rel string) (fs.FileInfo, error) {
	if rel == "" {
		return &vfsFileInfo{name: "log", dir: true}, nil
	}

	if !strings.HasSuffix(rel, ".log") {
		// A bare first segment is a namespace, vouched for by Authorize.
		if !strings.Contains(rel, "/") || m.hasLogDir(rel) || len(m.mergedKinds(rel)) > 0 {
			return &vfsFileInfo{name: lastSegment(rel), dir: true}, nil
		}
	} else {
		app := strings.TrimSuffix(rel, ".log")
		if m.hasLogService(app) || m.hasMergedKind(app) {
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
// readable namespace also yields a bare "ns/" so empty namespaces still
// show.
//
// Readable namespaces come from one query, not a permission lookup per
// namespace.
func (m *LogMount) Walk(rel string) ([]string, error) {
	nss, _, err := m.cfg.scope()
	if err != nil {
		return nil, err
	}
	readable := make(map[string]bool, len(nss))
	for _, ns := range nss {
		readable[ns] = true
	}

	nsFilter, _, _ := strings.Cut(rel, "/")

	var out []string
	for _, ns := range nss {
		if nsFilter == "" || ns == nsFilter {
			out = append(out, ns+"/")
		}
	}
	for _, a := range m.cfg.LogServices {
		parts := strings.Split(a, "/")
		if len(parts) == 1 || !readable[parts[0]] || nsFilter != "" && parts[0] != nsFilter {
			continue
		}
		if rel != "" && a != rel && !strings.HasPrefix(a, rel+"/") {
			continue
		}
		out = append(out, a+".log")
	}
	return out, nil
}

// --- log tree helpers ---

// mergedKinds backs the 3-segment form /log/<ns>/<svc>: when rel is
// "<ns>/<svc>", names no real directory (no env-less service of that name),
// and some env carries <svc> as "<ns>/<env>/<svc>/<kind>", it returns the
// sorted union of those kinds. That is the view cat has always read
// /log/<ns>/<svc>/<kind>.log through, which spans every env; ls, cd and stat
// now agree with it. A real env-less service of the same name wins.
func (m *LogMount) mergedKinds(rel string) []string {
	parts := strings.Split(rel, "/")
	if len(parts) != 2 || m.hasLogDir(rel) {
		return nil
	}
	ns, svc := parts[0], parts[1]
	seen := map[string]bool{}
	var kinds []string
	for _, a := range m.cfg.LogServices {
		p := strings.Split(a, "/")
		if len(p) != 4 || p[0] != ns || p[2] != svc || seen[p[3]] {
			continue
		}
		seen[p[3]] = true
		kinds = append(kinds, p[3])
	}
	sort.Strings(kinds)
	return kinds
}

// hasMergedKind reports whether app is "<ns>/<svc>/<kind>" for a kind that
// mergedKinds lists.
func (m *LogMount) hasMergedKind(app string) bool {
	parts := strings.Split(app, "/")
	if len(parts) != 3 {
		return false
	}
	for _, k := range m.mergedKinds(parts[0] + "/" + parts[1]) {
		if k == parts[2] {
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
