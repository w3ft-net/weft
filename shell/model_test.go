package shell

import (
	"context"
	"io/fs"
	"strings"
	"testing"

	shellcore "github.com/graybeardcorp/warp/pkg/shell"
)

// TestShellCoreDirect tests shellcore through the same write-to-buffer
// pattern the terminal shell uses (direct stdout writes).
func TestShellCoreDirect(t *testing.T) {
	var buf strings.Builder

	core := newCore(80, 24, func(b []byte) { buf.Write(b) })
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

// TestShellCoreShipsLs: with an FS wired in, `ls` still ships to the server
// (the server lists via runLocal+shellfs). The client does not render the
// listing itself; directory-listing content is covered server-side.
func TestShellCoreShipsLs(t *testing.T) {
	if got := lsDispatch(t, testMountFS([]string{"api", "web"}), "ls"); got != "ls" {
		t.Errorf("ls dispatched %q, want it shipped verbatim as %q", got, "ls")
	}
}

func TestQueryDispatch(t *testing.T) {
	got := lsDispatch(t, testMountFS([]string{"test"}), "cat /log/test.log")
	if got == "" {
		t.Fatal("no query dispatched")
	}
	if !strings.Contains(got, "test.log") {
		t.Errorf("dispatched pipeline = %q, want something with test.log", got)
	}
}

func TestSpaceWorks(t *testing.T) {
	var buf strings.Builder

	core := newCore(80, 24, func(b []byte) { buf.Write(b) })
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
func (m *stubMount) Authorize(_ context.Context, _ string, want shellcore.Perm) error {
	if !m.writable && want&shellcore.PermWrite != 0 {
		return shellcore.ErrReadOnly
	}
	return shellcore.ErrNotSupported
}
func (m *stubMount) ReadDir(ctx context.Context, rel string) ([]fs.DirEntry, error) {
	if m.prefix == "/log" && rel == "" {
		var entries []fs.DirEntry
		for _, app := range m.apps {
			entries = append(entries, &stubDirEntry{name: app + ".log"})
		}
		return entries, nil
	}
	return nil, nil
}
func (m *stubMount) ReadFile(context.Context, string) ([]byte, error)  { return nil, nil }
func (m *stubMount) Stat(context.Context, string) (fs.FileInfo, error) { return nil, nil }
func (m *stubMount) WriteFile(context.Context, string, []byte) error   { return nil }
func (m *stubMount) Remove(context.Context, string) error              { return nil }

type stubDirEntry struct{ name string }

func (e *stubDirEntry) Name() string               { return e.name }
func (e *stubDirEntry) IsDir() bool                { return false }
func (e *stubDirEntry) Type() fs.FileMode          { return 0 }
func (e *stubDirEntry) Info() (fs.FileInfo, error) { return nil, nil }

// dispHost is a Host that writes to a callback and passes Actions to another.
type dispHost struct {
	bufHost
	dispatch func(shellcore.Action)
}

func (d dispHost) Dispatch(a shellcore.Action) { d.dispatch(a) }

func newCoreD(w, h int, write func([]byte), dispatch func(shellcore.Action)) *shellcore.Shell {
	return shellcore.New(dispHost{bufHost{w, h, write}, dispatch})
}

// bufHost is a Host that writes to a callback and drops Actions.
type bufHost struct {
	w, h  int
	write func([]byte)
}

func (b bufHost) Write(p []byte)            { b.write(p) }
func (b bufHost) Dispatch(shellcore.Action) {}
func (b bufHost) Size() (int, int)          { return b.w, b.h }

func newCore(w, h int, write func([]byte)) *shellcore.Shell {
	return shellcore.New(bufHost{w, h, write})
}

// postHost is a Host that is also a Poster: runs go to a goroutine and come
// back through post.
type postHost struct {
	dispHost
	post func(func())
}

func (p postHost) Post(fn func()) { p.post(fn) }

func newCoreAsync(w, h int, write func([]byte), dispatch func(shellcore.Action), post func(func())) *shellcore.Shell {
	return shellcore.New(postHost{dispHost{bufHost{w, h, write}, dispatch}, post})
}
