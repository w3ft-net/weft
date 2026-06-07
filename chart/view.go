package chart

import (
	"fmt"
	"strings"
	"time"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/graybeardcorp/warp/pkg/shellcore/chart"
)

// View is the live-chart takeover screen. It implements
// shellcore.View; the embedder registers an instance via
// core.RegisterView. Activation is via core.EnterView("chart",
// args) where args is a WatchArgs value (typically produced by the
// chart Builtin handler when --follow is set).
type View struct {
	wr WatchResultsProvider // results source

	// Active session — non-nil only between OnEnter and OnExit.
	session *session
}

// session is the per-takeover state: which watch we're charting,
// the rendering args, last fetched points, and a heartbeat clock.
type session struct {
	args     WatchArgs
	lastTick time.Time     // when Tick last fired
	lastErr  string        // most recent fetch error (shown in status)
	pts      []chart.Point // currently-charted points
	width    int           // captured at enter
	height   int           // captured at enter
	rendered bool          // first frame painted
}

// NewView constructs a chart View backed by the given
// WatchResultsProvider. Pass it to core.RegisterView at startup.
func NewView(wr WatchResultsProvider) *View { return &View{wr: wr} }

// Name implements shellcore.View.
func (v *View) Name() string { return "chart" }

// OnEnter starts a chart-follow session. Args must be a WatchArgs
// value (the builtin handler produces these when --follow is set).
func (v *View) OnEnter(c *shellcore.ShellCore, args any) error {
	a, ok := args.(WatchArgs)
	if !ok {
		return fmt.Errorf("chart: expected WatchArgs, got %T", args)
	}
	w, h := dims(c)
	v.session = &session{
		args:   a,
		width:  w,
		height: h,
	}
	v.fetchAndDraw(c)
	return nil
}

// OnExit tears down the session and clears the screen.
func (v *View) OnExit(c *shellcore.ShellCore) {
	v.session = nil
}

// HandleKey processes a keystroke. q/Esc/Ctrl+C/Ctrl+D exit;
// everything else is ignored (no scroll, no zoom — the chart
// always shows the configured window).
func (v *View) HandleKey(c *shellcore.ShellCore, ev shellcore.KeyEvent) bool {
	if v.session == nil {
		return false
	}
	switch ev.Key {
	case shellcore.KeyEsc, shellcore.KeyCtrlC, shellcore.KeyCtrlD:
		c.ExitView()
		return true
	}
	if ev.Rune == 'q' || ev.Rune == 'Q' {
		c.ExitView()
		return true
	}
	// Consume all other keys — chart mode is read-only.
	return true
}

// Render paints one frame. Called on enter and resize.
func (v *View) Render(c *shellcore.ShellCore) {
	if v.session == nil {
		return
	}
	// Re-fit on resize: pick up the live terminal size.
	v.session.width, v.session.height = dims(c)
	v.paint(c)
}

// Tick is called every shellcore SpinnerTick. Throttled to
// args.Interval; refetches and repaints when due.
func (v *View) Tick(c *shellcore.ShellCore) {
	st := v.session
	if st == nil {
		return
	}
	if !st.rendered {
		v.fetchAndDraw(c)
		return
	}
	if time.Since(st.lastTick) < st.args.Interval {
		return
	}
	v.fetchAndDraw(c)
}

// fetchAndDraw pulls fresh results and repaints.
func (v *View) fetchAndDraw(c *shellcore.ShellCore) {
	st := v.session
	if st == nil {
		return
	}
	st.lastTick = time.Now()

	results, err := v.wr.GetWatchResults(st.args.Name, st.args.Limit)
	if err != nil {
		st.lastErr = err.Error()
	} else {
		pts, _ := watchResultsToPoints(results)
		st.pts = pts
		st.lastErr = ""
	}
	v.paint(c)
	st.rendered = true
}

// paint draws the current frame. Wraps in DEC synchronized-update
// sequences so terminals that support them (xterm.js, modern
// xterm, kitty, wezterm, iTerm2 ≥ 3.5) avoid mid-frame tearing.
func (v *View) paint(c *shellcore.ShellCore) {
	st := v.session
	if st == nil {
		return
	}
	w, h := st.width, st.height
	if h < 6 || w < 20 {
		var b strings.Builder
		b.WriteString("\x1b[H\x1b[2J")
		b.WriteString(shellcore.ErrorText("chart: terminal too small"))
		b.WriteString("\r\n")
		c.Write([]byte(b.String()))
		return
	}

	// Reserve one row at the bottom for the status bar.
	chartH := h - 1
	if chartH < 5 {
		chartH = 5
	}

	title := st.args.Name
	if len(st.pts) > 0 {
		title = fmt.Sprintf("%s  (last %d, every %s)",
			st.args.Name, len(st.pts), st.args.Interval)
	}

	var body string
	switch {
	case st.lastErr != "":
		body = shellcore.ErrorText("chart: "+st.lastErr) + "\r\n"
	case len(st.pts) == 0:
		body = shellcore.DimText(fmt.Sprintf("(no numeric results for %q)", st.args.Name)) + "\r\n"
	default:
		var rendered string
		var err error
		switch st.args.Kind {
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
			body = shellcore.ErrorText("chart: "+err.Error()) + "\r\n"
		} else {
			body = strings.ReplaceAll(rendered, "\n", "\r\n")
		}
	}

	var b strings.Builder
	b.WriteString("\x1b[?2026h") // DEC sync update begin
	b.WriteString("\x1b[H\x1b[2J")
	b.WriteString(body)

	// Status bar in reverse video, pinned to the last row.
	b.WriteString(fmt.Sprintf("\x1b[%d;1H", h))
	b.WriteString("\x1b[7m")
	left := fmt.Sprintf(" chart %s ", st.args.Name)
	right := " q exit "
	pad := w - len(left) - len(right)
	if pad < 0 {
		pad = 0
	}
	b.WriteString(left)
	b.WriteString(strings.Repeat(" ", pad))
	b.WriteString(right)
	b.WriteString(shellcore.AnsiReset)
	b.WriteString("\x1b[?2026l") // DEC sync update end
	c.Write([]byte(b.String()))
}

// dims returns (width, height) for one chart frame, sized to the
// current terminal. Reserves nothing because the View paints the
// full screen.
func dims(c *shellcore.ShellCore) (int, int) {
	w, h := c.Width(), c.Height()
	if w < 20 {
		w = 20
	}
	if h < 6 {
		h = 6
	}
	return w, h
}
