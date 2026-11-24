package chart

import (
	"fmt"
	"strings"
	"time"

	shellcore "github.com/graybeardcorp/warp/pkg/shell"
)

// View is the live-chart takeover screen. It implements
// shellcore.View; the embedder registers an instance via
// core.RegisterView. Activation is via core.EnterView("chart",
// args) where args is a WatchArgs value (typically produced by the
// chart Builtin handler when --follow is set).
type View struct {
	src       CronSource    // series source
	rangeSpec func() string // the shell's range, re-read on every fetch

	// Active session — non-nil only between OnEnter and OnExit.
	session *session
}

// session is the per-takeover state: which cron we're charting,
// the rendering args, the last fetched series, and a heartbeat clock.
type session struct {
	args     WatchArgs
	lastTick time.Time  // when Tick last fired
	lastErr  string     // most recent fetch error (shown in status)
	series   CronSeries // currently-charted series
	width    int        // captured at enter
	height   int        // captured at enter
	rendered bool       // first frame painted
}

// NewView constructs a chart View backed by the given CronSource and
// range. Pass it to core.RegisterView at startup.
func NewView(src CronSource, rangeSpec func() string) *View {
	return &View{src: src, rangeSpec: rangeSpec}
}

// Name implements shellcore.View.
func (v *View) Name() string { return "chart" }

// OnEnter starts a chart-follow session. Args must be a WatchArgs
// value (the builtin handler produces these when --follow is set).
func (v *View) OnEnter(c *shellcore.Shell, args any) error {
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
func (v *View) OnExit(c *shellcore.Shell) {
	v.session = nil
}

// HandleKey processes a keystroke. q/Esc/Ctrl+C/Ctrl+D exit;
// everything else is ignored (no scroll, no zoom — the chart
// always shows the configured window).
func (v *View) HandleKey(c *shellcore.Shell, ev shellcore.KeyEvent) bool {
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
func (v *View) Render(c *shellcore.Shell) {
	if v.session == nil {
		return
	}
	// Re-fit on resize: pick up the live terminal size.
	v.session.width, v.session.height = dims(c)
	v.paint(c)
}

// Tick is called every shellcore SpinnerTick. Throttled to
// args.Interval; refetches and repaints when due.
func (v *View) Tick(c *shellcore.Shell) {
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
func (v *View) fetchAndDraw(c *shellcore.Shell) {
	st := v.session
	if st == nil {
		return
	}
	st.lastTick = time.Now()

	// Resolving the range per fetch is what makes a relative start slide
	// and an absolute start with an open end grow.
	series, err := v.src.GetCronSeries(st.args.Name, v.rangeSpec())
	if err != nil {
		st.lastErr = err.Error()
	} else {
		st.series = series
		st.lastErr = ""
	}
	v.paint(c)
	st.rendered = true
}

// paint draws the current frame. Wraps in DEC synchronized-update
// sequences so terminals that support them (xterm.js, modern
// xterm, kitty, wezterm, iTerm2 ≥ 3.5) avoid mid-frame tearing.
func (v *View) paint(c *shellcore.Shell) {
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

	var body string
	if st.lastErr != "" {
		body = shellcore.ErrorText("chart: "+st.lastErr) + "\r\n"
	} else if rendered, msg := RenderCron(st.args.Name, st.series, st.args, w, chartH); msg != "" {
		body = shellcore.DimText("("+msg+")") + "\r\n"
	} else {
		body = strings.ReplaceAll(rendered, "\n", "\r\n")
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
func dims(c *shellcore.Shell) (int, int) {
	w, h := c.Width(), c.Height()
	if w < 20 {
		w = 20
	}
	if h < 6 {
		h = 6
	}
	return w, h
}
