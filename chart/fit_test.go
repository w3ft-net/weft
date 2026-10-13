package chart

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*[mK]")

// minuteRows is one "<HHMM> <value>" row per minute of a day, value 50
// except the last minute, which spikes to 1000.
func minuteRows() []string {
	var lines []string
	for m := 0; m < 24*60; m++ {
		v := 50
		if m == 24*60-1 {
			v = 1000
		}
		lines = append(lines, fmt.Sprintf("%02d%02d %d", m/60, m%60, v))
	}
	return lines
}

func visibleRows(out string) []string {
	return strings.Split(strings.TrimSuffix(ansiRE.ReplaceAllString(out, ""), "\n"), "\n")
}

// A bar chart with more rows than the terminal has columns must bin
// them, not clip the tail: the last row's spike has to be drawn, near
// the right edge, and every output row must fit the terminal.
func TestRenderFromStream_BarBinsRowsToFitWidth(t *testing.T) {
	args, _ := parsePipeArgs([]string{"-a", "max"})
	const w, h = 100, 30
	out, msg := RenderFromStream(minuteRows(), args, w, h)
	if msg != "" {
		t.Fatal(msg)
	}
	rows := visibleRows(out)
	if len(rows) > h {
		t.Errorf("rendered %d rows, terminal has %d; last %q", len(rows), h, rows[len(rows)-1])
	}
	tallest := -1
	for _, r := range rows {
		if utf8.RuneCountInString(r) > w {
			t.Errorf("row wider than %d: %q", w, r)
		}
		if i := strings.Index(r, "█"); i >= 0 && tallest < 0 {
			tallest = utf8.RuneCountInString(r[:i])
		}
	}
	if tallest < w*9/10 {
		t.Errorf("tallest bar at column %d, want the last-row spike in the right 10%% of %d\n%s", tallest, w, strings.Join(rows, "\n"))
	}
	if !strings.Contains(out, "1440 points → 90 bars, 16 per bar (max)") {
		t.Errorf("want a footer saying the rows were binned\n%s", strings.Join(rows, "\n"))
	}
}

// Binning re-reduces the raw rows with the chart's aggregate: count
// adds up, identity (no aggregate) averages.
func TestFitGroups_ReReducesRawRows(t *testing.T) {
	lines := []string{"a 1", "a 1", "b 1", "c 1", "c 1", "c 1", "d 1"}
	args, _ := parsePipeArgs([]string{"-x", "1", "-a", "count"})
	groups, ordered, _, _ := groupRows(lines, args)
	pts, per := fitPoints(groups, ordered, args.Aggregate, 2)
	if per != 2 {
		t.Fatalf("per-bar: got %d, want 2", per)
	}
	if len(pts) != 2 || pts[0].Y != 3 || pts[1].Y != 4 || pts[0].Label != "a" || pts[1].Label != "c" {
		t.Errorf("count bins: got %+v, want a=3 c=4", pts)
	}

	args, _ = parsePipeArgs(nil)
	groups, ordered, _, _ = groupRows([]string{"2", "4", "10", "20", "7"}, args)
	pts, _ = fitPoints(groups, ordered, args.Aggregate, 3)
	want := []float64{3, 15, 7}
	for i, p := range pts {
		if p.Y != want[i] {
			t.Errorf("identity bin %d: got %v, want %v (avg)", i, p.Y, want[i])
		}
	}
}

func TestRenderFromStream_SparklineFitsWidth(t *testing.T) {
	args, _ := parsePipeArgs([]string{"-t", "sparkline"})
	out, msg := RenderFromStream(minuteRows(), args, 80, 24)
	if msg != "" {
		t.Fatal(msg)
	}
	line := visibleRows(out)[0]
	if n := utf8.RuneCountInString(line); n > 80 {
		t.Errorf("sparkline is %d cells wide, terminal is 80", n)
	}
	if !strings.HasSuffix(line, "█") {
		t.Errorf("last-row spike missing from sparkline tail: %q", line)
	}
}

// `uniq -c` over HHMM minutes, charted -x 2 -y 1: the HHMM column is a
// plain number, so bars are labelled with it, not as 1970 timestamps.
func TestRenderFromStream_HHMMLabelsAreNotEpoch(t *testing.T) {
	var lines []string
	for m := 0; m < 24*60; m++ {
		lines = append(lines, fmt.Sprintf("%7d %02d%02d", 40, m/60, m%60))
	}
	args, _ := parsePipeArgs([]string{"-x", "2", "-y", "1", "-a", "min"})
	out, msg := RenderFromStream(lines, args, 100, 20)
	if msg != "" {
		t.Fatal(msg)
	}
	rows := visibleRows(out)
	axis := rows[len(rows)-2]
	if strings.Contains(axis, ":") || !strings.HasPrefix(strings.TrimSpace(axis), "0000") {
		t.Errorf("want HHMM labels, got %q", axis)
	}
}
