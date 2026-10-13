package chart

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParsePipeArgs_XCasts(t *testing.T) {
	got, msg := parsePipeArgs([]string{"-x", "2::time"})
	if msg != "" || !got.XTime || got.XFormat != "" || got.XSpec.Index != 2 || got.XEnd != 0 {
		t.Errorf("-x 2::time: got %+v %q", got, msg)
	}
	got, msg = parsePipeArgs([]string{"-x", "1-2::time(%Y/%m/%d %H:%M:%S)"})
	if msg != "" || !got.XTime || got.XFormat != "%Y/%m/%d %H:%M:%S" || got.XSpec.Index != 1 || got.XEnd != 2 {
		t.Errorf("-x 1-2::time(fmt): got %+v %q", got, msg)
	}
	got, msg = parsePipeArgs([]string{"-x", ".ts::time(%s)"})
	if msg != "" || !got.XTime || got.XFormat != "%s" || got.XSpec.Path != "ts" {
		t.Errorf("-x .ts::time(%%s): got %+v %q", got, msg)
	}
	// A dash inside a JSON path is a key, not a column range.
	got, msg = parsePipeArgs([]string{"-x", ".a-b"})
	if msg != "" || got.XSpec.Path != "a-b" || got.XEnd != 0 {
		t.Errorf("-x .a-b: got %+v %q", got, msg)
	}
}

func TestParsePipeArgs_XCastErrors(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"-x", "2::foo"}, `unknown cast "foo"`},
		{[]string{"-x", "2::time(%Y"}, `unknown cast "time(%Y"`},
		{[]string{"-x", "2::time()"}, "empty ::time() format"},
		{[]string{"-x", "1-2"}, "column range needs a ::time cast"},
		{[]string{"-x", "2-1::time"}, "range end must be after its start"},
		{[]string{"-y", "1::time"}, "casts are only supported on -x"},
		{[]string{"-y", ".v::time"}, "casts are only supported on -x"},
		{[]string{"--tag", ".h::time"}, "casts are only supported on -x"},
	}
	for _, tc := range cases {
		if _, msg := parsePipeArgs(tc.args); !strings.Contains(msg, tc.want) {
			t.Errorf("%v: got %q, want it to mention %q", tc.args, msg, tc.want)
		}
	}
}

// Without a cast, only unmistakable timestamps (RFC3339) make a time
// axis; bare numbers stay numbers. ::time opts a number into epoch.
func TestPlotFromStream_TimeAxisDetection(t *testing.T) {
	cases := []struct {
		x     string
		lines []string
		want  bool
	}{
		{"1", []string{"1736000000 5", "1736000060 6"}, false},
		{"1::time", []string{"1736000000 5", "1736000060 6"}, true},
		{"1", []string{"2026-01-01T00:00:00Z 5", "2026-01-01T00:01:00Z 6"}, true},
	}
	for _, tc := range cases {
		args, msg := parsePipeArgs([]string{"-x", tc.x, "-t", "line"})
		if msg != "" {
			t.Fatal(msg)
		}
		plot, msg := PlotFromStream(tc.lines, args)
		if msg != "" {
			t.Fatalf("-x %s: %s", tc.x, msg)
		}
		if plot.TimeX != tc.want {
			t.Errorf("-x %s over %q: TimeX=%v, want %v", tc.x, tc.lines[0], plot.TimeX, tc.want)
		}
	}
}

// The api log's stamp spans two whitespace fields; a column range plus
// a strftime format reads it. Zone-less stamps are UTC; bucket labels
// render in local time like the line chart's axis.
func TestPlotFromStream_FormatCastOverColumnRange(t *testing.T) {
	defer func(l *time.Location) { time.Local = l }(time.Local)
	time.Local = time.FixedZone("EST", -5*3600)

	lines := []string{
		"2026/10/13 15:04:05 GET /a",
		"2026/10/13 15:04:59 GET /b",
		"2026/10/13 15:05:10 GET /c",
	}
	args, msg := parsePipeArgs([]string{"-x", "1-2::time(%Y/%m/%d %H:%M:%S)", "--bucket", "1m", "-t", "bar"})
	if msg != "" {
		t.Fatal(msg)
	}
	plot, msg := PlotFromStream(lines, args)
	if msg != "" {
		t.Fatal(msg)
	}
	pts := plot.Series[0].Points
	want := time.Date(2026, 10, 13, 15, 4, 0, 0, time.UTC)
	if !plot.TimeX || len(pts) != 2 || pts[0].X != float64(want.Unix()) || pts[0].Y != 2 || pts[1].Y != 1 {
		t.Fatalf("got %+v, want 15:04 UTC=2, 15:05=1", plot)
	}
	if pts[0].Label != "10:04" {
		t.Errorf("label: got %q, want local 10:04", pts[0].Label)
	}

	_, msg = PlotFromStream(append(lines, "Oct 13 15:06:00 GET /d"), args)
	if !strings.Contains(msg, `"Oct 13" does not match %Y/%m/%d %H:%M:%S`) {
		t.Errorf("mismatched row: got %q", msg)
	}
}

// A minute with no rows draws as 0 for count/sum, so a gap in the logs
// is a hole in the chart rather than a line bridging across it.
func TestPlotFromStream_ZeroFillsEmptyBuckets(t *testing.T) {
	lines := []string{"2026-10-13T15:00:10Z 4", "2026-10-13T15:03:20Z 6"}
	for agg, want := range map[string][]float64{
		"count": {1, 0, 0, 1},
		"sum":   {4, 0, 0, 6},
		"avg":   {4, 6}, // no meaningful zero: empty buckets stay out
	} {
		args, _ := parsePipeArgs([]string{"-x", "1", "-y", "2", "--bucket", "1m", "-a", agg})
		plot, msg := PlotFromStream(lines, args)
		if msg != "" {
			t.Fatal(msg)
		}
		var got []float64
		for _, p := range plot.Series[0].Points {
			got = append(got, p.Y)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("-a %s: got %v, want %v", agg, got, want)
		}
	}
}

// Tagged series fill over the whole chart's span, so a host that went
// quiet before another started still shows its zeros.
func TestPlotFromStream_ZeroFillsTaggedOverSharedSpan(t *testing.T) {
	lines := []string{
		`{"ts":"2026-10-13T15:00:00Z","h":"a"}`,
		`{"ts":"2026-10-13T15:02:00Z","h":"b"}`,
	}
	args, _ := parsePipeArgs([]string{"-x", ".ts", "--tag", ".h", "--bucket", "1m"})
	plot, msg := PlotFromStream(lines, args)
	if msg != "" {
		t.Fatal(msg)
	}
	for _, s := range plot.Series {
		if len(s.Points) != 3 {
			t.Errorf("series %s: %d points, want 3 (15:00..15:02)", s.Tag, len(s.Points))
		}
	}
}

func TestPlotFromStream_ZeroFillBound(t *testing.T) {
	lines := []string{"2025-10-13T00:00:00Z 1", "2026-10-13T00:00:00Z 1"}
	args, _ := parsePipeArgs([]string{"-x", "1", "--bucket", "1s"})
	if _, msg := PlotFromStream(lines, args); !strings.Contains(msg, "use a wider --bucket") {
		t.Errorf("a year of 1s buckets: got %q", msg)
	}
}

// Bucket labels drop the date under an hour ("15:04"), so grouping on
// the label would merge the same minute of two days — every rolling
// 24h range spans two. Time buckets group on their instant.
func TestPlotFromStream_SameClockTimeOnTwoDaysStaysApart(t *testing.T) {
	stamps := []string{"2026-10-12T15:04:00Z", "2026-10-13T15:04:00Z", "2026-10-13T15:04:30Z"}
	var plain, tagged []string
	for _, ts := range stamps {
		plain = append(plain, ts+" 1")
		tagged = append(tagged, fmt.Sprintf(`{"ts":%q,"h":"a","v":1}`, ts))
	}
	// -a max: no zero-fill, so only the real buckets appear.
	for _, tc := range []struct {
		flags []string
		lines []string
	}{
		{[]string{"-x", "1", "-y", "2", "--bucket", "1m", "-a", "max"}, plain},
		{[]string{"-x", ".ts", "-y", ".v", "--tag", ".h", "--bucket", "1m", "-a", "max"}, tagged},
	} {
		args, msg := parsePipeArgs(tc.flags)
		if msg != "" {
			t.Fatal(msg)
		}
		plot, msg := PlotFromStream(tc.lines, args)
		if msg != "" {
			t.Fatal(msg)
		}
		if n := len(plot.Series[0].Points); n != 2 {
			t.Errorf("%v: %d buckets, want 2 (one per day)", tc.flags, n)
		}
	}
}
