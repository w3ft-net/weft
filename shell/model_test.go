package shell

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/kevlarcon/warp/pkg/shellcore"
)

// TestShellCoreDirect tests shellcore through the same write-to-buffer
// pattern the terminal shell uses (direct stdout writes).
func TestShellCoreDirect(t *testing.T) {
	var buf strings.Builder

	core := shellcore.NewShellCore(
		[]string{"test"}, 80, 24,
		func(b []byte) { buf.Write(b) },
		func(a shellcore.Action) {},
	)
	core.Redraw()
	buf.Reset()

	// Type "hello".
	for _, r := range "hello" {
		core.HandleEvent(shellcore.KeyEvent{Rune: r})
	}

	output := buf.String()
	if !strings.Contains(output, "hello") {
		t.Errorf("typed text not in output: %q", output[:min(len(output), 200)])
	}
	if !strings.Contains(output, "$") {
		t.Errorf("prompt missing: %q", output[:min(len(output), 200)])
	}
}

func TestShellCoreWithFS(t *testing.T) {
	var buf strings.Builder
	mfs := testMountFS([]string{"api", "web"})

	core := shellcore.NewShellCore(
		[]string{"api", "web"}, 80, 24,
		func(b []byte) { buf.Write(b) },
		func(a shellcore.Action) {},
	)
	core.SetFS(mfs)
	core.Redraw()
	buf.Reset()

	// Type "ls" and press Enter.
	for _, r := range "ls" {
		core.HandleEvent(shellcore.KeyEvent{Rune: r})
	}
	core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyEnter})

	output := buf.String()
	if !strings.Contains(output, "bin/") {
		t.Errorf("ls missing 'bin/': %q", output[:min(len(output), 300)])
	}
	if !strings.Contains(output, "log/") {
		t.Errorf("ls missing 'log/': %q", output[:min(len(output), 300)])
	}
}

func TestQueryDispatch(t *testing.T) {
	var buf strings.Builder
	var dispatched string

	mfs := testMountFS([]string{"test"})
	core := shellcore.NewShellCore(
		[]string{"test"}, 80, 24,
		func(b []byte) { buf.Write(b) },
		func(a shellcore.Action) {
			if q, ok := a.(shellcore.ActionExec); ok {
				dispatched = q.Pipeline
			}
		},
	)
	core.SetFS(mfs)
	core.Redraw()

	for _, r := range "cat /log/test.log" {
		core.HandleEvent(shellcore.KeyEvent{Rune: r})
	}
	core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyEnter})

	if dispatched == "" {
		t.Fatal("no query dispatched")
	}
	if !strings.Contains(dispatched, "test.log") {
		t.Errorf("dispatched pipeline = %q, want something with test.log", dispatched)
	}
}

func TestSpaceWorks(t *testing.T) {
	var buf strings.Builder

	core := shellcore.NewShellCore(
		[]string{}, 80, 24,
		func(b []byte) { buf.Write(b) },
		func(a shellcore.Action) {},
	)
	core.Redraw()
	buf.Reset()

	for _, r := range "ls /log" {
		core.HandleEvent(shellcore.KeyEvent{Rune: r})
	}

	if core.GetInput() != "ls /log" {
		t.Errorf("input = %q, want %q", core.GetInput(), "ls /log")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// testMountFS builds a MountFS with stub mounts matching the weft
// virtual layout, suitable for tests that exercise shellcore integration.
func testMountFS(apps []string) *shellcore.MountFS {
	mfs := shellcore.NewMountFS()
	mfs.Register(&stubMount{prefix: "/bin", kind: shellcore.MountLocal})
	mfs.Register(&stubMount{prefix: "/cron.d", kind: shellcore.MountLocal})
	mfs.Register(&stubMount{prefix: "/etc", kind: shellcore.MountLocal})
	mfs.Register(&stubMount{prefix: "/log", kind: shellcore.MountQuery, apps: apps})
	mfs.Register(&stubMount{prefix: "/tmp", kind: shellcore.MountLocal})
	return mfs
}

type stubMount struct {
	prefix string
	kind   shellcore.MountKind
	apps   []string
}

func (m *stubMount) Prefix() string { return m.prefix }
func (m *stubMount) Caps() shellcore.MountCaps {
	return shellcore.MountCaps{Kind: m.kind, Writable: m.kind == shellcore.MountLocal}
}
func (m *stubMount) ReadDir(rel string) ([]fs.DirEntry, error) {
	if m.prefix == "/log" && rel == "" {
		var entries []fs.DirEntry
		for _, app := range m.apps {
			entries = append(entries, &stubDirEntry{name: app + ".log"})
		}
		return entries, nil
	}
	return nil, nil
}
func (m *stubMount) ReadFile(string) ([]byte, error)  { return nil, nil }
func (m *stubMount) Stat(string) (fs.FileInfo, error) { return nil, nil }
func (m *stubMount) WriteFile(string, []byte) error   { return nil }
func (m *stubMount) Remove(string) error              { return nil }

type stubDirEntry struct{ name string }

func (e *stubDirEntry) Name() string               { return e.name }
func (e *stubDirEntry) IsDir() bool                { return false }
func (e *stubDirEntry) Type() fs.FileMode          { return 0 }
func (e *stubDirEntry) Info() (fs.FileInfo, error) { return nil, nil }
