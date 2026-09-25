package spanview

import (
	"strings"

	"github.com/graybeardcorp/weft/theme"
)

// Column widths for occurrenceColumns' fixed-width layout — LABEL is
// the only field that needs to flex with termWidth (TRACE_ID is a
// fixed 32-hex-char id, DURATION/STATUS are short). TRACE_ID rather
// than SPAN_ID: what the samples level is for is picking one occurrence
// and handing off into `trace <id>` for that specific request — see
// docs/plans/span-display.md's "Interactive drill-down" section. Within
// a single Phase 1 (per-trace) rollup every row shares the same trace
// ID; Phase 2's cross-trace aggregation is where this column earns its
// place.
const (
	occTraceIDWidth  = 32
	occDurationWidth = 9
	occStatusWidth   = 7
	occLabelMin      = 12
	occColSpacing    = 1
)

// occurrenceColumns lays occ out into fixed-width text — a header line
// and one line per occurrence — the samples-level counterpart to
// theme.SpanRollupColumns. singleTrace adds a SELF column (self/
// exclusive duration, see theme.SpanOccurrence) alongside the always-
// present inclusive DURATION: "LABEL TRACE_ID DURATION SELF STATUS"
// when true, "LABEL TRACE_ID DURATION STATUS" when false. This mirrors
// SpanRollupColumns' own singleTrace gating and for the same reason —
// self-time is only accurate when the line set is a complete single
// trace (Phase 1); Phase 2's cross-trace `trace spans` only fetches
// rows matching one span name, so it can never see cross-name children
// and a SELF value there would look complete but silently understate
// delegation (see theme.SpanOccurrence's doc comment). It's also the
// per-occurrence complement to the list level's TOTAL (sum of self
// time): TOTAL alone can't tell you whether a span name's time went
// into one slow call or several evenly slow ones — this is where that
// gets answered.
//
// Data rows are colored the same way SpanRollupColumns' are, and for
// the same reason: LABEL uses theme.TraceTagColor so a service badges
// the same color here as it does inline in the raw trace view and in
// theme.RenderTraceColumns' label column (that consistency is
// TraceTagColor's whole documented purpose); TRACE_ID is dimmed
// (Comment) since it's a reference value, not the primary read;
// DURATION/SELF match SpanRollupColumns' Number indigo; STATUS is
// green (DiffAdd — the palette's one semantic color, reserved for
// exactly this ok/error contrast) for "ok" and Error salmon for
// anything else. Cells end with theme.FgReset rather than a full reset
// so a caller can safely wrap a focused row in its own background/
// reverse-video highlight without an embedded reset canceling it early
// — see spanview/view.go's renderSamples and focusBg.
func occurrenceColumns(occ []theme.SpanOccurrence, termWidth int, singleTrace bool) (header string, rowTexts []string) {
	durCols := 1
	if singleTrace {
		durCols = 2
	}
	labelWidth := termWidth - occTraceIDWidth - durCols*occDurationWidth - occStatusWidth - (2+durCols)*occColSpacing
	if labelWidth < occLabelMin {
		labelWidth = occLabelMin
	}

	// theme.PadVisible/PadLeftVisible, not a byte-length pad: DURATION
	// formats via time.Duration.String(), which for microsecond values
	// (e.g. "669.006µs") is 9 visible characters but 10 bytes — a
	// byte-length check against occDurationWidth (9) would wrongly
	// truncate a value that already fits, silently dropping its
	// leading digit.
	pad, padLeft := theme.PadVisible, theme.PadLeftVisible
	color := func(s, sgr string) string { return sgr + s + theme.FgReset }
	sep := strings.Repeat(" ", occColSpacing)
	row := func(cells ...string) string { return strings.Join(cells, sep) }

	headerCells := []string{pad("LABEL", labelWidth), pad("TRACE_ID", occTraceIDWidth), padLeft("DURATION", occDurationWidth)}
	if singleTrace {
		headerCells = append(headerCells, padLeft("SELF", occDurationWidth))
	}
	headerCells = append(headerCells, pad("STATUS", occStatusWidth))
	header = row(headerCells...)

	rowTexts = make([]string, len(occ))
	for i, o := range occ {
		status := pad(o.Status, occStatusWidth)
		if o.Status == "ok" {
			status = color(status, theme.Palette().DiffAdd)
		} else {
			status = color(status, theme.Palette().Error)
		}
		cells := []string{
			color(pad(o.Label, labelWidth), theme.TraceTagColor(o.Label)),
			color(pad(o.TraceID, occTraceIDWidth), theme.Palette().Comment),
			color(padLeft(o.Duration.String(), occDurationWidth), theme.Palette().Number),
		}
		if singleTrace {
			cells = append(cells, color(padLeft(o.SelfDuration.String(), occDurationWidth), theme.Palette().Number))
		}
		cells = append(cells, status)
		rowTexts[i] = row(cells...)
	}
	return header, rowTexts
}
