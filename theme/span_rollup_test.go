package theme

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func spanDoneLine(name, traceID, spanID, parentID string, duration time.Duration, status string) string {
	return "span done name=" + name + " trace_id=" + traceID + " span_id=" + spanID +
		" parent_span_id=" + parentID + " duration=" + duration.String() +
		" duration_ns=" + strconv.FormatInt(duration.Nanoseconds(), 10) + " status=" + status
}

func spanErrorLine(name, traceID, spanID, errText string) string {
	return "span error name=" + name + " trace_id=" + traceID + " span_id=" + spanID + " err=" + errText
}

// TestRenderSpanRollup_MatchesRealIngestedLineWithTimestampPrefix is the
// regression test for a real production gap found while dogfooding
// span-display end to end (2026-09-25): every existing test in this
// file built its span-line fixtures as a BARE "span done name=..."
// string with nothing before it — but that's not what a real ingested
// row's `raw` column ever contains. weft-go's Span.End()/RecordError
// call log.Printf, which prepends Go's default timestamp
// ("2026/09/24 16:24:03 ") to every line; ingest.Parser.ParseLine sets
// row.Raw to that FULL original line, timestamp included, never just
// the message. spanDoneRe/spanErrorRe were anchored with `^` — assuming
// the line starts directly with "span done"/"span error" — so they
// never matched a single real ingested span line in production: `trace
// <id> --spans` and `trace spans` (which share this exact parser) both
// silently rendered "no spans found" against perfectly real, present
// data the whole time this shipped, confirmed live via `trace spans`
// returning empty despite `tail | awk 'DURATION_NS > 0'` finding 26
// real matching rows in the same window.
func TestRenderSpanRollup_MatchesRealIngestedLineWithTimestampPrefix(t *testing.T) {
	// Exact shape of a real ingested weft/coordinator row's raw text —
	// copied from a live production trace.
	realRaw := "2026/09/24 16:24:03 span done name=coordinator.Execute trace_id=cdb1c1463758ecb93aab9621ea604ed8 " +
		"span_id=278356b7a799fb18 parent_span_id=6f7eb239b86d3267 duration=49.114313ms duration_ns=49114313 status=ok"
	lines := []string{traceRow(1000, "weft/coordinator", realRaw)}

	rows := RenderSpanRollup(lines)
	if len(rows) != 1 {
		t.Fatalf("expected 1 span name parsed from a realistic timestamp-prefixed line, got %d rows: %+v", len(rows), rows)
	}
	if rows[0].Name != "coordinator.Execute" {
		t.Errorf("Name: got %q, want coordinator.Execute", rows[0].Name)
	}
	if rows[0].Count() != 1 {
		t.Fatalf("expected 1 occurrence, got %d", rows[0].Count())
	}
	if rows[0].Occurrences[0].Duration != 49114313 {
		t.Errorf("Duration: got %v, want 49114313ns", rows[0].Occurrences[0].Duration)
	}
	if rows[0].Occurrences[0].TraceID != "cdb1c1463758ecb93aab9621ea604ed8" {
		t.Errorf("TraceID: got %q", rows[0].Occurrences[0].TraceID)
	}
}

// TestRenderSpanRollup_MatchesRealIngestedErrorLineWithTimestampPrefix
// is the "span error" sibling of the above — same real-world shape,
// same anchoring bug.
func TestRenderSpanRollup_MatchesRealIngestedErrorLineWithTimestampPrefix(t *testing.T) {
	realRaw := "2026/09/24 16:24:03 span error name=shard.Query trace_id=cdb1c1463758ecb93aab9621ea604ed8 " +
		"span_id=278356b7a799fb18 err=connection reset"
	lines := []string{traceRow(1000, "weft/shard-a", realRaw)}

	rows := RenderSpanRollup(lines)
	if len(rows) != 1 {
		t.Fatalf("expected 1 span name parsed from a realistic timestamp-prefixed error line, got %d rows: %+v", len(rows), rows)
	}
	if rows[0].Errors != 1 {
		t.Errorf("Errors: got %d, want 1", rows[0].Errors)
	}
}

func TestRenderSpanRollup_GroupsByNameAndComputesStats(t *testing.T) {
	lines := []string{
		traceRow(1000, "weft/coordinator", spanDoneLine("shard.Query", "t1", "s1", "", 10*time.Millisecond, "ok")),
		traceRow(2000, "weft/shard-a", spanDoneLine("shard.Query", "t1", "s2", "s1", 20*time.Millisecond, "ok")),
		traceRow(3000, "weft/shard-b", spanDoneLine("shard.Query", "t1", "s3", "s1", 30*time.Millisecond, "ok")),
		traceRow(4000, "weft/coordinator", spanDoneLine("api.Handle", "t1", "s4", "", 5*time.Millisecond, "ok")),
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

// TestRenderSpanRollup_OccurrencesCarryTraceID: each occurrence's
// TraceID comes from its own row's trace_id= field, not a single
// shared value — the field Phase 1's per-trace rollup doesn't need
// (every row shares one trace ID there) but Phase 2's cross-trace
// aggregation (docs/plans/span-display.md) depends on, to let the
// interactive samples level hand off into `trace <id>` for a specific
// occurrence.
func TestRenderSpanRollup_OccurrencesCarryTraceID(t *testing.T) {
	lines := []string{
		traceRow(1000, "weft/shard-a", spanDoneLine("shard.Query", "trace-aaa", "s1", "", 10*time.Millisecond, "ok")),
		traceRow(2000, "weft/shard-b", spanDoneLine("shard.Query", "trace-bbb", "s2", "", 20*time.Millisecond, "ok")),
	}
	rows := RenderSpanRollup(lines)
	if len(rows) != 1 || rows[0].Count() != 2 {
		t.Fatalf("expected 1 span name with 2 occurrences, got %+v", rows)
	}
	got := []string{rows[0].Occurrences[0].TraceID, rows[0].Occurrences[1].TraceID}
	want := []string{"trace-aaa", "trace-bbb"}
	if got[0] != want[0] || got[1] != want[1] {
		t.Errorf("occurrence TraceIDs: got %v, want %v", got, want)
	}
}

func TestRenderSpanRollup_CountsErrorsByName(t *testing.T) {
	lines := []string{
		traceRow(1000, "weft/shard-a", spanDoneLine("shard.Query", "t1", "s1", "", 10*time.Millisecond, "error")),
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
		traceRow(2000, "weft/api", spanDoneLine("api.Handle", "t1", "s1", "", 1500*time.Microsecond, "ok")),
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
		traceRow(1000, "weft/api", spanDoneLine("api.Handle", "t1", "s1", "", 1*time.Millisecond, "ok")),
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
		traceRow(1000, "weft/api", spanDoneLine("api.Handle", "t1", "s1", "", 1500*time.Microsecond, "ok")),
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
