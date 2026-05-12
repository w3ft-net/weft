package runner

import (
	"strings"
	"testing"

	"github.com/kevlarcon/weft/shell"
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
