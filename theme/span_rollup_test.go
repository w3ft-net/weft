package theme

import (
	"strings"
	"testing"
	"time"
)

func spanDoneLine(name, traceID, spanID, parentID, duration, status string) string {
	return "span done name=" + name + " trace_id=" + traceID + " span_id=" + spanID +
		" parent_span_id=" + parentID + " duration=" + duration + " status=" + status
}

func spanErrorLine(name, traceID, spanID, errText string) string {
	return "span error name=" + name + " trace_id=" + traceID + " span_id=" + spanID + " err=" + errText
}

func TestRenderSpanRollup_GroupsByNameAndComputesStats(t *testing.T) {
	lines := []string{
		traceRow(1000, "weft/coordinator", spanDoneLine("shard.Query", "t1", "s1", "", "10ms", "ok")),
		traceRow(2000, "weft/shard-a", spanDoneLine("shard.Query", "t1", "s2", "s1", "20ms", "ok")),
		traceRow(3000, "weft/shard-b", spanDoneLine("shard.Query", "t1", "s3", "s1", "30ms", "ok")),
		traceRow(4000, "weft/coordinator", spanDoneLine("api.Handle", "t1", "s4", "", "5ms", "ok")),
	}

	rows := RenderSpanRollup(lines)
	if len(rows) != 2 {
		t.Fatalf("expected 2 distinct span names, got %d: %+v", len(rows), rows)
	}

	// Sorted by name: api.Handle before shard.Query.
	if rows[0].Name != "api.Handle" || rows[1].Name != "shard.Query" {
		t.Fatalf("expected sorted names [api.Handle shard.Query], got [%s %s]", rows[0].Name, rows[1].Name)
	}

	sq := rows[1]
	if sq.Count() != 3 {
		t.Fatalf("expected 3 occurrences for shard.Query, got %d", sq.Count())
	}
	if sq.Min != 10*time.Millisecond || sq.Max != 30*time.Millisecond {
		t.Errorf("expected min=10ms max=30ms, got min=%s max=%s", sq.Min, sq.Max)
	}
	if sq.Avg != 20*time.Millisecond {
		t.Errorf("expected avg=20ms, got %s", sq.Avg)
	}
}

func TestRenderSpanRollup_CountsErrorsByName(t *testing.T) {
	lines := []string{
		traceRow(1000, "weft/shard-a", spanDoneLine("shard.Query", "t1", "s1", "", "10ms", "error")),
		traceRow(1500, "weft/shard-a", spanErrorLine("shard.Query", "t1", "s1", "connection reset")),
	}

	rows := RenderSpanRollup(lines)
	if len(rows) != 1 {
		t.Fatalf("expected 1 span name, got %d", len(rows))
	}
	if rows[0].Errors != 1 {
		t.Errorf("expected 1 error, got %d", rows[0].Errors)
	}
	if rows[0].Count() != 1 {
		t.Errorf("expected 1 occurrence (the done line), got %d", rows[0].Count())
	}
}

func TestRenderSpanRollup_IgnoresNonSpanLines(t *testing.T) {
	lines := []string{
		"# 3 line(s) across 2 service(s), 2026-09-24T00:16:29Z to 2026-09-24T00:16:30Z",
		traceRow(1000, "weft/api", "handling request"),
		traceRow(2000, "weft/api", spanDoneLine("api.Handle", "t1", "s1", "", "1.5ms", "ok")),
	}

	rows := RenderSpanRollup(lines)
	if len(rows) != 1 {
		t.Fatalf("expected 1 span name, got %d: %+v", len(rows), rows)
	}
	if rows[0].Name != "api.Handle" {
		t.Errorf("expected api.Handle, got %s", rows[0].Name)
	}
}

func TestRenderSpanRollup_EmptyParentSpanID(t *testing.T) {
	// A top-level span logs parent_span_id= with nothing after the
	// equals sign — must still parse (the regex's \S* on that field).
	lines := []string{
		traceRow(1000, "weft/api", spanDoneLine("api.Handle", "t1", "s1", "", "1ms", "ok")),
	}
	rows := RenderSpanRollup(lines)
	if len(rows) != 1 || rows[0].Count() != 1 {
		t.Fatalf("expected span with empty parent_span_id to parse, got %+v", rows)
	}
}

func TestRenderSpanRollupTable_EmptyRowsRendersPlaceholder(t *testing.T) {
	out := RenderSpanRollupTable(nil, 80)
	if len(out) != 1 || !strings.Contains(stripANSI(out[0]), "no spans found") {
		t.Fatalf("expected a single placeholder line, got %v", out)
	}
}

func TestRenderSpanRollupTable_HeaderAndColumns(t *testing.T) {
	lines := []string{
		traceRow(1000, "weft/api", spanDoneLine("api.Handle", "t1", "s1", "", "1.5ms", "ok")),
	}
	rows := RenderSpanRollup(lines)
	out := RenderSpanRollupTable(rows, 100)
	if len(out) != 2 {
		t.Fatalf("expected header + 1 row, got %d lines: %v", len(out), out)
	}
	header := stripANSI(out[0])
	for _, col := range []string{"NAME", "COUNT", "AVG", "P50", "P99", "MIN", "MAX", "ERRORS"} {
		if !strings.Contains(header, col) {
			t.Errorf("expected header to contain %q, got %q", col, header)
		}
	}
	if !strings.Contains(out[1], "api.Handle") {
		t.Errorf("expected row to contain span name, got %q", out[1])
	}
}

func TestPercentileDuration_NearestRank(t *testing.T) {
	sorted := []time.Duration{
		1 * time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond,
		4 * time.Millisecond, 5 * time.Millisecond,
	}
	if got := percentileDuration(sorted, 0.50); got != 3*time.Millisecond {
		t.Errorf("p50: expected 3ms, got %s", got)
	}
	if got := percentileDuration(sorted, 0.99); got != 5*time.Millisecond {
		t.Errorf("p99: expected 5ms, got %s", got)
	}
}
