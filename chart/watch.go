package chart

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kevlarcon/warp/pkg/shellcore/chart"
)

// RenderOneShot fetches the latest results for the named watch and
// renders one ANSI frame sized to the given footprint. Both the
// interactive shell (watch-form chart command) and the
// non-interactive runner (weft sh -c "chart …") call this — the
// difference is only how the returned string is delivered to the
// terminal.
//
// kind is "line", "sparkline", or "bar"; limit caps how many recent
// results are charted; (w, h) defines the render footprint.
// Returns (output, errMsg). errMsg is empty on success.
func RenderOneShot(wr WatchResultsProvider, name, kind string, limit, w, h int) (string, string) {
	results, err := wr.GetWatchResults(name, limit)
	if err != nil {
		return "", err.Error()
	}
	pts, _ := watchResultsToPoints(results)
	if len(pts) == 0 {
		return "", fmt.Sprintf("no numeric results for watch %q", name)
	}
	title := fmt.Sprintf("%s  (last %d)", name, len(pts))

	switch kind {
	case "sparkline":
		values := make([]float64, len(pts))
		for i, p := range pts {
			values[i] = p.Y
		}
		return chart.Sparkline(values) + "\n", ""
	case "bar":
		labels := make([]string, len(pts))
		vals := make([]float64, len(pts))
		for i, p := range pts {
			labels[i] = formatChartTime(p.X)
			vals[i] = p.Y
		}
		s, err := chart.Bar(w, h).Title(title).BarValues(labels, vals).Render()
		if err != nil {
			return "", err.Error()
		}
		return s, ""
	default: // line
		s, err := chart.Line(w, h).Title(title).XAxisTime().Points(pts).Render()
		if err != nil {
			return "", err.Error()
		}
		return s, ""
	}
}

// watchResultsToPoints maps a slice of WatchResultValue into
// chart.Point data. The numeric value is the *last
// whitespace-separated field* of the trimmed stdout — that matches
// the multi-row alert convention (`-k N` where field N is the
// value) for the common case where N is the last column. Single
// scalar values are unaffected. The X coordinate is the RanAt
// timestamp parsed as RFC3339; rows that fail to parse are
// skipped.
//
// Results come in newest-first; the returned slice is oldest-first
// so the chart reads left-to-right as time-forward.
func watchResultsToPoints(results []WatchResultValue) ([]chart.Point, int) {
	pts := make([]chart.Point, 0, len(results))
	skipped := 0
	for _, r := range results {
		t, err := time.Parse(time.RFC3339, r.RanAt)
		if err != nil {
			skipped++
			continue
		}
		val := strings.TrimSpace(r.Value)
		if val == "" {
			skipped++
			continue
		}
		fields := strings.Fields(val)
		if len(fields) == 0 {
			skipped++
			continue
		}
		f, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			skipped++
			continue
		}
		pts = append(pts, chart.Point{X: float64(t.Unix()), Y: f})
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].X < pts[j].X })
	return pts, skipped
}

// formatChartTime is the bar-mode X-axis label formatter — uses
// the same HH:MM convention as the line-chart time axis so
// visually-similar data shows visually-similar labels.
func formatChartTime(epochSecs float64) string {
	t := time.Unix(int64(epochSecs), 0)
	if t.Second() == 0 {
		return t.Format("15:04")
	}
	return t.Format("15:04:05")
}
