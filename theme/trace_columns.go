package theme

import (
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
	traceColumnSpacing = 1 // single space between each of the 3 columns
)

// RenderTraceColumns re-renders the delimited rows compute.LocalComputeJob's
// emitTraceTagged emits for a bare `trace <id>` ("<ts_unix_ns>\t<namespace/
// service>\t<raw message>") into a 3-column display sized to termWidth:
//
//	<gap histogram, right-aligned> <service badge> <message, wrapped>
//
// The bar in column 1 is scaled against the largest gap in this trace, so
// it reads as a relative "where did the time go between log lines" view
// rather than an absolute duration axis. A line that doesn't match the
// exact 3-field tab-delimited shape (the "# N line(s)..." summary line
// emitTraceTagged also emits, "# no lines found", or anything else) is
// returned unchanged in place — this makes the function safe to call on
// the full line set unconditionally rather than needing the caller to
// separate summary/data lines first.
func RenderTraceColumns(lines []string, termWidth int) []string {
	type parsedLine struct {
		raw    string // original text, used verbatim for passthrough lines
		isData bool
		tsNs   int64
		label  string
		msg    string
	}

	parsed := make([]parsedLine, len(lines))
	for i, l := range lines {
		parts := strings.SplitN(l, "\t", 3)
		if len(parts) != 3 {
			parsed[i] = parsedLine{raw: l}
			continue
		}
		ts, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			parsed[i] = parsedLine{raw: l}
			continue
		}
		parsed[i] = parsedLine{isData: true, tsNs: ts, label: parts[1], msg: parts[2]}
	}

	// First pass over data rows only: gaps (relative to the previous
	// data row, not the previous output line — a passthrough line in
	// between doesn't reset the clock) and the widest label, so both
	// the bar scale and the badge column width are known before any
	// rendering happens.
	gaps := make([]int64, len(parsed))
	var maxGap int64
	var prevTs int64
	haveData := false
	firstData := true
	labelWidth := traceLabelMinWidth
	for i, p := range parsed {
		if !p.isData {
			continue
		}
		haveData = true
		if !firstData {
			gap := p.tsNs - prevTs
			if gap < 0 {
				gap = 0
			}
			gaps[i] = gap
			if gap > maxGap {
				maxGap = gap
			}
		}
		firstData = false
		prevTs = p.tsNs
		if w := len(p.label); w > labelWidth {
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
	if msgWidth < traceMinMsgWidth {
		msgWidth = traceMinMsgWidth
	}

	blankGapCol := strings.Repeat(" ", gapColWidth)
	blankLabelCol := strings.Repeat(" ", labelWidth)
	sep := strings.Repeat(" ", traceColumnSpacing)

	out := make([]string, 0, len(lines))
	for i, p := range parsed {
		if !p.isData {
			out = append(out, p.raw)
			continue
		}
		bar := renderGapBar(gaps[i], maxGap, gapColWidth)
		badge := renderTraceLabelColumn(p.label, labelWidth)
		for j, wrapped := range wrapText(p.msg, msgWidth) {
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
