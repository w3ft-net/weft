package shell

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kevlarcon/warp/pkg/shellcore"
)

type testFS struct{}

func (testFS) ReadFile(path string) ([]byte, error)     { return nil, fmt.Errorf("not found") }
func (testFS) WriteFile(path string, data []byte) error  { return nil }
func (testFS) ReadDir(path string) ([]string, error) {
	switch path {
	case "/":
		return []string{"bin/", "cron.d/", "log/", "etc/"}, nil
	case "/bin":
		return []string{"check_errors"}, nil
	case "/log":
		return []string{"vimgym.log", "callbox.log"}, nil
	}
	return nil, fmt.Errorf("not found")
}
func (testFS) Remove(path string) error { return nil }

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
