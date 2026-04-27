package chart

import (
	"strings"
	"testing"
	"time"
)

func TestParseChartCmd(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want WatchArgs
		err  string // substring match; "" = expect no error
	}{
		{
			name: "bare watch name",
			argv: []string{"errors"},
			want: WatchArgs{Name: "errors", Kind: "line", Limit: 50, Interval: time.Second},
		},
		{
			name: "path form strips /cron.d/",
			argv: []string{"/cron.d/errors"},
			want: WatchArgs{Name: "errors", Kind: "line", Limit: 50, Interval: time.Second},
		},
		{
			name: "type bar",
			argv: []string{"errors", "-t", "bar"},
			want: WatchArgs{Name: "errors", Kind: "bar", Limit: 50, Interval: time.Second},
		},
		{
			name: "limit + interval + follow",
			argv: []string{"errors", "-n", "20", "-i", "500ms", "-f"},
			want: WatchArgs{Name: "errors", Kind: "line", Limit: 20, Interval: 500 * time.Millisecond, Follow: true},
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
			got, errMsg := ParseWatchCmd(tc.argv)
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
