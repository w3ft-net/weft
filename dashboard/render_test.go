package dashboard

import (
	"strings"
	"testing"

	"github.com/kevlarcon/weft/store"
)

func mustParse(t *testing.T, body string) *store.DashboardSpec {
	t.Helper()
	spec, err := store.ParseDashboardYAML(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return spec
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
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")

	// Should have section heading + rule + 2 tile groups (2 lines each: header + body).
	if !strings.Contains(out, "Errors") {
		t.Errorf("missing section title")
	}
	if !strings.Contains(out, "42") {
		t.Errorf("missing first counter value")
	}
	if !strings.Contains(out, "99") {
		t.Errorf("missing second counter value")
	}
	// In single-column mode each tile occupies its own row, so the line
	// containing "42" should not also contain "99".
	for _, ln := range lines {
		if strings.Contains(ln, "42") && strings.Contains(ln, "99") {
			t.Errorf("single-column mode put both values on one line: %q", ln)
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

	// Both values should land on the same row.
	found := false
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "42") && strings.Contains(ln, "99") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected both counter values on one row in wide layout, got:\n%s", out)
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
	// At least one block character should appear.
	if !strings.ContainsAny(out, "▁▂▃▄▅▆▇█") {
		t.Errorf("no block characters in sparkline output:\n%s", out)
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
