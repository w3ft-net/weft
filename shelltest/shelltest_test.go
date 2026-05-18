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
					got, gotErr := execCase(fix.Files, tc.Command)
					if tc.WantErr != "" {
						if gotErr == "" {
							t.Fatalf("expected error containing %q, got output: %q", tc.WantErr, got)
						}
						if !strings.Contains(strings.ToLower(gotErr), strings.ToLower(tc.WantErr)) {
							t.Fatalf("error = %q, want substring %q", gotErr, tc.WantErr)
						}
						return
					}
					if gotErr != "" {
						t.Fatalf("unexpected error: %s", gotErr)
					}
					if got != tc.Want {
						t.Fatalf("output mismatch:\n  got:  %q\n  want: %q", got, tc.Want)
					}
				})
			}
		})
	}
}

// execCase builds a MountFS from the fixture files, wires it into a
// shellcore, executes the command, and returns the output or error.
func execCase(files []File, command string) (output, errMsg string) {
	mfs := buildMountFS(files)

	var buf strings.Builder
	var queryDispatched bool

	core := shellcore.NewShellCore(
		[]string{"myapp"}, 80, 24,
		func(b []byte) { buf.Write(b) },
		func(a shellcore.Action) {
			if _, ok := a.(shellcore.ActionExec); ok {
				queryDispatched = true
			}
		},
	)
	core.SetFS(mfs)
	core.Redraw()
	buf.Reset()

	for _, r := range command {
		core.HandleEvent(shellcore.KeyEvent{Rune: r})
	}
	core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyEnter})

	if queryDispatched {
		return "__QUERY_DISPATCHED__", ""
	}

	raw := buf.String()
	// Strip ANSI and normalize terminal line endings.
	stripped := shellcore.StripANSI(raw)
	stripped = strings.ReplaceAll(stripped, "\r\n", "\n")
	stripped = strings.ReplaceAll(stripped, "\r", "")
	// The scrollback includes the prompt + typed command + output + next prompt.
	// Find the first newline (end of the echoed command), then take everything
	// up to the final prompt line.
	lines := strings.Split(stripped, "\n")
	// Skip the first line (command echo) and the last line (next prompt).
	if len(lines) > 2 {
		outputLines := lines[1 : len(lines)-1]
		// Remove trailing empty line if present.
		if len(outputLines) > 0 && outputLines[len(outputLines)-1] == "" {
			outputLines = outputLines[:len(outputLines)-1]
		}
		result := strings.Join(outputLines, "\n")
		if result != "" {
			result += "\n"
		}
		// Check if the output contains an error indicator.
		lower := strings.ToLower(result)
		if strings.Contains(lower, "not found") || strings.Contains(lower, "no such") ||
			strings.Contains(lower, "read-only") || strings.Contains(lower, "permission denied") {
			return "", strings.TrimSpace(result)
		}
		return result, ""
	}
	return "", ""
}

func buildMountFS(files []File) *shellcore.MountFS {
	mfs := shellcore.NewMountFS()
	mfs.Register(&testMount{prefix: "/bin", files: files})
	mfs.Register(&testMount{prefix: "/cron.d", files: files})
	mfs.Register(&testMount{prefix: "/etc", files: files})
	mfs.Register(&testMount{prefix: "/log", kind: shellcore.MountQuery, files: files, apps: []string{"myapp"}})
	mfs.Register(&testMount{prefix: "/tmp", files: files})
	return mfs
}

// testMount is an in-memory Mount for test fixtures.
type testMount struct {
	prefix string
	kind   shellcore.MountKind
	files  []File
	apps   []string
}

func (m *testMount) Prefix() string { return m.prefix }
func (m *testMount) Caps() shellcore.MountCaps {
	return shellcore.MountCaps{
		Kind:     m.kind,
		Writable: m.kind == shellcore.MountLocal,
	}
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

	// For log mount, synthesize from apps list.
	if m.kind == shellcore.MountQuery && rel == "" {
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

func (fi *testFileInfo) Name() string      { return fi.name }
func (fi *testFileInfo) Size() int64       { return fi.size }
func (fi *testFileInfo) Mode() fs.FileMode { if fi.dir { return fs.ModeDir | 0755 }; return 0644 }
func (fi *testFileInfo) ModTime() time.Time { return time.Time{} }
func (fi *testFileInfo) IsDir() bool       { return fi.dir }
func (fi *testFileInfo) Sys() any          { return nil }

type testDirEntry struct {
	name string
	dir  bool
}

func (e *testDirEntry) Name() string               { return e.name }
func (e *testDirEntry) IsDir() bool                { return e.dir }
func (e *testDirEntry) Type() fs.FileMode          { if e.dir { return fs.ModeDir }; return 0 }
func (e *testDirEntry) Info() (fs.FileInfo, error) { return &testFileInfo{name: e.name, dir: e.dir}, nil }
