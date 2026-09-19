package dashboard

import (
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/store"
)

func mustParse(t *testing.T, body string) *store.DashboardSpec {
	t.Helper()
	spec, err := store.ParseDashboardYAML(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return spec
}

func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		if inEsc {
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		if r == '\x1b' {
			inEsc = true
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func TestRender_SingleColumnFallback(t *testing.T) {
	spec := mustParse(t, `
sections:
  - title: Errors
    tiles:
      - kind: counter
        watch: a
      - kind: counter
        watch: b
`)
	results := map[string]WatchSeries{
		"a": {Values: []float64{42}},
		"b": {Values: []float64{99}},
	}
	out := Render(spec, results, 40) // < threshold

	if !strings.Contains(out, "Errors") {
		t.Errorf("missing section title")
	}
	// Big numbers render as box-drawing glyphs; check watch names are present.
	if !strings.Contains(out, "a") {
		t.Errorf("missing first watch name")
	}
	if !strings.Contains(out, "b") {
		t.Errorf("missing second watch name")
	}
	// Single-column: each tile gets its own box, so "a" and "b" headers
	// should not appear on the same line.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for _, ln := range lines {
		stripped := stripANSI(ln)
		if strings.Contains(stripped, "│a") && strings.Contains(stripped, "│b") {
			t.Errorf("single-column mode put both tiles on one line: %q", ln)
		}
	}
}

func TestRender_MultiColumnWide(t *testing.T) {
	spec := mustParse(t, `
sections:
  - tiles:
      - kind: counter
        watch: a
        size: sm
      - kind: counter
        watch: b
        size: sm
`)
	results := map[string]WatchSeries{
		"a": {Values: []float64{42}},
		"b": {Values: []float64{99}},
	}
	out := Render(spec, results, 120) // wide

	// In wide mode, both tile boxes appear on the same row. The top
	// border of both boxes should be on the same line.
	found := false
	for _, ln := range strings.Split(out, "\n") {
		stripped := stripANSI(ln)
		if strings.Count(stripped, "┌") >= 2 {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected both tile boxes on one row in wide layout, got:\n%s", out)
	}
}

func TestRender_Sparkline(t *testing.T) {
	spec := mustParse(t, `
tiles:
  - kind: sparkline
    watch: traffic
`)
	results := map[string]WatchSeries{
		"traffic": {Values: []float64{1, 5, 3, 8, 2, 7, 4}},
	}
	out := Render(spec, results, 80)
	if !strings.Contains(out, "traffic") {
		t.Errorf("missing watch name in output:\n%s", out)
	}
	// Braille line chart should produce braille characters (U+2800 block).
	hasBraille := false
	for _, r := range out {
		if r >= 0x2800 && r <= 0x28FF {
			hasBraille = true
			break
		}
	}
	if !hasBraille {
		t.Errorf("no braille characters in sparkline output:\n%s", out)
	}
}

func TestRender_SparklineTagged(t *testing.T) {
	spec := mustParse(t, `
tiles:
  - kind: sparkline
    watch: coordinator-mem-pct
    tag: .coordinator_id
    y: .mem_pct
    size: lg
`)
	results := map[string]WatchSeries{
		"coordinator-mem-pct": {
			Tags: map[string]WatchSeries{
				"i-aaa": {Values: []float64{48, 44, 40}, Times: []float64{300, 200, 100}},
				"i-bbb": {Values: []float64{68, 64, 60}, Times: []float64{300, 200, 100}},
			},
		},
	}
	out := Render(spec, results, 80)
	plain := stripANSI(out)
	for _, want := range []string{"i-aaa", "i-bbb"} {
		if !strings.Contains(plain, want) {
			t.Errorf("missing tag legend %q in output:\n%s", want, plain)
		}
	}
	hasBraille := false
	for _, r := range plain {
		if r >= 0x2800 && r <= 0x28FF {
			hasBraille = true
			break
		}
	}
	if !hasBraille {
		t.Errorf("no braille characters in tagged sparkline output:\n%s", plain)
	}
}

func TestRender_SparklineTaggedMissingData(t *testing.T) {
	spec := mustParse(t, `
tiles:
  - kind: sparkline
    watch: ghost
    tag: .coordinator_id
    y: .mem_pct
`)
	out := Render(spec, map[string]WatchSeries{}, 80)
	if !strings.Contains(out, "<no data>") {
		t.Errorf("missing placeholder for absent tagged watch:\n%s", out)
	}
}

func TestRender_Table(t *testing.T) {
	spec := mustParse(t, `
tiles:
  - kind: table
    watch: top
    columns: [host, count]
`)
	results := map[string]WatchSeries{
		"top": {LatestText: "alpha 12\nbeta 7\ngamma 3\n"},
	}
	out := Render(spec, results, 80)
	for _, want := range []string{"host", "count", "alpha", "beta", "gamma"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
}

func TestRender_MissingWatchPlaceholder(t *testing.T) {
	spec := mustParse(t, `
tiles:
  - kind: counter
    watch: ghost
`)
	out := Render(spec, map[string]WatchSeries{}, 80)
	if !strings.Contains(out, "<no data>") {
		t.Errorf("missing placeholder for absent watch:\n%s", out)
	}
}

func TestRender_GaugeWithThreshold(t *testing.T) {
	body := `tiles:
  - kind: gauge
    watch: cpu
    threshold: { warn: 70, crit: 90, max: 100 }
`
	spec := mustParse(t, body)
	results := map[string]WatchSeries{
		"cpu": {Values: []float64{95}},
	}
	out := Render(spec, results, 80)
	if !strings.Contains(out, "95") || !strings.Contains(out, "100") {
		t.Errorf("gauge output missing numbers:\n%s", out)
	}
	// Should have ANSI red for crit.
	if !strings.Contains(out, "\x1b[38;5;1m") {
		t.Errorf("expected crit color in output:\n%s", out)
	}
}

func TestRender_SparklineTimeAxis(t *testing.T) {
	spec := mustParse(t, `
tiles:
  - kind: sparkline
    watch: mem
`)
	base := 1746550800.0 // 2025-05-06 17:00:00 UTC
	results := map[string]WatchSeries{
		"mem": {
			Values: []float64{80, 75, 70, 65, 60, 55, 50}, // newest-first
			Times:  []float64{base + 360, base + 300, base + 240, base + 180, base + 120, base + 60, base},
		},
	}
	out := stripANSI(Render(spec, results, 80))
	if !strings.Contains(out, ":") {
		t.Errorf("expected time-formatted x-axis labels (HH:MM), got:\n%s", out)
	}
	if strings.Contains(out, "5.00") || strings.Contains(out, "10.00") {
		t.Errorf("x-axis still shows index numbers instead of times:\n%s", out)
	}
}

func TestRender_DashboardTitleAndSectionTitles(t *testing.T) {
	spec := mustParse(t, `
title: Production
sections:
  - title: Errors
    tiles:
      - kind: counter
        watch: a
  - title: Capacity
    tiles:
      - kind: counter
        watch: b
`)
	out := Render(spec, map[string]WatchSeries{
		"a": {Values: []float64{1}},
		"b": {Values: []float64{2}},
	}, 80)

	for _, want := range []string{"Production", "Errors", "Capacity"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}
