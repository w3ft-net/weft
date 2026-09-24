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

// occurrenceColumns lays occ out into plain fixed-width text — a header
// line and one line per occurrence, "LABEL TRACE_ID DURATION STATUS" —
// the samples-level counterpart to theme.SpanRollupColumns.
func occurrenceColumns(occ []theme.SpanOccurrence, termWidth int) (header string, rowTexts []string) {
	labelWidth := termWidth - occTraceIDWidth - occDurationWidth - occStatusWidth - 3*occColSpacing
	if labelWidth < occLabelMin {
		labelWidth = occLabelMin
	}

	pad := func(s string, w int) string {
		if len(s) > w {
			if w > 1 {
				return s[:w-1] + "…"
			}
			return s[:w]
		}
		return s + strings.Repeat(" ", w-len(s))
	}
	padLeft := func(s string, w int) string {
		if len(s) > w {
			return s[len(s)-w:]
		}
		return strings.Repeat(" ", w-len(s)) + s
	}
	sep := strings.Repeat(" ", occColSpacing)
	row := func(label, traceID, duration, status string) string {
		return strings.Join([]string{
			pad(label, labelWidth),
			pad(traceID, occTraceIDWidth),
			padLeft(duration, occDurationWidth),
			pad(status, occStatusWidth),
		}, sep)
	}

	header = row("LABEL", "TRACE_ID", "DURATION", "STATUS")
	rowTexts = make([]string, len(occ))
	for i, o := range occ {
		rowTexts[i] = row(o.Label, o.TraceID, o.Duration.String(), o.Status)
	}
	return header, rowTexts
}
