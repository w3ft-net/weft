package spanview

import (
	"strings"

	"github.com/graybeardcorp/weft/theme"
)

// Column widths for occurrenceColumns' fixed-width layout — LABEL is
// the only field that needs to flex with termWidth (SPAN_ID is a fixed
// 16-hex-char id, DURATION/STATUS are short).
const (
	occSpanIDWidth   = 16
	occDurationWidth = 9
	occStatusWidth   = 7
	occLabelMin      = 12
	occColSpacing    = 1
)

// occurrenceColumns lays occ out into plain fixed-width text — a header
// line and one line per occurrence, "LABEL SPAN_ID DURATION STATUS" —
// the samples-level counterpart to theme.SpanRollupColumns.
func occurrenceColumns(occ []theme.SpanOccurrence, termWidth int) (header string, rowTexts []string) {
	labelWidth := termWidth - occSpanIDWidth - occDurationWidth - occStatusWidth - 3*occColSpacing
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
	row := func(label, spanID, duration, status string) string {
		return strings.Join([]string{
			pad(label, labelWidth),
			pad(spanID, occSpanIDWidth),
			padLeft(duration, occDurationWidth),
			pad(status, occStatusWidth),
		}, sep)
	}

	header = row("LABEL", "SPAN_ID", "DURATION", "STATUS")
	rowTexts = make([]string, len(occ))
	for i, o := range occ {
		rowTexts[i] = row(o.Label, o.SpanID, o.Duration.String(), o.Status)
	}
	return header, rowTexts
}
