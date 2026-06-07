package shell

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/graybeardcorp/warp/pkg/shellcore"
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

// TestShellCoreShipsLs: with an FS wired in, `ls` still ships to the
// server in pure-terminal mode (the server lists via runLocal+shellfs).
// The client does not render the listing itself anymore; directory-
// listing content is covered server-side.
func TestShellCoreShipsLs(t *testing.T) {
	mfs := testMountFS([]string{"api", "web"})
	var dispatched string
	core := shellcore.NewShellCore(
		[]string{"api", "web"}, 80, 24,
		func(b []byte) {},
		func(a shellcore.Action) {
			if q, ok := a.(shellcore.ActionExec); ok {
				dispatched = q.Pipeline
			}
		},
	)
	core.SetFS(mfs)
	core.Redraw()

	for _, r := range "ls" {
		core.HandleEvent(shellcore.KeyEvent{Rune: r})
	}
	core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyEnter})

	if dispatched != "ls" {
		t.Errorf("ls dispatched %q, want it shipped verbatim as %q", dispatched, "ls")
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
	mfs.Register(&stubMount{prefix: "/bin", writable: true})
	mfs.Register(&stubMount{prefix: "/cron.d", writable: true})
	mfs.Register(&stubMount{prefix: "/etc", writable: true})
	mfs.Register(&stubMount{prefix: "/log", apps: apps}) // read-only query namespace
	mfs.Register(&stubMount{prefix: "/tmp", writable: true})
	return mfs
}

type stubMount struct {
	prefix   string
	writable bool
	apps     []string
}

func (m *stubMount) Prefix() string { return m.prefix }
func (m *stubMount) Caps() shellcore.MountCaps {
	return shellcore.MountCaps{Writable: m.writable}
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
