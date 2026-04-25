package shellcore

import (
	"fmt"
	"strings"
	"time"

	"github.com/kevlarcon/weft/shellcore/chart"
)

// chartState is the live-chart session: a watch name, the rendering
// args, the last fetched results, and a heartbeat-throttled poll clock.
//
// Lives on ShellCore.chart while c.mode == ModeChart. Created by
// startChartFollow, torn down by exitChart.
type chartState struct {
	args     chartArgs
	wr       WatchResultsProvider
	lastTick time.Time      // when chartTick last *fired*
	lastErr  string         // most recent fetch error (shown in status)
	pts      []chart.Point  // currently-charted points
	width    int            // captured at enter
	height   int            // captured at enter
	rendered bool           // first frame painted
}

// startChartFollow enters ModeChart for the given watch.
//
// The first frame is fetched and painted synchronously so the user
// sees the chart immediately; subsequent frames update on the
// renderer's heartbeat (SpinnerTick), throttled to args.interval.
func (c *ShellCore) startChartFollow(args chartArgs, wr WatchResultsProvider) {
	st := &chartState{
		args:   args,
		wr:     wr,
		width:  c.width,
		height: c.height,
	}
	c.chart = st
	c.mode = ModeChart
	c.fetchAndDrawChart(true)
}

// chartTick is called every shellcore SpinnerTick. It throttles to
// args.interval and re-renders only when due. No-op when c.mode is not
// ModeChart.
func (c *ShellCore) chartTick() {
	if c.mode != ModeChart || c.chart == nil {
		return
	}
	if !c.chart.rendered {
		c.fetchAndDrawChart(true)
		return
	}
	if time.Since(c.chart.lastTick) < c.chart.args.interval {
		return
	}
	c.fetchAndDrawChart(true)
}

// fetchAndDrawChart pulls fresh results and repaints the frame. force
// is currently always true — kept as a parameter so a future
// `If-Modified-Since` short-circuit (per the plan) has a place to land.
func (c *ShellCore) fetchAndDrawChart(force bool) {
	st := c.chart
	if st == nil {
		return
	}
	st.lastTick = time.Now()

	// Re-fit on resize: the captured width/height belong to the frame
	// we last drew; if the terminal grew or shrunk the next paint uses
	// the live size instead.
	st.width = c.width
	st.height = c.height

	results, err := st.wr.GetWatchResults(st.args.name, st.args.limit)
	if err != nil {
		st.lastErr = err.Error()
	} else {
		pts, _ := watchResultsToPoints(results)
		st.pts = pts
		st.lastErr = ""
	}
	c.renderChart()
	st.rendered = true
}

// renderChart paints one frame: clear-screen + home, draw the chart,
// then a status row. Wraps the whole frame in DEC synchronized-update
// sequences so terminals that support them (xterm.js, modern xterm,
// kitty, wezterm, iTerm2 ≥ 3.5) avoid mid-frame tearing on slow paths.
func (c *ShellCore) renderChart() {
	st := c.chart
	if st == nil {
		return
	}
	w, h := st.width, st.height
	if h < 6 || w < 20 {
		// Terminal too small to be useful — render a one-liner and bail.
		var b strings.Builder
		b.WriteString("\x1b[H\x1b[2J")
		b.WriteString(ErrorText("chart: terminal too small"))
		b.WriteString("\r\n")
		c.write([]byte(b.String()))
		return
	}

	// Reserve one row at the bottom for the status bar.
	chartH := h - 1
	if chartH < 5 {
		chartH = 5
	}

	title := st.args.name
	if len(st.pts) > 0 {
		title = fmt.Sprintf("%s  (last %d, every %s)",
			st.args.name, len(st.pts), st.args.interval)
	}

	var body string
	switch {
	case st.lastErr != "":
		body = ErrorText("chart: "+st.lastErr) + "\r\n"
	case len(st.pts) == 0:
		body = DimText(fmt.Sprintf("(no numeric results for %q)", st.args.name)) + "\r\n"
	default:
		var rendered string
		var err error
		switch st.args.kind {
		case "sparkline":
			values := make([]float64, len(st.pts))
			for i, p := range st.pts {
				values[i] = p.Y
			}
			rendered = chart.Sparkline(values) + "\n"
		case "bar":
			labels := make([]string, len(st.pts))
			vals := make([]float64, len(st.pts))
			for i, p := range st.pts {
				labels[i] = formatChartTime(p.X)
				vals[i] = p.Y
			}
			rendered, err = chart.Bar(w, chartH).Title(title).BarValues(labels, vals).Render()
		default:
			rendered, err = chart.Line(w, chartH).Title(title).XAxisTime().Points(st.pts).Render()
		}
		if err != nil {
			body = ErrorText("chart: "+err.Error()) + "\r\n"
		} else {
			body = strings.ReplaceAll(rendered, "\n", "\r\n")
		}
	}

	// Build the full frame.
	var b strings.Builder
	// DEC sync update begin (no-op on terminals that don't grok it).
	b.WriteString("\x1b[?2026h")
	b.WriteString("\x1b[H\x1b[2J") // home + clear
	b.WriteString(body)

	// Status bar in reverse video, pinned to the last row.
	b.WriteString(fmt.Sprintf("\x1b[%d;1H", h)) // move to row h, col 1
	b.WriteString("\x1b[7m")
	left := fmt.Sprintf(" chart %s ", st.args.name)
	right := " q exit "
	pad := w - len(left) - len(right)
	if pad < 0 {
		pad = 0
	}
	b.WriteString(left)
	b.WriteString(strings.Repeat(" ", pad))
	b.WriteString(right)
	b.WriteString(ansiReset)

	// DEC sync update end.
	b.WriteString("\x1b[?2026l")
	c.write([]byte(b.String()))
}

// handleChartKey processes a keystroke in ModeChart. q / Esc / Ctrl+C
// exit; everything else is ignored (no scroll, no zoom — the chart
// always shows the full configured window).
func (c *ShellCore) handleChartKey(ev KeyEvent) {
	if c.chart == nil {
		return
	}
	switch ev.Key {
	case KeyEsc, KeyCtrlC, KeyCtrlD:
		c.exitChart()
		return
	}
	if ev.Rune == 'q' || ev.Rune == 'Q' {
		c.exitChart()
	}
}

// exitChart tears down the chart session and returns to ModeInput.
func (c *ShellCore) exitChart() {
	c.chart = nil
	c.mode = ModeInput
	c.write([]byte("\x1b[2J\x1b[H"))
	c.resetFlushState()
	c.Redraw()
}
