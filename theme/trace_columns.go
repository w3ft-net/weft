package theme

import (
	"fmt"
	"strconv"
	"strings"
)

// Column widths for RenderTraceColumns' fixed-width layout. gapColWidth
// is the right-aligned histogram bar; traceLabel{Min,Max}Width bound how
// wide the service-badge column grows so a trace with long service names
// still leaves room for the message. traceMinMsgWidth is the floor the
// message column never shrinks below, even on a pathologically narrow
// terminal — better a message column narrower than the bar/badge columns
// "should" leave it than zero width.
const (
	gapColWidth        = 8
	traceLabelMinWidth = 8
	traceLabelMaxWidth = 24
	traceMinMsgWidth   = 20
	traceColumnSpacing = 1 // single space between each of the columns
	// gapTextWidth fits the widest formatTraceGap output ("+999.9ms"
	// never occurs: values of 10 or more print without a decimal).
	gapTextWidth = 7
)

// TraceRow is one line of a `trace <id>` result, parsed. IsData is
// false for a passthrough line (the "# N line(s)..." summary
// emitTraceTagged emits, "# no lines found", or anything else that
// doesn't match the 3-field tab-delimited data shape) — Raw is the
// only meaningful field in that case, the rest are zero. ColorIdx is
// TraceLabelColorIndex(Label), included per-row so a JSON consumer
// (the browser's wasmParseTraceRows export — see
// docs/plans/browser-span-view.md) doesn't need to re-derive the same
// hash to color the badge consistently with the ANSI renderer.
type TraceRow struct {
	Raw     string
	IsData  bool
	TsNs    int64
	Label   string
	Message string
	GapNs   int64 // relative to the previous data row; 0 for the first
	// Gap is GapNs as text ("+24ms"); empty for the first data row,
	// which has no previous line to be a gap from. Formatted here so
	// the ANSI renderer and the browser show identical text.
	Gap      string
	ColorIdx int
}

// ParseTraceRows parses lines — the same "<ts_unix_ns>\t<namespace/
// service>\t<raw message>" shape emitTraceTagged emits — into TraceRow,
// computing each data row's gap relative to the previous data row (a
// passthrough line in between doesn't reset the clock, matching
// RenderTraceColumns' own gap computation). This is the one parse step
// both RenderTraceColumns (the ANSI renderer) and the browser's
// wasmParseTraceRows export share, so there's exactly one place that
// decides what counts as a data row.
func ParseTraceRows(lines []string) []TraceRow {
	rows := make([]TraceRow, len(lines))
	for i, l := range lines {
		parts := strings.SplitN(l, "\t", 3)
		if len(parts) != 3 {
			rows[i] = TraceRow{Raw: l}
			continue
		}
		ts, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			rows[i] = TraceRow{Raw: l}
			continue
		}
		rows[i] = TraceRow{
			Raw: l, IsData: true, TsNs: ts, Label: parts[1], Message: parts[2],
			ColorIdx: TraceLabelColorIndex(parts[1]),
		}
	}

	var prevTs int64
	firstData := true
	for i := range rows {
		if !rows[i].IsData {
			continue
		}
		if !firstData {
			gap := rows[i].TsNs - prevTs
			if gap < 0 {
				gap = 0
			}
			rows[i].GapNs = gap
			rows[i].Gap = formatTraceGap(gap)
		}
		firstData = false
		prevTs = rows[i].TsNs
	}
	return rows
}

// formatTraceGap renders a gap in ns as "+850ns", "+24µs", "+2.3ms",
// "+1.2s" or "+1.5m": the largest unit that keeps the value at or above
// 1, with one decimal below 10 and none above.
func formatTraceGap(ns int64) string {
	if ns < 1000 {
		return "+" + strconv.FormatInt(ns, 10) + "ns"
	}
	v, unit := float64(ns)/1e3, "µs"
	switch {
	case ns >= 60_000_000_000:
		v, unit = float64(ns)/6e10, "m"
	case ns >= 1_000_000_000:
		v, unit = float64(ns)/1e9, "s"
	case ns >= 1_000_000:
		v, unit = float64(ns)/1e6, "ms"
	}
	if v < 10 {
		return "+" + strings.TrimSuffix(strconv.FormatFloat(v, 'f', 1, 64), ".0") + unit
	}
	return "+" + strconv.FormatFloat(v, 'f', 0, 64) + unit
}

// RenderTraceColumns re-renders the delimited rows compute.Job's
// emitTraceTagged emits for a bare `trace <id>` ("<ts_unix_ns>\t<namespace/
// service>\t<raw message>") into a 3-column display sized to termWidth:
//
//	<gap, e.g. +24ms> <gap histogram, right-aligned> <service badge> <message, wrapped>
//
// The gap column is dropped when the terminal is too narrow to leave the
// message its minimum width. The bar in column 2 is scaled against the largest gap in this trace, so
// it reads as a relative "where did the time go between log lines" view
// rather than an absolute duration axis. A line that doesn't match the
// exact 3-field tab-delimited shape (the "# N line(s)..." summary line
// emitTraceTagged also emits, "# no lines found", or anything else) is
// returned unchanged in place — this makes the function safe to call on
// the full line set unconditionally rather than needing the caller to
// separate summary/data lines first.
func RenderTraceColumns(lines []string, termWidth int) []string {
	parsed := ParseTraceRows(lines)

	// First pass over data rows only: the widest label (gaps are
	// already computed by ParseTraceRows) so the bar scale and badge
	// column width are known before any rendering happens.
	var maxGap int64
	haveData := false
	labelWidth := traceLabelMinWidth
	for _, p := range parsed {
		if !p.IsData {
			continue
		}
		haveData = true
		if p.GapNs > maxGap {
			maxGap = p.GapNs
		}
		if w := len(p.Label); w > labelWidth {
			labelWidth = w
		}
	}
	if !haveData {
		return lines
	}
	if labelWidth > traceLabelMaxWidth {
		labelWidth = traceLabelMaxWidth
	}

	msgWidth := termWidth - gapColWidth - labelWidth - 2*traceColumnSpacing
	showGapText := msgWidth-gapTextWidth-traceColumnSpacing >= traceMinMsgWidth
	if showGapText {
		msgWidth -= gapTextWidth + traceColumnSpacing
	}
	if msgWidth < traceMinMsgWidth {
		msgWidth = traceMinMsgWidth
	}

	blankGapCol := strings.Repeat(" ", gapColWidth)
	if showGapText {
		blankGapCol = strings.Repeat(" ", gapTextWidth+traceColumnSpacing+gapColWidth)
	}
	blankLabelCol := strings.Repeat(" ", labelWidth)
	sep := strings.Repeat(" ", traceColumnSpacing)

	out := make([]string, 0, len(lines))
	for _, p := range parsed {
		if !p.IsData {
			out = append(out, p.Raw)
			continue
		}
		bar := renderGapBar(p.GapNs, maxGap, gapColWidth)
		if showGapText {
			bar = Palette().Comment + fmt.Sprintf("%*s", gapTextWidth, p.Gap) + "\x1b[0m" + sep + bar
		}
		badge := renderTraceLabelColumn(p.Label, labelWidth)
		for j, wrapped := range wrapText(p.Message, msgWidth) {
			if j == 0 {
				out = append(out, bar+sep+badge+sep+wrapped)
			} else {
				out = append(out, blankGapCol+sep+blankLabelCol+sep+wrapped)
			}
		}
	}
	return out
}

// renderGapBar draws a right-aligned bar of width chars, filled
// proportionally to gap/maxGap (0 gap, or the trace's first row with no
// prior line to gap from, renders as all spaces — not a 1-char sliver,
// which would misread as "some time passed" when none did relative to
// what's being measured).
func renderGapBar(gap, maxGap int64, width int) string {
	n := 0
	if maxGap > 0 {
		n = int(float64(gap) / float64(maxGap) * float64(width))
		if n > width {
			n = width
		}
	}
	return Palette().Comment + strings.Repeat(" ", width-n) + strings.Repeat("█", n) + "\x1b[0m"
}

// renderTraceLabelColumn pads/truncates label to exactly width visible
// characters and colors it with the same deterministic per-label hash
// TraceTag's badge uses, so a service reads as the same color whether
// it's shown as an inline "[ns/svc]" tag or a column.
func renderTraceLabelColumn(label string, width int) string {
	display := label
	if len(display) > width {
		if width > 1 {
			display = display[:width-1] + "…"
		} else {
			display = display[:width]
		}
	}
	pad := strings.Repeat(" ", width-len(display))
	return TraceTagColor(label) + display + "\x1b[0m" + pad
}

// wrapText greedily word-wraps s to width, hard-breaking any single word
// longer than width on its own (a URL or path, say) rather than
// overflowing the column. Always returns at least one line, even for an
// empty message, so RenderTraceColumns' caller can rely on "one entry
// per physical output line."
func wrapText(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	words := strings.Fields(s)
	if len(words) == 0 {
		return []string{""}
	}
	var lines []string
	var cur strings.Builder
	curLen := 0
	flush := func() {
		if curLen > 0 {
			lines = append(lines, cur.String())
			cur.Reset()
			curLen = 0
		}
	}
	for _, w := range words {
		for len(w) > width {
			flush()
			lines = append(lines, w[:width])
			w = w[width:]
		}
		wlen := len(w)
		switch {
		case curLen == 0:
			cur.WriteString(w)
			curLen = wlen
		case curLen+1+wlen <= width:
			cur.WriteByte(' ')
			cur.WriteString(w)
			curLen += 1 + wlen
		default:
			flush()
			cur.WriteString(w)
			curLen = wlen
		}
	}
	flush()
	return lines
}
