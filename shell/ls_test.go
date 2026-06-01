package shell

import (
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/kevlarcon/warp/pkg/shellcore"
)

// tEntry / tInfo are minimal fs.DirEntry / fs.FileInfo helpers shared
// by the weft shell test fakes now that shellcore.FS == bash.FS.
type tEntry struct {
	name string
	dir  bool
}

func (e tEntry) Name() string { return e.name }
func (e tEntry) IsDir() bool  { return e.dir }
func (e tEntry) Type() fs.FileMode {
	if e.dir {
		return fs.ModeDir
	}
	return 0
}
func (e tEntry) Info() (fs.FileInfo, error) { return tInfo{e.name, e.dir}, nil }

type tInfo struct {
	name string
	dir  bool
}

func (i tInfo) Name() string { return i.name }
func (i tInfo) Size() int64  { return 0 }
func (i tInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir
	}
	return 0
}
func (i tInfo) ModTime() time.Time { return time.Time{} }
func (i tInfo) IsDir() bool        { return i.dir }
func (i tInfo) Sys() any           { return nil }

// ents builds []fs.DirEntry from names; a trailing "/" marks a dir.
func ents(names ...string) []fs.DirEntry {
	out := make([]fs.DirEntry, len(names))
	for i, n := range names {
		dir := strings.HasSuffix(n, "/")
		out[i] = tEntry{name: strings.TrimSuffix(n, "/"), dir: dir}
	}
	return out
}

type testFS struct{}

func (testFS) ReadFile(path string) ([]byte, error)     { return nil, fmt.Errorf("not found") }
func (testFS) WriteFile(path string, data []byte) error { return nil }
func (testFS) ReadDir(path string) ([]fs.DirEntry, error) {
	switch path {
	case "/":
		return ents("bin/", "cron.d/", "log/", "etc/"), nil
	case "/bin":
		return ents("check_errors"), nil
	case "/log":
		return ents("vimgym.log", "callbox.log"), nil
	}
	return nil, fmt.Errorf("not found")
}
func (testFS) Stat(path string) (fs.FileInfo, error) { return nil, fmt.Errorf("not found") }
func (testFS) Remove(path string) error              { return nil }

// lsDispatch types `line` at the prompt and returns the pipeline the
// core shipped via ActionExec (empty if none). In pure-terminal mode ls
// is not intercepted client-side — it ships to the server, which lists
// the FS through runLocal+shellfs. (The listing-content coverage lives
// server-side; these tests guard the client ships ls unmangled.)
func lsDispatch(t *testing.T, fs shellcore.FS, line string) string {
	t.Helper()
	var dispatched string
	core := shellcore.NewShellCore(
		[]string{"vimgym"}, 80, 24,
		func(b []byte) {},
		func(a shellcore.Action) {
			if q, ok := a.(shellcore.ActionExec); ok {
				dispatched = q.Pipeline
			}
		},
	)
	if fs != nil {
		core.SetFS(fs)
	}
	core.Redraw()
	for _, r := range line {
		core.HandleEvent(shellcore.KeyEvent{Rune: r})
	}
	core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyEnter})
	return dispatched
}

func TestLsShipsToServer(t *testing.T) {
	if got := lsDispatch(t, testFS{}, "ls"); got != "ls" {
		t.Errorf("ls dispatched %q, want it shipped verbatim as %q", got, "ls")
	}
}

func TestLsPathShipsToServer(t *testing.T) {
	if got := lsDispatch(t, testFS{}, "ls /log"); got != "ls /log" {
		t.Errorf("ls /log dispatched %q, want %q", got, "ls /log")
	}
}

func TestLsWithoutFSStillShips(t *testing.T) {
	// Even with no client FS, ls ships — the server owns the listing.
	if got := lsDispatch(t, nil, "ls"); got != "ls" {
		t.Errorf("ls (no FS) dispatched %q, want %q", got, "ls")
	}
}

func TestLsRelativePathShipsUnrewritten(t *testing.T) {
	// Relative path ships as typed; the server resolves it against cwd.
	if got := lsDispatch(t, testFS{}, "ls log"); got != "ls log" {
		t.Errorf("ls log dispatched %q, want %q shipped unrewritten", got, "ls log")
	}
}
