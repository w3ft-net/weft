package chart

import (
	"math"
	"strings"
	"testing"
)

// TestSparklineKnownExamples mirrors termichart's own sparkline
// regression suite — same inputs, same expected glyph runs. If this
// fails after a refactor, the rendering math diverged from the
// reference implementation.
func TestSparklineKnownExamples(t *testing.T) {
	cases := []struct {
		name string
		in   []float64
		want string
	}{
		{"empty", nil, ""},
		{"single", []float64{42}, "▁"},
		{"all-same", []float64{5, 5, 5}, "▁▁▁"},
		{"ascending", []float64{0, 1, 2, 3, 4, 5, 6, 7}, "▁▂▃▄▅▆▇█"},
		{"descending", []float64{7, 6, 5, 4, 3, 2, 1, 0}, "█▇▆▅▄▃▂▁"},
		{"known", []float64{1, 3, 5, 2, 8}, "▁▃▅▂█"},
		{"negatives", []float64{-3, -1, 0, 2}, "▁▄▅█"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Sparkline(tc.in)
			if got != tc.want {
				t.Errorf("Sparkline(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSparklineColored(t *testing.T) {
	s := SparklineColored([]float64{1, 5, 3})
	if !strings.Contains(s, "\x1b[38;5;2m") {
		t.Error("expected green ANSI for above-mean values")
	}
	if !strings.Contains(s, "\x1b[38;5;1m") {
		t.Error("expected red ANSI for below-mean values")
	}
	if !strings.Contains(s, "\x1b[0m") {
		t.Error("expected reset between cells")
	}
}

func TestNiceTicks(t *testing.T) {
	cases := []struct {
		min, max float64
		max_t    int
		min_n    int // expect at least this many ticks
		max_n    int // ...at most this many
	}{
		{0, 100, 10, 5, 12},
		{0, 1, 5, 3, 8},
		{-50, 50, 8, 3, 12},
	}
	for _, tc := range cases {
		got := niceTicks(tc.min, tc.max, tc.max_t)
		if len(got) < tc.min_n || len(got) > tc.max_n {
			t.Errorf("niceTicks(%v,%v,%v) returned %d ticks, want %d..%d",
				tc.min, tc.max, tc.max_t, len(got), tc.min_n, tc.max_n)
		}
		// Ensure ticks lie within range (with a tiny tolerance for
		// the inclusive upper-bound rounding).
		for _, v := range got {
			if v < tc.min-1e-9 || v > tc.max+1e-9 {
				t.Errorf("niceTicks tick %v out of [%v,%v]", v, tc.min, tc.max)
			}
		}
	}
}

func TestNiceTicksDegenerate(t *testing.T) {
	if got := niceTicks(0, 0, 5); got != nil {
		t.Errorf("zero span: got %v, want nil", got)
	}
	if got := niceTicks(0, math.Inf(1), 5); got != nil {
		t.Errorf("inf span: got %v, want nil", got)
	}
	if got := niceTicks(0, 100, 1); got != nil {
		t.Errorf("max_ticks<2: got %v, want nil", got)
	}
}

// TestLineChartRenders verifies the line-chart pipeline produces
// non-empty output with axis labels and at least one braille glyph.
// Pure smoke — no golden file yet (added in a follow-up commit).
func TestLineChartRenders(t *testing.T) {
	pts := make([]Point, 20)
	for i := range pts {
		pts[i] = Point{X: float64(i), Y: math.Sin(float64(i)*0.3) * 10}
	}
	out, err := Line(60, 14).Title("sine").Points(pts).Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if out == "" {
		t.Fatal("empty render")
	}
	plain := stripANSILocal(out)
	// Title shows up.
	if !strings.Contains(plain, "sine") {
		t.Error("title missing from rendered output")
	}
	// At least one braille glyph (U+2800 .. U+28FF) painted.
	hasBraille := false
	for _, r := range plain {
		if r >= 0x2800 && r <= 0x28FF {
			hasBraille = true
			break
		}
	}
	if !hasBraille {
		t.Error("no braille glyphs rendered for line chart")
	}
}

func TestLineChartTooFewPoints(t *testing.T) {
	out, err := Line(60, 14).Points([]Point{{0, 1}}).Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	plain := stripANSILocal(out)
	if !strings.Contains(plain, "1.0000") && !strings.Contains(plain, "1.00") {
		t.Errorf("single-point render missing centered value, got:\n%s", plain)
	}
}

func TestBarChartRenders(t *testing.T) {
	out, err := Bar(60, 14).
		Title("by host").
		BarValues([]string{"alpha", "bravo", "charlie"}, []float64{12, 8, 17}).
		Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	plain := stripANSILocal(out)
	for _, want := range []string{"by host", "alpha", "bravo", "charlie"} {
		if !strings.Contains(plain, want) {
			t.Errorf("missing %q in:\n%s", want, plain)
		}
	}
	if !strings.Contains(plain, "█") {
		t.Error("bar chart missing block characters")
	}
}

func TestRenderEmpty(t *testing.T) {
	out, err := Line(0, 0).Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if out != "" {
		t.Errorf("expected empty for zero-size chart, got %q", out)
	}
}

// TestRenderHeightFits is the property the user named: charts always
// fit the terminal. Render output should never exceed h+1 newlines for
// (w, h) input — h rows of content plus one trailing newline.
func TestRenderHeightFits(t *testing.T) {
	pts := []Point{{0, 1}, {1, 5}, {2, 3}, {3, 7}}
	for _, h := range []int{6, 12, 24} {
		out, err := Line(80, h).Points(pts).Render()
		if err != nil {
			t.Fatal(err)
		}
		got := strings.Count(out, "\n")
		if got != h {
			t.Errorf("h=%d: got %d newlines, want %d", h, got, h)
		}
	}
}
