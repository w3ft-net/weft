// Package spanview implements the interactive drill-down shared by
// both span-display phases: `trace <id> --spans` (Phase 1, a list of
// span names rolled up from one trace's rows — see
// theme.RenderSpanRollup) and `trace spans [name]` (Phase 2,
// cross-trace aggregation over the queried time range). Both feed the
// same list + "samples" levels — a samples row is a single occurrence
// (which trace/label it hit, its own duration), letting the user pick
// one and hand off into `trace <id>` for that specific request.
// Modeled on dashboard.DashboardView's focus/zoom pattern — see
// docs/plans/span-display.md, "Interactive drill-down."
package spanview

import (
	"fmt"
	"strings"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/graybeardcorp/weft/theme"
)

const viewName = "span"

// QueryFunc runs pipeline (a full weft pipeline string — `trace <id>`
// for Phase 1, `trace spans` or `trace spans <name>` for Phase 2's
// cross-trace aggregation) and returns its rows the same shape
// theme.RenderSpanRollup consumes: one raw "<ts_unix_ns>\t<namespace/
// service>\t<raw message>" line per row. OnEnter calls this once,
// asynchronously.
type QueryFunc func(pipeline string) (lines []string, err error)

// ListArgs is passed to core.EnterView(viewName, ...) when Builtin
// intercepts `trace <id> --spans`, `trace spans`, or `trace spans
// <name>` before normal pipeline dispatch. Title is what the status
// bar / empty-state text shows — the trace ID for Phase 1, the span
// name (or "all spans") for Phase 2.
type ListArgs struct {
	Pipeline string
	Title    string
	Query    QueryFunc
}

type level int

const (
	levelList level = iota
	levelSamples
)

// SpanView implements shellcore.View.
type SpanView struct {
	pipeline string
	title    string
	query    QueryFunc

	rows        []theme.SpanRollupRow
	err         string
	level       level
	focus       int // index into rows, list level
	sampleFocus int // index into rows[focus].Occurrences, samples level

	pending chan fetchResult // async data delivery (cap 1); nil once drained

	samples        []theme.SpanOccurrence
	samplesPending chan samplesResult
}

type samplesResult struct {
	occ []theme.SpanOccurrence
	err error
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
	v.pipeline = a.Pipeline
	v.title = a.Title
	v.query = a.Query
	v.rows = nil
	v.err = ""
	v.level = levelList
	v.focus = 0
	v.sampleFocus = 0
	v.samples = nil
	v.samplesPending = nil

	c.Write([]byte("\x1b[?1049h"))

	// ch is captured by the goroutine below, not v.pending itself — a
	// re-entered View (a second `trace ... --spans` before the first
	// fetch lands) reassigns v.pending to a fresh channel, and the
	// stale goroutine must keep writing to its own ch (silently
	// discarded once nothing reads it) rather than delivering an old
	// query's rows into the new session via the field.
	ch := make(chan fetchResult, 1)
	v.pending = ch
	go func(pipeline string, query QueryFunc, ch chan fetchResult) {
		lines, err := query(pipeline)
		if err != nil {
			ch <- fetchResult{err: err}
			return
		}
		if isSpansAggregatePipeline(pipeline) {
			ch <- fetchResult{rows: theme.RenderSpanAggregate(lines)}
			return
		}
		ch <- fetchResult{rows: theme.RenderSpanRollup(lines)}
	}(a.Pipeline, a.Query, ch)
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
			v.samples = nil
			if isSpansAggregatePipeline(v.pipeline) {
				v.fetchSamples(v.rows[v.focus].Name)
			}
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
	if isSpansAggregatePipeline(v.pipeline) {
		return v.samples
	}
	if v.focus < 0 || v.focus >= len(v.rows) {
		return nil
	}
	return v.rows[v.focus].Occurrences
}

func (v *SpanView) fetchSamples(name string) {
	ch := make(chan samplesResult, 1)
	v.samplesPending = ch
	go func(pipeline string, query QueryFunc) {
		lines, err := query(pipeline)
		if err != nil {
			ch <- samplesResult{err: err}
			return
		}
		ch <- samplesResult{occ: theme.RenderSpanSamples(lines, theme.SpanSamplesLimit)}
	}("trace spans "+name+" --samples", v.query)
}

// Tick drains the async fetch result, if any, and repaints once.
func (v *SpanView) Tick(c *shellcore.ShellCore) {
	if v.samplesPending != nil {
		select {
		case res := <-v.samplesPending:
			v.samplesPending = nil
			if res.err != nil {
				v.err = res.err.Error()
			} else {
				v.samples = res.occ
			}
			v.Render(c)
		default:
		}
	}
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
		buf.WriteString(shellcore.DimText("fetching " + v.title + "..."))
		buf.WriteString("\r\n")
	case v.samplesPending != nil:
		buf.WriteString(shellcore.DimText("fetching the slowest " + v.currentName() + " spans..."))
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
	return fmt.Sprintf(" spans: %s   \xe2\x86\x91\xe2\x86\x93/jk move  \xe2\x8f\x8e samples  esc/q exit ", v.title)
}

func (v *SpanView) currentName() string {
	if v.focus < 0 || v.focus >= len(v.rows) {
		return ""
	}
	return v.rows[v.focus].Name
}

func (v *SpanView) renderList(buf *strings.Builder, w, h int) {
	if len(v.rows) == 0 {
		buf.WriteString(shellcore.DimText(fmt.Sprintf("(no spans found for %s)", v.title)))
		buf.WriteString("\r\n")
		return
	}

	contentWidth := w - cursorWidth
	header, rowTexts := theme.SpanRollupColumns(v.rows, contentWidth, !isSpansAggregatePipeline(v.pipeline))
	buf.WriteString(shellcore.DimText(cursorBlank + header))
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
		line := padToWidth(rowTexts[i], contentWidth)
		if i == v.focus {
			buf.WriteString(cursorMark)
			buf.WriteString(underlineOn)
			buf.WriteString(line)
			buf.WriteString(shellcore.AnsiReset)
		} else {
			buf.WriteString(cursorBlank)
			buf.WriteString(line)
		}
		buf.WriteString("\r\n")
	}
}

// The focused row in renderList/renderSamples used to be marked with
// plain reverse video ("\x1b[7m"), which swaps foreground and
// background per character — fine for plain text, but
// theme.SpanRollupColumns' and occurrenceColumns' rows carry per-cell
// foreground color (NAME gold, durations indigo, the BAR itself
// orange), so reversing swapped each cell to a background tinted by
// *its own* color instead of one uniform highlight (the "lavender"
// background was the indigo Number color, reversed). An explicit
// black background composed more cleanly but still read as messy —
// this instead marks the row with a left-hand-side cursor plus an
// underline: cursorMark/cursorBlank reserve a fixed 2-column gutter
// (cursorWidth) that both renderers subtract from the width they hand
// theme.SpanRollupColumns/occurrenceColumns, so the arrow doesn't
// shift the table's own column alignment between focused and
// unfocused rows. underlineOn ("\x1b[4m") is, like the background
// attempt before it, an independent SGR parameter from foreground
// color — cells already reset only fg/bold/italic between colors
// (theme.FgReset), never underline, so it stays on for the whole row
// and each cell's own color still reads normally on top of it.
const (
	cursorWidth = 2
	cursorBlank = "  "
	underlineOn = "\x1b[4m"
)

// cursorMark is the focused-row indicator itself, colored in the
// palette's Operator orange (the same accent renderSpanBar's fill
// uses) so it reads as a pointer, not more table content. It resets
// with theme.FgReset (not a full reset) before underlineOn turns on,
// so the mark's own color doesn't bleed into the underlined row that
// follows it, and so nothing here interferes with the caller's later
// full reset at the row's end.
var cursorMark = theme.Palette().Operator + "▸ " + theme.FgReset

// isSpansAggregatePipeline reports whether pipeline is specifically a
// `trace spans [name]` query (Phase 2) as opposed to `trace <id>`
// (Phase 1) — mirrors shell/shell.go's function of the same name.
// Duplicated rather than shared: it's a three-line check, and the two
// packages' pipeline strings come from different builders (ParseSpansLine's
// ListArgs.Pipeline here, planner-parsed pipeline text there) that only
// happen to agree on this one shape. Gates theme.SpanRollupColumns'
// singleTrace in renderList — see that function's doc comment for why
// Phase 2 doesn't get self-time.
func isSpansAggregatePipeline(pipeline string) bool {
	fields := strings.Fields(pipeline)
	return len(fields) > 1 && fields[0] == "trace" && fields[1] == "spans"
}

func (v *SpanView) renderSamples(buf *strings.Builder, w, h int) {
	occ := v.currentOccurrences()
	if len(occ) == 0 {
		buf.WriteString(shellcore.DimText("(no occurrences)"))
		buf.WriteString("\r\n")
		return
	}

	contentWidth := w - cursorWidth
	header, rowTexts := occurrenceColumns(occ, contentWidth, !isSpansAggregatePipeline(v.pipeline))
	buf.WriteString(shellcore.DimText(cursorBlank + header))
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
		line := padToWidth(rowTexts[i], contentWidth)
		if i == v.sampleFocus {
			buf.WriteString(cursorMark)
			buf.WriteString(underlineOn)
			buf.WriteString(line)
			buf.WriteString(shellcore.AnsiReset)
		} else {
			buf.WriteString(cursorBlank)
			buf.WriteString(line)
		}
		buf.WriteString("\r\n")
	}
}

// padToWidth right-pads s to exactly w visible chars, so a
// reverse-video-wrapped focused row fills the full terminal width
// rather than only as far as its own text. Measures with
// shellcore.StripANSI rather than len(s) — theme.SpanRollupColumns'
// rows now carry embedded color codes (see its doc comment), and
// len(s) counts those escape bytes as width, which would both
// misjudge whether padding is needed and risk byte-slicing s[:w]
// through the middle of an escape sequence. Rows are already sized to
// w in visible chars by SpanRollupColumns, so the >= case is expected
// to be an exact match, not truncation — a colored string can't be
// safely truncated by byte index, so a wider-than-expected row is left
// alone rather than risking a corrupted escape sequence.
func padToWidth(s string, w int) string {
	visLen := len(shellcore.StripANSI(s))
	if visLen >= w {
		return s
	}
	return s + strings.Repeat(" ", w-visLen)
}
