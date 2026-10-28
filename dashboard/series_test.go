package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/chart"
)

func bucket(min int, tag string, v float64) chart.CronBucket {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return chart.CronBucket{Start: t0.Add(time.Duration(min) * time.Minute), Tag: tag, Count: 1, Sum: v, Min: v, Max: v}
}

// A series tile reduces the cron's stored points with its own aggregate and
// bucket, as `chart <cron> -a max --bucket 1h` would.
func TestRender_SeriesTileAppliesAggregateAndBucket(t *testing.T) {
	spec := mustParse(t, `
tiles:
  - kind: bar
    cron: errs
    aggregate: max
    bucket: 1h
`)
	ws := series(1, 9, 3, 2)
	out := stripANSI(Render(spec, map[string]WatchSeries{key("errs"): ws}, 80))
	if !strings.Contains(out, "9") {
		t.Errorf("max over the hour should draw 9:\n%s", out)
	}
	if strings.Contains(out, "15") {
		t.Errorf("avg of the hour would be 3.75, not drawn:\n%s", out)
	}
}

// A bar draws one series, so a tagged cron says so rather than drawing nothing.
func TestRender_BarTileOnTaggedCron(t *testing.T) {
	spec := mustParse(t, `
tiles:
  - kind: bar
    cron: mem
`)
	ws := WatchSeries{Series: chart.CronSeries{Granularity: "raw", Buckets: []chart.CronBucket{bucket(0, "a", 1), bucket(0, "b", 2)}}}
	out := stripANSI(Render(spec, map[string]WatchSeries{key("mem"): ws}, 80))
	if !strings.Contains(out, "bar chart draws one series") {
		t.Errorf("expected the chart's own bar error:\n%s", out)
	}
}

type rangeProvider struct {
	Provider
	asked []string
}

func (p *rangeProvider) GetCronSeries(name, rangeSpec string) (chart.CronSeries, error) {
	p.asked = append(p.asked, name+" "+rangeSpec)
	return chart.CronSeries{}, nil
}

func (p *rangeProvider) GetLatestRun(string) ([]chart.LatestPoint, error) { return nil, nil }

// Each tile reads its cron over its own range, and tiles that share both
// share one read.
func TestFetchAllSeries_ReadsEachTilesRange(t *testing.T) {
	spec := mustParse(t, `
tiles:
  - {kind: line, cron: errs}
  - {kind: bar, cron: errs, range: 7d..}
  - {kind: sparkline, cron: errs, range: 7d..}
  - {kind: counter, cron: errs}
`)
	p := &rangeProvider{}
	got := fetchAllSeries(p, spec)
	if want := []string{"errs 24h..", "errs 7d.."}; strings.Join(p.asked, ",") != strings.Join(want, ",") {
		t.Errorf("reads = %v, want %v", p.asked, want)
	}
	if len(got) != 2 {
		t.Errorf("results = %d entries, want one per cron and range", len(got))
	}
}

// A counter with an aggregate reads that aggregate over its range, not the
// cron's latest run, and turns red when it breaches the cron's alert.
func TestRender_WindowedCounter(t *testing.T) {
	spec := mustParse(t, "tiles:\n  - kind: counter\n    cron: errs\n    aggregate: max\n    range: 1h..\n")
	ws := series(1, 9, 3)
	l := 3.0
	ws.Latest = []chart.LatestPoint{{Value: &l}}
	ws.Series.Alert = &chart.CronAlert{Condition: "above", Threshold: 5}
	// The 9 shows as a box-drawing number; the latest run's 3 would not
	// be red.
	out := Render(spec, map[string]WatchSeries{resultKey(spec.Sections[0].Tiles[0]): ws}, 80)
	if !strings.Contains(out, "\x1b[38;5;1m") {
		t.Errorf("max 9 breaches above 5, want red:\n%s", out)
	}
	ws.Series.Alert = nil
	if out := Render(spec, map[string]WatchSeries{resultKey(spec.Sections[0].Tiles[0]): ws}, 80); strings.Contains(out, "\x1b[38;5;1m") {
		t.Errorf("no alert, nothing is critical:\n%s", out)
	}
}

// A table with a range has one row per tag: that tag's aggregate over the
// window, highest first, and the rows past the cron's alert cutoff are red.
func TestRender_WindowedTable(t *testing.T) {
	spec := mustParse(t, "tiles:\n  - kind: table\n    cron: mem\n    aggregate: sum\n    range: 1h..\n")
	ws := WatchSeries{Series: chart.CronSeries{Granularity: "raw", Buckets: []chart.CronBucket{
		bucket(0, "a", 1), bucket(1, "a", 2), bucket(0, "b", 10),
	}, Alert: &chart.CronAlert{Condition: "above", Threshold: 5}}}
	raw := Render(spec, map[string]WatchSeries{resultKey(spec.Sections[0].Tiles[0]): ws}, 80)
	out := stripANSI(raw)
	if strings.Index(out, "b ") > strings.Index(out, "a ") || !strings.Contains(out, "10") || !strings.Contains(out, "3") {
		t.Errorf("want b (10) above a (3):\n%s", out)
	}
	if n := strings.Count(raw, "\x1b[38;5;1m"); n != 1 {
		t.Errorf("only b breaches 5, red rows = %d:\n%s", n, raw)
	}
}
