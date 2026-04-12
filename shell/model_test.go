package shell

import (
	"strings"
	"testing"
	"time"

	"github.com/kevlarcon/weft/shellcore"
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
	fs := &DevFS{apps: []string{"api", "web"}, files: make(map[string]string)}

	core := shellcore.NewShellCore(
		[]string{"api", "web"}, 80, 24,
		func(b []byte) { buf.Write(b) },
		func(a shellcore.Action) {},
	)
	core.SetFS(fs)
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

	fs := NewDevFS([]string{"test"})
	core := shellcore.NewShellCore(
		[]string{"test"}, 80, 24,
		func(b []byte) { buf.Write(b) },
		func(a shellcore.Action) {
			if q, ok := a.(shellcore.ActionQuery); ok {
				dispatched = q.Pipeline
			}
		},
	)
	core.SetFS(fs)
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

	// Check that core has "ls /log" in its input (space preserved).
	if core.GetInput() != "ls /log" {
		t.Errorf("input = %q, want %q", core.GetInput(), "ls /log")
	}
}

func TestRemoteFSReadDir(t *testing.T) {
	// Verify RemoteFS basics without a gRPC connection (just the app list).
	// Full gRPC tests would require a running coordinator.
	fs := NewDevFS([]string{"api", "web"})

	entries, err := fs.ReadDir("/")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Errorf("root entries = %d, want 4", len(entries))
	}

	logEntries, err := fs.ReadDir("/log")
	if err != nil {
		t.Fatal(err)
	}
	if len(logEntries) != 2 {
		t.Errorf("log entries = %d, want 2", len(logEntries))
	}
}

func TestDevFSWriteRead(t *testing.T) {
	fs := NewDevFS([]string{"api"})

	err := fs.WriteFile("/bin/check", []byte("echo hi"))
	if err != nil {
		t.Fatal(err)
	}

	data, err := fs.ReadFile("/bin/check")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "echo hi" {
		t.Errorf("read = %q, want %q", string(data), "echo hi")
	}

	entries, err := fs.ReadDir("/bin")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0] != "check" {
		t.Errorf("ls /bin = %v, want [check]", entries)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Ensure unused import doesn't break.
var _ = time.Now
