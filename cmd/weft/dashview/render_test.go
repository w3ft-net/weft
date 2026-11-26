package dashview

import (
	"strings"
	"testing"

	weftchart "github.com/w3ft-net/weft/chart"
	dspec "github.com/w3ft-net/weft/dashboard"
)

func mustParse(t *testing.T, body string) *dspec.Spec {
	t.Helper()
	spec, err := dspec.Parse(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return spec
}

// series is a cron's stored series: one raw point per value, a minute apart,
// oldest first.
func series(vals ...float64) WatchSeries {
	ws := WatchSeries{Series: weftchart.CronSeries{Granularity: "raw"}}
	for i, v := range vals {
		ws.Series.Buckets = append(ws.Series.Buckets, bucket(i, "", v))
	}
	return ws
}

// latest is a cron's latest untagged run.
func latest(v float64) WatchSeries {
	return WatchSeries{Latest: []weftchart.LatestPoint{{Value: &v}}}
}

// key is where a tile on cron, with no range of its own, finds its data.
func key(cron string) string { return resultKey(dspec.Tile{Watch: cron}) }

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
        cron: a
      - kind: counter
        cron: b
`)
	results := map[string]WatchSeries{
		key("a"): latest(42),
		key("b"): latest(99),
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
        cron: a
        size: sm
      - kind: counter
        cron: b
        size: sm
`)
	results := map[string]WatchSeries{
		key("a"): latest(42),
		key("b"): latest(99),
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
    cron: traffic
`)
	results := map[string]WatchSeries{
		key("traffic"): series(1, 5, 3, 8, 2, 7, 4),
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
    cron: coordinator-mem-pct
    size: lg
`)
	results := map[string]WatchSeries{
		key("coordinator-mem-pct"): {Series: weftchart.CronSeries{Granularity: "raw", Buckets: []weftchart.CronBucket{
			bucket(0, "i-aaa", 40), bucket(0, "i-bbb", 60),
			bucket(1, "i-aaa", 44), bucket(1, "i-bbb", 64),
			bucket(2, "i-aaa", 48), bucket(2, "i-bbb", 68),
		}}},
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
    cron: ghost
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
    cron: top
`)
	num := func(v float64) *float64 { return &v }
	text := func(s string) *string { return &s }
	results := map[string]WatchSeries{
		key("top"): {Latest: []weftchart.LatestPoint{
			{Tag: "alpha", Value: num(12)}, {Tag: "beta", Value: num(7)}, {Tag: "gamma", Text: text("draining")},
		}},
	}
	out := stripANSI(Render(spec, results, 80))
	for _, want := range []string{"tag", "value", "alpha", "12", "beta", "7", "gamma", "draining"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
	// Rows keep the server's table order.
	if strings.Index(out, "alpha") > strings.Index(out, "beta") {
		t.Errorf("rows reordered:\n%s", out)
	}
}

func TestRender_MissingWatchPlaceholder(t *testing.T) {
	spec := mustParse(t, `
tiles:
  - kind: counter
    cron: ghost
`)
	out := Render(spec, map[string]WatchSeries{}, 80)
	if !strings.Contains(out, "<no data>") {
		t.Errorf("missing placeholder for absent watch:\n%s", out)
	}
}

// A gauge turns red when the cron's alert cutoff is breached, and only then;
// the tile has no threshold of its own.
func TestRender_GaugeCriticalFromCronAlert(t *testing.T) {
	spec := mustParse(t, "tiles:\n  - kind: gauge\n    cron: cpu\n    max: 100\n")
	for _, c := range []struct {
		value float64
		red   bool
	}{{95, true}, {50, false}} {
		ws := latest(c.value)
		ws.Series.Alert = &weftchart.CronAlert{Condition: "above", Threshold: 90}
		out := Render(spec, map[string]WatchSeries{key("cpu"): ws}, 80)
		if !strings.Contains(out, "100") {
			t.Errorf("gauge output missing its scale:\n%s", out)
		}
		if got := strings.Contains(out, "\x1b[38;5;1m"); got != c.red {
			t.Errorf("value %v: red = %v, want %v:\n%s", c.value, got, c.red, out)
		}
	}
}
