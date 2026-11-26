package shell

import (
	"bytes"
	"context"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/w3ft-net/weft/chart"
)

// rawSeries is a raw-tier series of one untagged run per minute.
func rawSeries(values ...float64) chart.CronSeries {
	start := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s := chart.CronSeries{Granularity: "raw", Start: start, End: start.Add(time.Hour)}
	for i, v := range values {
		s.Buckets = append(s.Buckets, chart.CronBucket{
			Start: start.Add(time.Duration(i) * time.Minute), Count: 1, Sum: v, Min: v, Max: v,
		})
	}
	return s
}

// TestExecNonInteractiveChart is the documented smoke test from the
// shell-charts plan: `weft sh -c "chart errors"` should print a chart
// to stdout and return cleanly without entering any persistent mode.
// The window is the shell's range, handed to the source as typed.
func TestExecNonInteractiveChart(t *testing.T) {
	src := &chartTestFS{series: rawSeries(1, 5, 3, 8, 12)}
	s := &Shell{FS: src, Range: "2h.."}
	var buf bytes.Buffer
	if err := s.ExecNonInteractive("chart errors", &buf); err != nil {
		t.Fatalf("ExecNonInteractive: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "errors") {
		t.Errorf("output missing cron name 'errors':\n%s", out)
	}
	// Time-axis labels: HH:MM colon-separated.
	if !strings.Contains(out, ":") {
		t.Errorf("output missing time-axis colon:\n%s", out)
	}
	if src.gotRange != "2h.." {
		t.Errorf("source got range %q, want the shell's 2h..", src.gotRange)
	}
}

func TestExecNonInteractiveChartSparkline(t *testing.T) {
	s := &Shell{FS: &chartTestFS{series: rawSeries(1, 5, 3)}}
	var buf bytes.Buffer
	if err := s.ExecNonInteractive("chart errors --kind sparkline", &buf); err != nil {
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
	s := &Shell{FS: &chartTestFS{series: rawSeries(5, 8)}}
	var buf bytes.Buffer
	if err := s.ExecNonInteractive("chart errors -f -i 1s", &buf); err != nil {
		t.Fatalf("ExecNonInteractive: %v", err)
	}
	if buf.Len() == 0 {
		t.Error("expected chart output even with --follow flag")
	}
}

func TestExecNonInteractiveChartUnknownWatch(t *testing.T) {
	s := &Shell{FS: &chartTestFS{}} // no points for any name
	var buf bytes.Buffer
	if err := s.ExecNonInteractive("chart ghost", &buf); err != nil {
		t.Fatalf("ExecNonInteractive: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "no numeric results") {
		t.Errorf("expected 'no numeric results' message, got:\n%s", out)
	}
}

// chartTestFS implements FS + chart.CronSource for non-interactive shell
// tests, recording the range it was asked for.
type chartTestFS struct {
	series   chart.CronSeries
	gotRange string
}

func (f *chartTestFS) ReadFile(context.Context, string) ([]byte, error)       { return nil, nil }
func (f *chartTestFS) WriteFile(context.Context, string, []byte) error        { return nil }
func (f *chartTestFS) ReadDir(context.Context, string) ([]fs.DirEntry, error) { return nil, nil }
func (f *chartTestFS) Stat(context.Context, string) (fs.FileInfo, error)      { return nil, fs.ErrNotExist }
func (f *chartTestFS) Remove(context.Context, string) error                   { return nil }
func (f *chartTestFS) GetCronSeries(_ string, rangeSpec string) (chart.CronSeries, error) {
	f.gotRange = rangeSpec
	return f.series, nil
}

// TestExecNonInteractiveChartRejectsBadArgs pins that the one-shot cron form
// refuses what the interactive parser refuses, instead of silently charting
// with defaults.
func TestExecNonInteractiveChartRejectsBadArgs(t *testing.T) {
	cases := map[string]string{
		"chart errors -x 1":          "chart: -x is only for",
		"chart errors --type bar":    "chart: unrecognized option '--type'",
		"chart errors --kind pie":    "invalid argument 'pie' for '--kind'",
		"chart errors -n 20":         "chart: invalid option -- 'n'",
		"chart errors -a":            "option requires an argument -- 'a'",
		"chart errors other":         "unexpected argument",
		"chart errors --kind bar -q": "chart: invalid option -- 'q'",
	}
	for line, want := range cases {
		s := &Shell{FS: &chartTestFS{series: rawSeries(1)}}
		var buf bytes.Buffer
		err := s.ExecNonInteractive(line, &buf)
		if err == nil {
			t.Errorf("%q: want error containing %q, got nil (output %q)", line, want, buf.String())
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %q does not contain %q", line, err, want)
		}
	}
}
