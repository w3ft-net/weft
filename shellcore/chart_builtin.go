package shellcore

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kevlarcon/weft/shellcore/chart"
)

// chartArgs holds the parsed `chart <name> [...]` invocation.
type chartArgs struct {
	name     string
	kind     string        // "line", "sparkline", "bar"; "" = auto
	limit    int           // results to fetch (default 50)
	interval time.Duration // poll interval in --follow mode (default 1s)
	follow   bool
}

// parseChartCmd parses a tokenised `chart …` command line into chartArgs.
// The first non-flag token is the watch name (bare or `/cron.d/<name>`).
// Returns an error message when the user types something we don't grok —
// the message is what shows up in the shell, so it should be terse.
func parseChartCmd(args []string) (chartArgs, string) {
	out := chartArgs{limit: 50, interval: time.Second}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-t" || a == "--type":
			if i+1 >= len(args) {
				return out, "chart: -t requires a value (line, sparkline, bar)"
			}
			out.kind = args[i+1]
			i++
		case a == "-n" || a == "--limit":
			if i+1 >= len(args) {
				return out, "chart: -n requires a number"
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n <= 0 {
				return out, "chart: -n must be a positive integer"
			}
			out.limit = n
			i++
		case a == "-i" || a == "--interval":
			if i+1 >= len(args) {
				return out, "chart: -i requires a duration (e.g. 1s, 500ms)"
			}
			d, err := time.ParseDuration(args[i+1])
			if err != nil || d < 100*time.Millisecond {
				return out, "chart: -i must be a duration ≥ 100ms"
			}
			out.interval = d
			i++
		case a == "-f" || a == "--follow":
			out.follow = true
		case strings.HasPrefix(a, "-"):
			return out, fmt.Sprintf("chart: unknown flag %q", a)
		default:
			if out.name != "" {
				return out, fmt.Sprintf("chart: unexpected argument %q (only one watch name allowed)", a)
			}
			out.name = a
		}
	}
	if out.name == "" {
		return out, "usage: chart <watch> [-t TYPE] [-n LIMIT] [-i INTERVAL] [-f]"
	}
	// Strip /cron.d/ prefix so the user can `chart /cron.d/foo` or `chart foo`.
	out.name = strings.TrimPrefix(out.name, "/cron.d/")
	out.name = strings.TrimPrefix(out.name, "cron.d/")
	if out.kind == "" {
		out.kind = "line"
	}
	switch out.kind {
	case "line", "sparkline", "bar":
	default:
		return out, fmt.Sprintf("chart: unknown type %q (want line, sparkline, bar)", out.kind)
	}
	return out, ""
}

// execChart implements the `chart` builtin. Returns true if the line
// was a chart invocation. One-shot path renders straight to scrollback;
// follow-mode delegates to startChartFollow (mode_chart.go).
func (c *ShellCore) execChart(parts []string) bool {
	if len(parts) == 0 || parts[0] != "chart" {
		return false
	}
	args, errMsg := parseChartCmd(parts[1:])
	if errMsg != "" {
		c.appendScrollback(ErrorText(errMsg) + "\n")
		return true
	}

	if c.shellFS == nil {
		c.appendScrollback(ErrorText("chart: filesystem not available") + "\n")
		return true
	}
	wr, ok := c.shellFS.(WatchResultsProvider)
	if !ok {
		c.appendScrollback(ErrorText("chart: watch results not supported in this shell") + "\n")
		return true
	}

	if args.follow {
		c.startChartFollow(args, wr)
		return true
	}

	out, msg := c.renderChartOnce(args, wr)
	if msg != "" {
		c.appendScrollback(ErrorText("chart: "+msg) + "\n")
		return true
	}
	c.appendScrollback(out)
	return true
}

// renderChartOnce fetches the latest results and renders one ANSI frame
// sized to the current terminal. Reserves 1 row at the bottom so the
// returning prompt has room to draw.
func (c *ShellCore) renderChartOnce(args chartArgs, wr WatchResultsProvider) (string, string) {
	w, h := c.chartDims()
	return RenderChartOneShot(wr, args.name, args.kind, args.limit, w, h)
}

// RenderChartOneShot is the reusable one-shot chart renderer. Both the
// interactive shell (via execChart) and the non-interactive runner (via
// `weft sh -c "chart …"`) call this — the difference is only how the
// returned string is delivered to the terminal.
//
// kind is "line", "sparkline", or "bar"; limit caps how many recent
// results are charted; (w, h) defines the render footprint. Returns
// (output, errMsg). errMsg is empty on success, otherwise a
// human-friendly string suitable for the shell to surface as-is.
func RenderChartOneShot(wr WatchResultsProvider, name, kind string, limit, w, h int) (string, string) {
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

// chartDims returns (width, height) for one chart frame, sized to fit
// the current terminal with one row reserved for the trailing prompt.
func (c *ShellCore) chartDims() (int, int) {
	w := c.width
	h := c.height - 1
	if w < 20 {
		w = 20
	}
	if h < 6 {
		h = 6
	}
	return w, h
}

// watchResultsToPoints maps a slice of WatchResultValue into chart.Point
// data. The numeric value is the *last whitespace-separated field* of the
// trimmed stdout — that matches the multi-row alert convention (`-k N`,
// where field N is the value) for the common case where N is the last
// column. Single scalar values are unaffected. The X coordinate is the
// RanAt timestamp parsed as RFC3339; rows that fail to parse are skipped.
//
// Results come in newest-first; the returned slice is oldest-first so
// the chart reads left-to-right as time-forward.
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

// formatChartTime is the bar-mode X-axis label formatter — uses the
// same HH:MM convention as the line-chart time axis so visually-similar
// data shows visually-similar labels.
func formatChartTime(epochSecs float64) string {
	t := time.Unix(int64(epochSecs), 0)
	if t.Second() == 0 {
		return t.Format("15:04")
	}
	return t.Format("15:04:05")
}

// lsCronWatchesLong renders `ls -l /cron.d/` rows: name + a 16-cell
// colored sparkline of the last 30 results. Watches with no recent
// results (or a backend that doesn't support GetWatchResults) print
// without the sparkline, so the long-form is never *worse* than plain
// ls — it only adds.
func (c *ShellCore) lsCronWatchesLong(entries []string) {
	wr, _ := c.shellFS.(WatchResultsProvider)
	for _, name := range entries {
		if wr == nil {
			c.appendScrollback(name + "\n")
			continue
		}
		results, err := wr.GetWatchResults(name, 30)
		if err != nil || len(results) == 0 {
			c.appendScrollback(fmt.Sprintf("%-24s\n", name))
			continue
		}
		pts, _ := watchResultsToPoints(results)
		if len(pts) == 0 {
			c.appendScrollback(fmt.Sprintf("%-24s\n", name))
			continue
		}
		// Cap the spark at 16 cells regardless of how many results.
		values := make([]float64, len(pts))
		for i, p := range pts {
			values[i] = p.Y
		}
		if len(values) > 16 {
			values = values[len(values)-16:]
		}
		spark := chart.SparklineColored(values)
		latest := fmt.Sprintf("%g", pts[len(pts)-1].Y)
		c.appendScrollback(fmt.Sprintf("%-24s  %s  %s\n", name, spark, latest))
	}
}
