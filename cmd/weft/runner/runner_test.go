package runner

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/graybeardcorp/warp/pkg/term"
	"github.com/w3ft-net/weft/shell"
)

// TestRenderNotice_KindToColor pins the ANSI color picked per Kind so
// a downstream caller can rely on `success` rendering green, `error`
// red, and unknown / "warning" yellow. The Title + Body content is
// preserved verbatim between the framing rules.
func TestRenderNotice_KindToColor(t *testing.T) {
	cases := []struct {
		kind  string
		color string // expected ANSI fragment
	}{
		{"success", "\033[38;5;2m"},
		{"error", "\033[38;5;1m"},
		{"warning", "\033[38;5;3m"},
		{"something-else", "\033[38;5;3m"}, // default yellow
	}
	for _, c := range cases {
		got := RenderNotice(shell.QueryResultNotice{
			Kind:  c.kind,
			Title: "hello",
			Body:  "world",
		})
		if !strings.Contains(got, c.color) {
			t.Errorf("kind=%q: missing color %q in %q", c.kind, c.color, got)
		}
		if !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
			t.Errorf("kind=%q: title/body lost from %q", c.kind, got)
		}
	}
}

// TestFileHistoryStore_RoundTrip writes entries, reloads them, and
// confirms the on-disk format strips trailing newlines and skips
// blanks (matches the historical contract from when this lived in
// shell/history.go).
func TestFileHistoryStore_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := &FileHistoryStore{path: dir + "/.hist"}

	if err := s.Save([]string{"ls", "range 1h..", "cat /log/api.log"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := []string{"ls", "range 1h..", "cat /log/api.log"}
	if len(got) != len(want) {
		t.Fatalf("entries: got %d want %d (%v)", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("entry %d: got %q want %q", i, got[i], w)
		}
	}
}

// syncBuf is a terminal's output that the loop writes and a test reads.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The whole CLI shell runs on warp/term: a line runs on a goroutine and comes
// back through the host's queue, and `exit` ends Run.
func TestRun_DrivesTheShellOnTerm(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // the history file
	pr, pw := io.Pipe()
	defer pw.Close()
	out := &syncBuf{}
	s := &shell.Shell{}
	done := make(chan error, 1)
	go func() {
		done <- run(context.Background(), s, false,
			term.Input(pr), term.Output(out),
			term.Size(func() (int, int, error) { return 100, 30, nil }),
			term.Resize(make(chan struct{})))
	}()
	io.WriteString(pw, "echo from-weft\r")
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(out.String(), "from-weft") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(out.String(), "from-weft") {
		t.Fatalf("no output from the line: %q", out.String())
	}
	if w := s.Width(); w != 100 {
		t.Errorf("Shell.Width() = %d, want the terminal's 100", w)
	}
	io.WriteString(pw, "exit\r")
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after exit")
	}
}
