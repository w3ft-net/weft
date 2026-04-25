package shellcore

import (
	"strings"
	"testing"
	"time"
)

func TestParseChartCmd(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want chartArgs
		err  string // substring match; "" = expect no error
	}{
		{
			name: "bare watch name",
			argv: []string{"errors"},
			want: chartArgs{name: "errors", kind: "line", limit: 50, interval: time.Second},
		},
		{
			name: "path form strips /cron.d/",
			argv: []string{"/cron.d/errors"},
			want: chartArgs{name: "errors", kind: "line", limit: 50, interval: time.Second},
		},
		{
			name: "type bar",
			argv: []string{"errors", "-t", "bar"},
			want: chartArgs{name: "errors", kind: "bar", limit: 50, interval: time.Second},
		},
		{
			name: "limit + interval + follow",
			argv: []string{"errors", "-n", "20", "-i", "500ms", "-f"},
			want: chartArgs{name: "errors", kind: "line", limit: 20, interval: 500 * time.Millisecond, follow: true},
		},
		{
			name: "missing watch name",
			argv: []string{"-f"},
			err:  "usage: chart",
		},
		{
			name: "unknown flag",
			argv: []string{"errors", "--no-such"},
			err:  "unknown flag",
		},
		{
			name: "bad type",
			argv: []string{"errors", "-t", "candlestick"},
			err:  "unknown type",
		},
		{
			name: "interval too short",
			argv: []string{"errors", "-i", "10ms"},
			err:  "duration",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, errMsg := parseChartCmd(tc.argv)
			if tc.err != "" {
				if !strings.Contains(errMsg, tc.err) {
					t.Errorf("expected error containing %q, got %q", tc.err, errMsg)
				}
				return
			}
			if errMsg != "" {
				t.Fatalf("unexpected error: %s", errMsg)
			}
			if got != tc.want {
				t.Errorf("parsed = %+v\n   want %+v", got, tc.want)
			}
		})
	}
}

func TestWatchResultsToPoints(t *testing.T) {
	results := []WatchResultValue{
		// newest first (matches the API contract).
		{Value: "alpha 17", RanAt: "2026-04-25T14:00:00Z"},
		{Value: "alpha 12", RanAt: "2026-04-25T13:00:00Z"},
		{Value: "non-numeric", RanAt: "2026-04-25T12:00:00Z"},
		{Value: "alpha 9", RanAt: "2026-04-25T11:00:00Z"},
	}
	pts, skipped := watchResultsToPoints(results)
	if len(pts) != 3 {
		t.Errorf("got %d pts, want 3", len(pts))
	}
	if skipped != 1 {
		t.Errorf("got %d skipped, want 1", skipped)
	}
	// Sorted oldest-first.
	for i := 1; i < len(pts); i++ {
		if pts[i].X < pts[i-1].X {
			t.Errorf("not sorted: pts[%d].X=%v < pts[%d].X=%v", i, pts[i].X, i-1, pts[i-1].X)
		}
	}
	// Last whitespace token is the value.
	if pts[len(pts)-1].Y != 17 {
		t.Errorf("got most-recent Y=%v, want 17 (last field of 'alpha 17')", pts[len(pts)-1].Y)
	}
}

// TestExecChartFlowSmoke threads a fake FS that satisfies both FS and
// WatchResultsProvider, then calls the chart builtin in one-shot mode.
// We verify the rendered output landed in scrollback and the shell is
// still in ModeInput (i.e. one-shot didn't accidentally enter live mode).
func TestExecChartFlowSmoke(t *testing.T) {
	c := NewShellCore(
		[]string{"api"}, 80, 24,
		func([]byte) {},
		func(Action) {},
	)
	c.SetFS(&fakeWatchFS{
		results: []WatchResultValue{
			{Value: "1", RanAt: "2026-04-25T12:00:00Z"},
			{Value: "5", RanAt: "2026-04-25T12:01:00Z"},
			{Value: "3", RanAt: "2026-04-25T12:02:00Z"},
			{Value: "8", RanAt: "2026-04-25T12:03:00Z"},
		},
	})
	if !c.execChart([]string{"chart", "errors"}) {
		t.Fatal("execChart returned false")
	}
	if c.Mode() != ModeInput {
		t.Errorf("one-shot left shell in mode %v, want ModeInput", c.Mode())
	}
	out := c.scrollback.String()
	if !strings.Contains(out, "errors") {
		t.Errorf("scrollback missing watch name 'errors':\n%s", out)
	}
}

// TestExecChartFollowEntersMode verifies --follow drops into ModeChart.
func TestExecChartFollowEntersMode(t *testing.T) {
	c := NewShellCore(
		[]string{"api"}, 80, 24,
		func([]byte) {},
		func(Action) {},
	)
	c.SetFS(&fakeWatchFS{results: []WatchResultValue{
		{Value: "5", RanAt: "2026-04-25T12:00:00Z"},
	}})
	if !c.execChart([]string{"chart", "errors", "-f"}) {
		t.Fatal("execChart returned false")
	}
	if c.Mode() != ModeChart {
		t.Errorf("--follow left shell in mode %v, want ModeChart", c.Mode())
	}
	// Quitting via 'q' returns to input.
	c.HandleEvent(KeyEvent{Rune: 'q'})
	if c.Mode() != ModeInput {
		t.Errorf("q after follow left shell in mode %v, want ModeInput", c.Mode())
	}
}

// TestLsLongCronSparklines verifies `ls -l /cron.d/` decorates each
// watch row with a colored sparkline glyph and the latest value.
func TestLsLongCronSparklines(t *testing.T) {
	c := NewShellCore(
		[]string{"api"}, 80, 24,
		func([]byte) {},
		func(Action) {},
	)
	c.SetFS(&fakeWatchFS{
		dirs: map[string][]string{
			"/cron.d":  {"errors", "latency"},
			"/cron.d/": {"errors", "latency"},
		},
		results: []WatchResultValue{
			{Value: "1", RanAt: "2026-04-25T12:00:00Z"},
			{Value: "5", RanAt: "2026-04-25T12:01:00Z"},
			{Value: "8", RanAt: "2026-04-25T12:02:00Z"},
		},
	})
	c.execFSBuiltin("ls -l /cron.d")
	out := c.scrollback.String()
	for _, want := range []string{"errors", "latency"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing watch %q in:\n%s", want, out)
		}
	}
	// Sparkline glyphs are in the U+2581..U+2588 range; at least one
	// must be present.
	hasSpark := false
	for _, r := range out {
		if r >= 0x2581 && r <= 0x2588 {
			hasSpark = true
			break
		}
	}
	if !hasSpark {
		t.Errorf("expected sparkline glyphs in `ls -l /cron.d` output:\n%s", out)
	}
}

// fakeWatchFS implements FS + WatchResultsProvider for tests.
type fakeWatchFS struct {
	results []WatchResultValue
	dirs    map[string][]string
}

func (f *fakeWatchFS) ReadFile(string) ([]byte, error) { return nil, nil }
func (f *fakeWatchFS) WriteFile(string, []byte) error  { return nil }
func (f *fakeWatchFS) ReadDir(p string) ([]string, error) {
	if f.dirs != nil {
		if entries, ok := f.dirs[p]; ok {
			return entries, nil
		}
	}
	return nil, nil
}
func (f *fakeWatchFS) Remove(string) error { return nil }
func (f *fakeWatchFS) GetWatchResults(string, int) ([]WatchResultValue, error) {
	return f.results, nil
}
