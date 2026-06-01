package shelltest

import (
	"io/fs"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kevlarcon/warp/pkg/shellcore"
)

func TestShellBehavior(t *testing.T) {
	for _, fix := range Fixtures {
		t.Run(fix.Name, func(t *testing.T) {
			for _, tc := range fix.Cases {
				t.Run(tc.Name, func(t *testing.T) {
					got := dispatchCase(fix.Files, tc.Command)
					if got != tc.WantDispatch {
						t.Fatalf("dispatch mismatch:\n  got:  %q\n  want: %q (pure-terminal client must ship the line verbatim)", got, tc.WantDispatch)
					}
				})
			}
		})
	}
}

// dispatchCase builds a MountFS from the fixture files, wires it into a
// shellcore, types the command, and returns the pipeline the client
// shipped to the executor via ActionExec (empty if nothing dispatched —
// e.g. a client-only builtin). The FS is wired so tab-completion and
// bare-cd validation have something to read, but the pure-terminal
// client renders no output itself; it ships the line.
func dispatchCase(files []File, command string) string {
	mfs := buildMountFS(files)

	var dispatched string
	core := shellcore.NewShellCore(
		[]string{"myapp"}, 80, 24,
		func(b []byte) {},
		func(a shellcore.Action) {
			if q, ok := a.(shellcore.ActionExec); ok {
				dispatched = q.Pipeline
			}
		},
	)
	core.SetFS(mfs)
	core.Redraw()

	for _, r := range command {
		core.HandleEvent(shellcore.KeyEvent{Rune: r})
	}
	core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyEnter})

	return dispatched
}

func buildMountFS(files []File) *shellcore.MountFS {
	mfs := shellcore.NewMountFS()
	mfs.Register(&testMount{prefix: "/bin", writable: true, files: files})
	mfs.Register(&testMount{prefix: "/cron.d", writable: true, files: files})
	mfs.Register(&testMount{prefix: "/etc", writable: true, files: files})
	mfs.Register(&testMount{prefix: "/log", files: files, apps: []string{"myapp"}}) // read-only query namespace
	mfs.Register(&testMount{prefix: "/tmp", writable: true, files: files})
	return mfs
}

// testMount is an in-memory Mount for test fixtures. The /log mount is
// read-only and carries an apps list it synthesizes a directory listing
// from (mimicking the query namespace); the rest are writable local
// trees. (Mounts no longer have a "kind" — the server owns query-vs-local
// routing; the fixture keys the synthesized listing off apps.)
type testMount struct {
	prefix   string
	writable bool
	files    []File
	apps     []string
}

func (m *testMount) Prefix() string { return m.prefix }
func (m *testMount) Caps() shellcore.MountCaps {
	return shellcore.MountCaps{Writable: m.writable}
}

func (m *testMount) ReadFile(rel string) ([]byte, error) {
	p := m.abs(rel)
	for _, f := range m.files {
		if f.Path == p && !f.IsDir {
			return []byte(f.Content), nil
		}
	}
	return nil, fs.ErrNotExist
}

func (m *testMount) WriteFile(rel string, data []byte) error {
	p := m.abs(rel)
	for i := range m.files {
		if m.files[i].Path == p {
			m.files[i].Content = string(data)
			return nil
		}
	}
	m.files = append(m.files, File{Path: p, Content: string(data)})
	return nil
}

func (m *testMount) ReadDir(rel string) ([]fs.DirEntry, error) {
	p := m.abs(rel)
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}

	// For the log mount, synthesize the listing from the apps list.
	if len(m.apps) > 0 && rel == "" {
		var entries []fs.DirEntry
		for _, app := range m.apps {
			entries = append(entries, &testDirEntry{name: app + ".log"})
		}
		return entries, nil
	}

	seen := map[string]bool{}
	var entries []fs.DirEntry
	for _, f := range m.files {
		if !strings.HasPrefix(f.Path, p) {
			continue
		}
		rest := strings.TrimPrefix(f.Path, p)
		if rest == "" {
			continue
		}
		name := rest
		isDir := false
		if i := strings.Index(rest, "/"); i >= 0 {
			name = rest[:i]
			isDir = true
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		entries = append(entries, &testDirEntry{name: name, dir: isDir || f.IsDir})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})
	return entries, nil
}

func (m *testMount) Stat(rel string) (fs.FileInfo, error) {
	p := m.abs(rel)
	for _, f := range m.files {
		if f.Path == p {
			return &testFileInfo{name: lastSeg(p), size: int64(len(f.Content)), dir: f.IsDir}, nil
		}
	}
	// Check implicit directory.
	prefix := p + "/"
	for _, f := range m.files {
		if strings.HasPrefix(f.Path, prefix) {
			return &testFileInfo{name: lastSeg(p), dir: true}, nil
		}
	}
	return nil, fs.ErrNotExist
}

func (m *testMount) Remove(rel string) error {
	if !m.Caps().Writable {
		return shellcore.ErrReadOnly
	}
	p := m.abs(rel)
	for i := range m.files {
		if m.files[i].Path == p {
			m.files = append(m.files[:i], m.files[i+1:]...)
			return nil
		}
	}
	return fs.ErrNotExist
}

func (m *testMount) abs(rel string) string {
	if rel == "" {
		return m.prefix
	}
	return m.prefix + "/" + rel
}

func lastSeg(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

type testFileInfo struct {
	name string
	size int64
	dir  bool
}

func (fi *testFileInfo) Name() string { return fi.name }
func (fi *testFileInfo) Size() int64  { return fi.size }
func (fi *testFileInfo) Mode() fs.FileMode {
	if fi.dir {
		return fs.ModeDir | 0755
	}
	return 0644
}
func (fi *testFileInfo) ModTime() time.Time { return time.Time{} }
func (fi *testFileInfo) IsDir() bool        { return fi.dir }
func (fi *testFileInfo) Sys() any           { return nil }

type testDirEntry struct {
	name string
	dir  bool
}

func (e *testDirEntry) Name() string { return e.name }
func (e *testDirEntry) IsDir() bool  { return e.dir }
func (e *testDirEntry) Type() fs.FileMode {
	if e.dir {
		return fs.ModeDir
	}
	return 0
}
func (e *testDirEntry) Info() (fs.FileInfo, error) {
	return &testFileInfo{name: e.name, dir: e.dir}, nil
}
