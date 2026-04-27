package shell

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kevlarcon/warp/pkg/shellcore"
)

// TestExecNonInteractiveChart is the documented smoke test from the
// shell-charts plan: `weft sh -c "chart errors"` should print a chart
// to stdout and return cleanly without entering any persistent mode.
func TestExecNonInteractiveChart(t *testing.T) {
	s := &Shell{
		FS: &chartTestFS{
			results: []shellcore.WatchResultValue{
				{Value: "1", RanAt: "2026-04-25T12:00:00Z"},
				{Value: "5", RanAt: "2026-04-25T12:01:00Z"},
				{Value: "3", RanAt: "2026-04-25T12:02:00Z"},
				{Value: "8", RanAt: "2026-04-25T12:03:00Z"},
				{Value: "12", RanAt: "2026-04-25T12:04:00Z"},
			},
		},
	}
	var buf bytes.Buffer
	if err := s.ExecNonInteractive("chart errors", &buf); err != nil {
		t.Fatalf("ExecNonInteractive: %v", err)
	}
	out := buf.String()
	if out == "" {
		t.Fatal("expected non-empty chart output")
	}
	// Title appears verbatim (after stripping ANSI it's "errors  (last 5)").
	if !strings.Contains(out, "errors") {
		t.Errorf("output missing watch name 'errors':\n%s", out)
	}
	// Time-axis labels: HH:MM colon-separated.
	if !strings.Contains(out, ":") {
		t.Errorf("output missing time-axis colon:\n%s", out)
	}
}

func TestExecNonInteractiveChartSparkline(t *testing.T) {
	s := &Shell{
		FS: &chartTestFS{
			results: []shellcore.WatchResultValue{
				{Value: "1", RanAt: "2026-04-25T12:00:00Z"},
				{Value: "5", RanAt: "2026-04-25T12:01:00Z"},
				{Value: "3", RanAt: "2026-04-25T12:02:00Z"},
			},
		},
	}
	var buf bytes.Buffer
	if err := s.ExecNonInteractive("chart errors -t sparkline", &buf); err != nil {
		t.Fatalf("ExecNonInteractive: %v", err)
	}
	out := buf.String()
	hasSpark := false
	for _, r := range out {
		if r >= 0x2581 && r <= 0x2588 {
			hasSpark = true
			break
		}
	}
	if !hasSpark {
		t.Errorf("expected sparkline glyph in output:\n%q", out)
	}
}

func TestExecNonInteractiveChartFollowFlagIgnored(t *testing.T) {
	// --follow has no meaning in non-interactive mode (no terminal to
	// redraw into); the runner should treat it as a one-shot anyway.
	s := &Shell{
		FS: &chartTestFS{
			results: []shellcore.WatchResultValue{
				{Value: "5", RanAt: "2026-04-25T12:00:00Z"},
				{Value: "8", RanAt: "2026-04-25T12:01:00Z"},
			},
		},
	}
	var buf bytes.Buffer
	if err := s.ExecNonInteractive("chart errors -f -i 1s", &buf); err != nil {
		t.Fatalf("ExecNonInteractive: %v", err)
	}
	if buf.Len() == 0 {
		t.Error("expected chart output even with --follow flag")
	}
}

func TestExecNonInteractiveChartUnknownWatch(t *testing.T) {
	s := &Shell{
		FS: &chartTestFS{results: nil}, // no results for any name
	}
	var buf bytes.Buffer
	if err := s.ExecNonInteractive("chart ghost", &buf); err != nil {
		t.Fatalf("ExecNonInteractive: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "no numeric results") {
		t.Errorf("expected 'no numeric results' message, got:\n%s", out)
	}
}

// chartTestFS implements FS + WatchResultsProvider for non-interactive
// shell tests.
type chartTestFS struct {
	results []shellcore.WatchResultValue
}

func (f *chartTestFS) ReadFile(string) ([]byte, error)  { return nil, nil }
func (f *chartTestFS) WriteFile(string, []byte) error   { return nil }
func (f *chartTestFS) ReadDir(string) ([]string, error) { return nil, nil }
func (f *chartTestFS) Remove(string) error              { return nil }
func (f *chartTestFS) GetWatchResults(string, int) ([]shellcore.WatchResultValue, error) {
	return f.results, nil
}
