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

func TestLsIsNotAQuery(t *testing.T) {
	queryDispatched := false
	var buf strings.Builder

	core := shellcore.NewShellCore(
		[]string{"vimgym"}, 80, 24,
		func(b []byte) { buf.Write(b) },
		func(a shellcore.Action) {
			if _, ok := a.(shellcore.ActionQuery); ok {
				queryDispatched = true
			}
		},
	)
	core.SetFS(testFS{})
	core.Redraw()
	buf.Reset()

	for _, r := range "ls" {
		core.HandleEvent(shellcore.KeyEvent{Rune: r})
	}
	core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyEnter})

	if queryDispatched {
		t.Error("'ls' dispatched a query")
	}
	if !strings.Contains(buf.String(), "bin/") {
		t.Errorf("'ls' missing 'bin/': %q", buf.String()[:min(len(buf.String()), 300)])
	}
}

func TestLsLogShowsFiles(t *testing.T) {
	var buf strings.Builder

	core := shellcore.NewShellCore(
		[]string{"vimgym"}, 80, 24,
		func(b []byte) { buf.Write(b) },
		func(a shellcore.Action) {},
	)
	core.SetFS(testFS{})
	core.Redraw()
	buf.Reset()

	for _, r := range "ls /log" {
		core.HandleEvent(shellcore.KeyEvent{Rune: r})
	}
	core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyEnter})

	if !strings.Contains(buf.String(), "vimgym.log") {
		t.Errorf("'ls /log' missing 'vimgym.log': %q", buf.String()[:min(len(buf.String()), 300)])
	}
}

func TestLsWithoutFS(t *testing.T) {
	queryDispatched := false
	var buf strings.Builder

	core := shellcore.NewShellCore(
		[]string{"vimgym"}, 80, 24,
		func(b []byte) { buf.Write(b) },
		func(a shellcore.Action) {
			if _, ok := a.(shellcore.ActionQuery); ok {
				queryDispatched = true
			}
		},
	)
	// No FS set.
	core.Redraw()
	buf.Reset()

	for _, r := range "ls" {
		core.HandleEvent(shellcore.KeyEvent{Rune: r})
	}
	core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyEnter})

	if queryDispatched {
		t.Error("'ls' dispatched a query")
	}
}

func TestLsRelativePath(t *testing.T) {
	var buf strings.Builder

	core := shellcore.NewShellCore(
		[]string{"vimgym"}, 80, 24,
		func(b []byte) { buf.Write(b) },
		func(a shellcore.Action) {},
	)
	core.SetFS(testFS{})
	core.Redraw()
	buf.Reset()

	for _, r := range "ls log" {
		core.HandleEvent(shellcore.KeyEvent{Rune: r})
	}
	core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyEnter})

	if !strings.Contains(buf.String(), "vimgym.log") {
		t.Errorf("'ls log' missing 'vimgym.log': %q", buf.String()[:min(len(buf.String()), 300)])
	}
}
