// Package spanview implements the interactive drill-down for `trace
// <id> --spans`: a list of span names rolled up from a trace's rows
// (see theme.RenderSpanRollup), and a "samples" level showing each
// individual occurrence of the focused name within that trace. Modeled
// on dashboard.DashboardView's focus/zoom pattern — see docs/plans/
// span-display.md, "Interactive drill-down."
package spanview

import (
	"fmt"
	"strings"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/graybeardcorp/weft/theme"
)

const viewName = "span"

// QueryFunc fetches trace_id's full row set the same way a bare `trace
// <id>` pipeline would — one raw "<ts_unix_ns>\t<namespace/service>\t
// <raw message>" line per row, the same shape theme.RenderSpanRollup
// consumes. OnEnter calls this once, asynchronously.
type QueryFunc func(traceID string) (lines []string, err error)

// ListArgs is passed to core.EnterView(viewName, ...) when `trace <id>
// --spans` is intercepted before dispatch — see Builtin.
type ListArgs struct {
	TraceID string
	Query   QueryFunc
}

type level int

const (
	levelList level = iota
	levelSamples
)

// SpanView implements shellcore.View.
type SpanView struct {
	traceID string
	query   QueryFunc

	rows        []theme.SpanRollupRow
	err         string
	level       level
	focus       int // index into rows, list level
	sampleFocus int // index into rows[focus].Occurrences, samples level

	pending chan fetchResult // async data delivery (cap 1); nil once drained
}

type fetchResult struct {
	rows []theme.SpanRollupRow
	err  error
}

// NewView constructs an unregistered SpanView. Pass it to
// core.RegisterView, or use Register/Builtin below.
func NewView() *SpanView { return &SpanView{} }

func (v *SpanView) Name() string { return viewName }

func (v *SpanView) OnEnter(c *shellcore.ShellCore, args any) error {
	a, ok := args.(*ListArgs)
	if !ok || a.Query == nil {
		return fmt.Errorf("span: invalid args")
	}
	v.traceID = a.TraceID
	v.query = a.Query
	v.rows = nil
	v.err = ""
	v.level = levelList
	v.focus = 0
	v.sampleFocus = 0

	c.Write([]byte("\x1b[?1049h"))

	// ch is captured by the goroutine below, not v.pending itself — a
	// re-entered View (a second `trace ... --spans` before the first
	// fetch lands) reassigns v.pending to a fresh channel, and the
	// stale goroutine must keep writing to its own ch (silently
	// discarded once nothing reads it) rather than delivering an old
	// trace's rows into the new session via the field.
	ch := make(chan fetchResult, 1)
	v.pending = ch
	go func(traceID string, query QueryFunc, ch chan fetchResult) {
		lines, err := query(traceID)
		if err != nil {
			ch <- fetchResult{err: err}
			return
		}
		ch <- fetchResult{rows: theme.RenderSpanRollup(lines)}
	}(a.TraceID, a.Query, ch)
	return nil
}

func (v *SpanView) OnExit(c *shellcore.ShellCore) {
	c.Write([]byte("\x1b[?1049l"))
}

func (v *SpanView) HandleKey(c *shellcore.ShellCore, ev shellcore.KeyEvent) bool {
	if v.level == levelSamples {
		return v.handleSamplesKey(c, ev)
	}
	return v.handleListKey(c, ev)
}

func (v *SpanView) handleListKey(c *shellcore.ShellCore, ev shellcore.KeyEvent) bool {
	switch {
	case ev.Key == shellcore.KeyEsc, ev.Rune == 'q', ev.Rune == 'Q':
		c.ExitView()
	case ev.Key == shellcore.KeyUp, ev.Rune == 'k':
		v.move(-1)
		v.Render(c)
	case ev.Key == shellcore.KeyDown, ev.Rune == 'j':
		v.move(1)
		v.Render(c)
	case ev.Rune == 'g':
		v.focus = 0
		v.Render(c)
	case ev.Rune == 'G':
		v.focus = len(v.rows) - 1
		v.clampFocus()
		v.Render(c)
	case ev.Key == shellcore.KeyEnter:
		if len(v.rows) > 0 && v.rows[v.focus].Count() > 0 {
			v.level = levelSamples
			v.sampleFocus = 0
			v.Render(c)
		}
	}
	return true
}

func (v *SpanView) handleSamplesKey(c *shellcore.ShellCore, ev shellcore.KeyEvent) bool {
	occ := v.currentOccurrences()
	switch {
	case ev.Key == shellcore.KeyEsc:
		v.level = levelList
		v.Render(c)
	case ev.Rune == 'q', ev.Rune == 'Q':
		c.ExitView()
	case ev.Key == shellcore.KeyUp, ev.Rune == 'k':
		v.moveSample(-1, occ)
		v.Render(c)
	case ev.Key == shellcore.KeyDown, ev.Rune == 'j':
		v.moveSample(1, occ)
		v.Render(c)
	case ev.Rune == 'g':
		v.sampleFocus = 0
		v.Render(c)
	case ev.Rune == 'G':
		v.sampleFocus = len(occ) - 1
		v.clampSample(occ)
		v.Render(c)
	}
	return true
}

func (v *SpanView) move(dir int) {
	v.focus += dir
	v.clampFocus()
}

func (v *SpanView) clampFocus() {
	if v.focus < 0 {
		v.focus = 0
	}
	if v.focus >= len(v.rows) {
		v.focus = len(v.rows) - 1
	}
	if v.focus < 0 {
		v.focus = 0
	}
}

func (v *SpanView) moveSample(dir int, occ []theme.SpanOccurrence) {
	v.sampleFocus += dir
	v.clampSample(occ)
}

func (v *SpanView) clampSample(occ []theme.SpanOccurrence) {
	if v.sampleFocus < 0 {
		v.sampleFocus = 0
	}
	if v.sampleFocus >= len(occ) {
		v.sampleFocus = len(occ) - 1
	}
	if v.sampleFocus < 0 {
		v.sampleFocus = 0
	}
}

func (v *SpanView) currentOccurrences() []theme.SpanOccurrence {
	if v.focus < 0 || v.focus >= len(v.rows) {
		return nil
	}
	return v.rows[v.focus].Occurrences
}

// Tick drains the async fetch result, if any, and repaints once.
func (v *SpanView) Tick(c *shellcore.ShellCore) {
	if v.pending == nil {
		return
	}
	select {
	case res := <-v.pending:
		v.pending = nil
		if res.err != nil {
			v.err = res.err.Error()
		} else {
			v.rows = res.rows
			v.clampFocus()
		}
		v.Render(c)
	default:
	}
}

func (v *SpanView) Render(c *shellcore.ShellCore) {
	w, h := c.Width(), c.Height()
	if w < 20 {
		w = 20
	}
	if h < 6 {
		h = 6
	}

	var buf strings.Builder
	buf.WriteString("\x1b[?2026h") // DEC sync update begin
	buf.WriteString("\x1b[H\x1b[2J")

	switch {
	case v.err != "":
		buf.WriteString(shellcore.ErrorText("span: " + v.err))
		buf.WriteString("\r\n")
	case v.pending != nil:
		buf.WriteString(shellcore.DimText("fetching trace " + v.traceID + "..."))
		buf.WriteString("\r\n")
	case v.level == levelSamples:
		v.renderSamples(&buf, w, h)
	default:
		v.renderList(&buf, w, h)
	}

	buf.WriteString(fmt.Sprintf("\x1b[%d;1H", h))
	buf.WriteString("\x1b[7m")
	status := v.statusBar()
	pad := w - len(status)
	if pad < 0 {
		pad = 0
	}
	buf.WriteString(status)
	buf.WriteString(strings.Repeat(" ", pad))
	buf.WriteString(shellcore.AnsiReset)

	buf.WriteString("\x1b[?2026l") // DEC sync update end
	c.Write([]byte(buf.String()))
}

func (v *SpanView) statusBar() string {
	if v.level == levelSamples {
		return fmt.Sprintf(" spans: %s — samples   \xe2\x86\x91\xe2\x86\x93/jk move  esc back  q exit ", v.currentName())
	}
	return fmt.Sprintf(" spans: %s   \xe2\x86\x91\xe2\x86\x93/jk move  \xe2\x8f\x8e samples  esc/q exit ", v.traceID)
}

func (v *SpanView) currentName() string {
	if v.focus < 0 || v.focus >= len(v.rows) {
		return ""
	}
	return v.rows[v.focus].Name
}

func (v *SpanView) renderList(buf *strings.Builder, w, h int) {
	if len(v.rows) == 0 {
		buf.WriteString(shellcore.DimText(fmt.Sprintf("(no spans found in trace %s)", v.traceID)))
		buf.WriteString("\r\n")
		return
	}

	header, rowTexts := theme.SpanRollupColumns(v.rows, w)
	buf.WriteString(shellcore.DimText(header))
	buf.WriteString("\r\n")

	viewport := h - 2 // header + status bar
	if viewport < 1 {
		viewport = 1
	}
	start := 0
	if v.focus >= viewport {
		start = v.focus - viewport + 1
	}
	end := start + viewport
	if end > len(rowTexts) {
		end = len(rowTexts)
	}
	for i := start; i < end; i++ {
		line := padToWidth(rowTexts[i], w)
		if i == v.focus {
			buf.WriteString("\x1b[7m")
			buf.WriteString(line)
			buf.WriteString(shellcore.AnsiReset)
		} else {
			buf.WriteString(line)
		}
		buf.WriteString("\r\n")
	}
}

func (v *SpanView) renderSamples(buf *strings.Builder, w, h int) {
	occ := v.currentOccurrences()
	if len(occ) == 0 {
		buf.WriteString(shellcore.DimText("(no occurrences)"))
		buf.WriteString("\r\n")
		return
	}

	header, rowTexts := occurrenceColumns(occ, w)
	buf.WriteString(shellcore.DimText(header))
	buf.WriteString("\r\n")

	viewport := h - 2
	if viewport < 1 {
		viewport = 1
	}
	start := 0
	if v.sampleFocus >= viewport {
		start = v.sampleFocus - viewport + 1
	}
	end := start + viewport
	if end > len(rowTexts) {
		end = len(rowTexts)
	}
	for i := start; i < end; i++ {
		line := padToWidth(rowTexts[i], w)
		if i == v.sampleFocus {
			buf.WriteString("\x1b[7m")
			buf.WriteString(line)
			buf.WriteString(shellcore.AnsiReset)
		} else {
			buf.WriteString(line)
		}
		buf.WriteString("\r\n")
	}
}

// padToWidth right-pads (or truncates) s to exactly w visible chars, so
// a reverse-video-wrapped focused row fills the full terminal width
// rather than only as far as its own text.
func padToWidth(s string, w int) string {
	if len(s) >= w {
		return s[:w]
	}
	return s + strings.Repeat(" ", w-len(s))
}
