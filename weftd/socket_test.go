package weftd

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// shortSocketPath returns a temp path short enough for a Unix socket —
// t.TempDir() embeds the full test name and can exceed sun_path's ~104
// byte limit on macOS.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "wsock")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

func dialAndSend(t *testing.T, path string, lines ...string) {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	for _, l := range lines {
		if _, err := conn.Write([]byte(l + "\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
}

func recvLine(t *testing.T, sl *SocketListener) SocketLine {
	t.Helper()
	select {
	case line, ok := <-sl.Lines():
		if !ok {
			t.Fatal("Lines() channel closed unexpectedly")
		}
		return line
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for socket line")
		return SocketLine{}
	}
}

// TestSocketListener_BackwardCompat asserts a plain "app:raw line" with no
// \x1e trace envelope parses byte-identically to before the envelope
// extension — TraceID empty, Line unchanged.
func TestSocketListener_BackwardCompat(t *testing.T) {
	path := shortSocketPath(t)
	sl, err := NewSocketListener(path, "default")
	if err != nil {
		t.Fatalf("NewSocketListener: %v", err)
	}
	defer sl.Close()

	dialAndSend(t, path, "myapp:plain log line, no envelope")

	got := recvLine(t, sl)
	want := SocketLine{App: "myapp", TraceID: "", Line: "plain log line, no envelope"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestSocketListener_TraceEnvelope asserts the \x1eTRACE_ID\x1e envelope
// is parsed out structurally, leaving Line holding just the raw text.
func TestSocketListener_TraceEnvelope(t *testing.T) {
	path := shortSocketPath(t)
	sl, err := NewSocketListener(path, "default")
	if err != nil {
		t.Fatalf("NewSocketListener: %v", err)
	}
	defer sl.Close()

	dialAndSend(t, path, "myapp:\x1eabc-123\x1eraw message")

	got := recvLine(t, sl)
	want := SocketLine{App: "myapp", TraceID: "abc-123", Line: "raw message"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestSocketListener_NoAppPrefixDefaultsApp confirms the default-app
// fallback (no "app:" prefix) is unaffected by the envelope addition, and
// still composes correctly when an envelope is present.
func TestSocketListener_NoAppPrefixDefaultsApp(t *testing.T) {
	path := shortSocketPath(t)
	sl, err := NewSocketListener(path, "default")
	if err != nil {
		t.Fatalf("NewSocketListener: %v", err)
	}
	defer sl.Close()

	dialAndSend(t, path, "\x1etrace-1\x1eno app prefix here")

	got := recvLine(t, sl)
	want := SocketLine{App: "default", TraceID: "trace-1", Line: "no app prefix here"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestSocketListener_WorldWritableUnderRestrictiveUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)

	path := shortSocketPath(t)
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	sl, err := NewSocketListener(path, "default")
	if err != nil {
		t.Fatalf("NewSocketListener: %v", err)
	}
	defer sl.Close()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s is %v, want a socket", path, fi.Mode())
	}
	if perm := fi.Mode().Perm(); perm != 0o666 {
		t.Errorf("socket mode = %o, want 666", perm)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("directory has %d entries, want only the socket", len(entries))
	}
	dialAndSend(t, path, "ok")
	if got := recvLine(t, sl); got.Line != "ok" {
		t.Errorf("line = %q", got.Line)
	}
}
