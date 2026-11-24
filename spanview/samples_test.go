package spanview

import (
	"strings"
	"testing"
	"time"

	shellcore "github.com/graybeardcorp/warp/pkg/shell"
	"github.com/w3ft-net/weft/theme"
)

func TestOccurrenceColumns_HeaderAndRows(t *testing.T) {
	occ := []theme.SpanOccurrence{
		{Label: "weft/shard-a", TraceID: "trace-abc-123", Duration: 12 * time.Millisecond, Status: "ok"},
		{Label: "weft/shard-b", TraceID: "trace-def-456", Duration: 34 * time.Millisecond, Status: "error"},
	}
	header, rows := occurrenceColumns(occ, 80, false)
	for _, col := range []string{"LABEL", "TRACE_ID", "DURATION", "STATUS"} {
		if !strings.Contains(header, col) {
			t.Errorf("expected header to contain %q, got %q", col, header)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if !strings.Contains(rows[0], "weft/shard-a") || !strings.Contains(rows[0], "12ms") {
		t.Errorf("row 0 missing expected fields: %q", rows[0])
	}
	if !strings.Contains(rows[1], "error") {
		t.Errorf("row 1 missing status: %q", rows[1])
	}
}

// TestOccurrenceColumns_SingleTraceAddsSelfColumn is the samples-level
// counterpart to theme's TOTAL/aggregate gating: singleTrace=true adds
// a SELF column (self/exclusive duration) alongside the always-present
// inclusive DURATION — the per-occurrence detail that lets you tell
// whether a span name's list-level TOTAL came from one slow call or
// several evenly-slow ones. singleTrace=false (Phase 2's cross-trace
// samples) must never show SELF, for the same reason theme.
// SpanRollupColumns never shows a self-time number there: those rows
// can't see cross-name children, so a SELF value would look complete
// but silently understate delegation.
func TestOccurrenceColumns_SingleTraceAddsSelfColumn(t *testing.T) {
	occ := []theme.SpanOccurrence{
		{Label: "weft/shard-a", TraceID: "trace-abc-123", Duration: 12 * time.Millisecond, SelfDuration: 9 * time.Millisecond, Status: "ok"},
	}
	header, rows := occurrenceColumns(occ, 80, true)
	if !strings.Contains(header, "SELF") {
		t.Errorf("expected header to contain SELF when singleTrace, got %q", header)
	}
	if got := shellcore.StripANSI(rows[0]); !strings.Contains(got, "9ms") {
		t.Errorf("expected row to contain self duration 9ms, got %q", got)
	}

	header, rows = occurrenceColumns(occ, 80, false)
	if strings.Contains(header, "SELF") {
		t.Errorf("expected no SELF column when not singleTrace, got %q", header)
	}
	if strings.Contains(shellcore.StripANSI(rows[0]), "9ms") {
		t.Errorf("expected self duration not to appear when not singleTrace, got %q", shellcore.StripANSI(rows[0]))
	}
}

func TestOccurrenceColumns_NarrowWidthFloorsLabelWidth(t *testing.T) {
	occ := []theme.SpanOccurrence{
		{Label: "weft/a-very-long-service-name-indeed", TraceID: "trace-abc-123", Duration: time.Millisecond, Status: "ok"},
	}
	_, rows := occurrenceColumns(occ, 20, false)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	// Visible width, not byte length — rows are colored now (embedded
	// ANSI codes add bytes without adding columns), so len(rows[0])
	// alone would trivially clear occLabelMin regardless of whether the
	// floor logic (which runs on plain text before coloring) actually
	// held.
	if visLen := len(shellcore.StripANSI(rows[0])); visLen < occLabelMin {
		t.Errorf("expected row visible width to respect occLabelMin floor, got %d: %q", visLen, rows[0])
	}
}

// TestOccurrenceColumns_RowsAreReverseVideoSafe mirrors theme's
// TestSpanRollupColumns_RowsAreReverseVideoSafe: occurrenceColumns'
// rows must never contain a full SGR reset, only theme.FgReset, or
// view.go's renderSamples underline wrap around the focused row would
// get canceled early by an embedded reset.
func TestOccurrenceColumns_RowsAreReverseVideoSafe(t *testing.T) {
	occ := []theme.SpanOccurrence{
		{Label: "weft/shard-a", TraceID: "trace-abc-123", Duration: 12 * time.Millisecond, Status: "ok"},
		{Label: "weft/shard-b", TraceID: "trace-def-456", Duration: 34 * time.Millisecond, Status: "error"},
	}
	for _, singleTrace := range []bool{true, false} {
		_, rows := occurrenceColumns(occ, 80, singleTrace)
		for i, r := range rows {
			if strings.Contains(r, "\x1b[0m") {
				t.Errorf("singleTrace=%v: row %d contains a full SGR reset, which would cancel the focus underline early: %q", singleTrace, i, r)
			}
		}
	}
}

// TestOccurrenceColumns_MicrosecondDurationsKeepAllDigits is
// occurrenceColumns' sibling of theme's
// TestSpanRollupColumns_MicrosecondDurationsKeepAllDigits: DURATION
// goes through the same time.Duration.String() + fixed-width pad as
// SpanRollupColumns' duration columns, and occDurationWidth (9) is the
// same width where a byte-length check misjudges "669.006µs" (9
// visible chars, 10 bytes) as needing truncation.
func TestOccurrenceColumns_MicrosecondDurationsKeepAllDigits(t *testing.T) {
	occ := []theme.SpanOccurrence{
		{Label: "weft/api", TraceID: "trace-abc-123", Duration: 669006 * time.Nanosecond, Status: "ok"},
	}
	_, rows := occurrenceColumns(occ, 80, false)
	if got := shellcore.StripANSI(rows[0]); !strings.Contains(got, "669.006µs") {
		t.Errorf("expected full, untruncated duration %q in row, got %q", "669.006µs", got)
	}
}
