package spanview

import (
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/theme"
)

func TestOccurrenceColumns_HeaderAndRows(t *testing.T) {
	occ := []theme.SpanOccurrence{
		{Label: "weft/shard-a", TraceID: "trace-abc-123", Duration: 12 * time.Millisecond, Status: "ok"},
		{Label: "weft/shard-b", TraceID: "trace-def-456", Duration: 34 * time.Millisecond, Status: "error"},
	}
	header, rows := occurrenceColumns(occ, 80)
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

func TestOccurrenceColumns_NarrowWidthFloorsLabelWidth(t *testing.T) {
	occ := []theme.SpanOccurrence{
		{Label: "weft/a-very-long-service-name-indeed", TraceID: "trace-abc-123", Duration: time.Millisecond, Status: "ok"},
	}
	_, rows := occurrenceColumns(occ, 20)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if len(rows[0]) < occLabelMin {
		t.Errorf("expected row width to respect occLabelMin floor, got len=%d: %q", len(rows[0]), rows[0])
	}
}
