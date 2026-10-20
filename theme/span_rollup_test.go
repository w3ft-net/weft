package theme

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func spanDoneRow(tsNs int64, label, name, traceID, spanID, parentID string, duration time.Duration, status string) string {
	ns, svc, _ := strings.Cut(label, "/")
	out, _ := json.Marshal(map[string]any{
		"name": name, "namespace": ns, "service": svc, "trace_id": traceID, "span_id": spanID,
		"parent_id": parentID, "dur_ns": duration.Nanoseconds(), "ts_ns": tsNs, "level": "INFO",
	})
	return string(out)
}

func spanErrorRow(tsNs int64, label, name, traceID, spanID string) string {
	ns, svc, _ := strings.Cut(label, "/")
	out, _ := json.Marshal(map[string]any{
		"name": name, "namespace": ns, "service": svc, "trace_id": traceID, "span_id": spanID,
		"parent_id": nil, "dur_ns": nil, "ts_ns": tsNs, "level": "ERROR",
	})
	return string(out)
}

// A span written by the SDK has its name, IDs and duration in columns and
// only a JSON message as its body; the rollup reads the columns.
func TestRenderSpanRollup_ReadsSpanColumns(t *testing.T) {
	lines := []string{spanDoneRow(1000, "acme/api", "coordinator.Execute", "cdb1c1463758ecb93aab9621ea604ed8", "278356b7a799fb18", "6f7eb239b86d3267", 49114313*time.Nanosecond, "ok")}

	rows := RenderSpanRollup(lines)
	if len(rows) != 1 || rows[0].Name != "coordinator.Execute" || rows[0].Count() != 1 {
		t.Fatalf("rolled up %+v", rows)
	}
	if rows[0].Occurrences[0].Duration != 49114313 {
		t.Errorf("Duration: got %v, want 49114313ns", rows[0].Occurrences[0].Duration)
	}
	if rows[0].Occurrences[0].TraceID != "cdb1c1463758ecb93aab9621ea604ed8" {
		t.Errorf("TraceID: got %q", rows[0].Occurrences[0].TraceID)
	}
	if rows[0].Occurrences[0].Label != "acme/api" {
		t.Errorf("Label: got %q", rows[0].Occurrences[0].Label)
	}
}

func TestRenderSpanRollup_ErrorRowCountsAgainstNameOnly(t *testing.T) {
	rows := RenderSpanRollup([]string{spanErrorRow(1000, "acme/shard-a", "shard.Query", "t1", "s1")})
	if len(rows) != 1 || rows[0].Errors != 1 || rows[0].Count() != 0 {
		t.Fatalf("expected one name with 1 error and no finished spans, got %+v", rows)
	}
}

func TestRenderSpanAggregate_ReadsJSONLines(t *testing.T) {
	lines := []string{
		`{"name":"db.query","count":18,"errors":2,"total_ns":900,"avg_ns":50,"p50_ns":40,"p99_ns":90,"min_ns":10,"max_ns":100}`,
		`{"name":"db.query","namespace":"acme","service":"api","count":1,"errors":0,"total_ns":5,"avg_ns":5,"p50_ns":5,"p99_ns":5,"min_ns":5,"max_ns":5}`,
		`not json`,
	}
	rows := RenderSpanAggregate(lines)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}
	if rows[0].Count() != 18 || rows[0].Errors != 2 || rows[0].P99 != 90 || rows[0].Label != "" {
		t.Errorf("row 0 = %+v", rows[0])
	}
	if rows[1].Label != "acme/api" || rows[1].displayName() != "db.query (acme/api)" {
		t.Errorf("row 1 = %+v", rows[1])
	}
}

func TestRenderSpanSamples_SlowestFirst(t *testing.T) {
	lines := []string{
		spanDoneRow(1, "acme/api", "db.query", "t1", "s1", "", 10*time.Millisecond, "ok"),
		spanDoneRow(2, "acme/api", "db.query", "t2", "s2", "", 30*time.Millisecond, "ok"),
		spanDoneRow(3, "acme/api", "db.query", "t3", "s3", "", 20*time.Millisecond, "ok"),
	}
	occ := RenderSpanSamples(lines, 2)
	if len(occ) != 2 || occ[0].TraceID != "t2" || occ[1].TraceID != "t3" {
		t.Fatalf("got %+v, want t2 then t3", occ)
	}
}

func TestRenderSpanRollup_GroupsByNameAndComputesStats(t *testing.T) {
	lines := []string{
		spanDoneRow(1000, "weft/coordinator", "shard.Query", "t1", "s1", "", 10*time.Millisecond, "ok"),
		spanDoneRow(2000, "weft/shard-a", "shard.Query", "t1", "s2", "s1", 20*time.Millisecond, "ok"),
		spanDoneRow(3000, "weft/shard-b", "shard.Query", "t1", "s3", "s1", 30*time.Millisecond, "ok"),
		spanDoneRow(4000, "weft/coordinator", "api.Handle", "t1", "s4", "", 5*time.Millisecond, "ok"),
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
		spanDoneRow(1000, "weft/shard-a", "shard.Query", "trace-aaa", "s1", "", 10*time.Millisecond, "ok"),
		spanDoneRow(2000, "weft/shard-b", "shard.Query", "trace-bbb", "s2", "", 20*time.Millisecond, "ok"),
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
		spanDoneRow(1000, "weft/shard-a", "shard.Query", "t1", "s1", "", 10*time.Millisecond, "error"),
		spanErrorRow(1500, "weft/shard-a", "shard.Query", "t1", "s1"),
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
		spanDoneRow(2000, "weft/api", "api.Handle", "t1", "s1", "", 1500*time.Microsecond, "ok"),
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
		spanDoneRow(1000, "weft/api", "api.Handle", "t1", "s1", "", 1*time.Millisecond, "ok"),
	}
	rows := RenderSpanRollup(lines)
	if len(rows) != 1 || rows[0].Count() != 1 {
		t.Fatalf("expected span with empty parent_span_id to parse, got %+v", rows)
	}
}

// TestRenderSpanRollup_SelfTimeSubtractsSequentialChild is the basic
// self-time case: one child fully inside its parent's own time window.
// Parent p1 runs [0,100ms] (end=100ms, duration=100ms); child c1 runs
// [20ms,70ms] (end=70ms, duration=50ms) — entirely within p1's window,
// no overlap with any sibling to worry about. Self(p1) should be
// 100ms - 50ms = 50ms; the leaf child's self equals its own duration.
func TestRenderSpanRollup_SelfTimeSubtractsSequentialChild(t *testing.T) {
	lines := []string{
		spanDoneRow(100_000_000, "weft/api", "api.Handle", "t1", "p1", "", 100*time.Millisecond, "ok"),
		spanDoneRow(70_000_000, "weft/coordinator", "coordinator.Execute", "t1", "c1", "p1", 50*time.Millisecond, "ok"),
	}
	rows := RenderSpanRollup(lines)
	byName := map[string]SpanRollupRow{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	parent := byName["api.Handle"]
	if parent.Count() != 1 || parent.Occurrences[0].SelfDuration != 50*time.Millisecond {
		t.Fatalf("expected parent self=50ms, got %+v", parent.Occurrences)
	}
	child := byName["coordinator.Execute"]
	if child.Count() != 1 || child.Occurrences[0].SelfDuration != 50*time.Millisecond {
		t.Fatalf("expected leaf child self==duration (50ms), got %+v", child.Occurrences)
	}
}

// TestRenderSpanRollup_SelfTimeUnionsOverlappingChildren is the
// regression test for the naive-summation pitfall the APM research
// this feature is based on called out explicitly (OTel's spanmetrics
// connector, New Relic's exclusive-duration calc — both merge
// overlapping child intervals rather than summing durations): parent
// p2 runs [0,100ms]; two CONCURRENT children run [10ms,70ms] (60ms)
// and [40ms,100ms] (60ms) — they overlap in [40ms,70ms]. Naive
// summation would compute covered=120ms against a 100ms parent,
// clamping self to 0 — wrong. The correct union of the two intervals
// is [10ms,100ms] = 90ms covered, so self(p2) should be 10ms, not 0.
// This is exactly the fan-out shape weft's own spans have (coordinator
// dispatching to multiple shards concurrently), not a contrived edge
// case.
func TestRenderSpanRollup_SelfTimeUnionsOverlappingChildren(t *testing.T) {
	lines := []string{
		spanDoneRow(100_000_000, "weft/coordinator", "coordinator.Execute", "t1", "p2", "", 100*time.Millisecond, "ok"),
		spanDoneRow(70_000_000, "weft/shard-a", "shard.Query", "t1", "c1", "p2", 60*time.Millisecond, "ok"),
		spanDoneRow(100_000_000, "weft/shard-b", "shard.Query", "t1", "c2", "p2", 60*time.Millisecond, "ok"),
	}
	rows := RenderSpanRollup(lines)
	var parentSelf time.Duration
	for _, r := range rows {
		for _, o := range r.Occurrences {
			if o.SpanID == "p2" {
				parentSelf = o.SelfDuration
			}
		}
	}
	if parentSelf != 10*time.Millisecond {
		t.Errorf("expected self=10ms from unioned (not naively summed) overlapping children, got %s", parentSelf)
	}
}

// TestRenderSpanRollup_SelfTimeLeafEqualsInclusive: a span with no
// children found in the line set (a true leaf, or one whose children
// simply aren't present — see SpanOccurrence's doc comment on
// completeness) has nothing to subtract, so self must equal inclusive
// duration exactly.
func TestRenderSpanRollup_SelfTimeLeafEqualsInclusive(t *testing.T) {
	lines := []string{
		spanDoneRow(1000, "weft/shard-a", "shard.Query", "t1", "s1", "", 15*time.Millisecond, "ok"),
	}
	rows := RenderSpanRollup(lines)
	if rows[0].Occurrences[0].SelfDuration != 15*time.Millisecond {
		t.Errorf("expected leaf self==duration (15ms), got %s", rows[0].Occurrences[0].SelfDuration)
	}
}

func TestRenderSpanRollupTable_EmptyRowsRendersPlaceholder(t *testing.T) {
	out := RenderSpanRollupTable(nil, 80, true)
	if len(out) != 1 || !strings.Contains(stripANSI(out[0]), "no spans found") {
		t.Fatalf("expected a single placeholder line, got %v", out)
	}
}

// TestRenderSpanRollupTable_SingleTraceShowsTotalNotAggregates is
// Phase 1's column set: `trace <id> --spans` fetches one complete
// trace, where a span name typically occurs once or a handful of
// times — not enough samples for AVG/P50/P99/MIN/MAX to mean anything
// as a distribution. TOTAL (sum of self time, see SpanRollupRow) is
// what actually answers "how much of this request's time did this
// span name account for," so singleTrace=true shows COUNT/TOTAL/ERRORS
// and omits the aggregate columns entirely — not just an unfilled
// AVG/P50/etc, but their absence. BAR's header cell is also always
// blank (no "BAR" text) in both modes.
func TestRenderSpanRollupTable_SingleTraceShowsTotalNotAggregates(t *testing.T) {
	lines := []string{
		spanDoneRow(1000, "weft/api", "api.Handle", "t1", "s1", "", 1500*time.Microsecond, "ok"),
	}
	rows := RenderSpanRollup(lines)
	out := RenderSpanRollupTable(rows, 100, true)
	if len(out) != 2 {
		t.Fatalf("expected header + 1 row, got %d lines: %v", len(out), out)
	}
	header := stripANSI(out[0])
	for _, col := range []string{"NAME", "COUNT", "TOTAL", "ERRORS"} {
		if !strings.Contains(header, col) {
			t.Errorf("expected header to contain %q, got %q", col, header)
		}
	}
	for _, absent := range []string{"BAR", "AVG", "SELF", "P50", "P99", "MIN", "MAX"} {
		if strings.Contains(header, absent) {
			t.Errorf("expected header NOT to contain %q in single-trace mode, got %q", absent, header)
		}
	}
	if !strings.Contains(out[1], "api.Handle") {
		t.Errorf("expected row to contain span name, got %q", out[1])
	}
}

// TestRenderSpanRollupTable_CrossTraceShowsAggregatesNotTotal is
// Phase 2's side: `trace spans [name]` fans out across many traces, so
// the AVG/P50/P99/MIN/MAX distribution IS meaningful — and TOTAL
// (self-time) can't be computed safely at all, since Phase 2 only
// fetches rows matching one span name and so can never see cross-name
// children (see SpanOccurrence's doc comment). singleTrace=false must
// show the full aggregate set and never a TOTAL column.
func TestRenderSpanRollupTable_CrossTraceShowsAggregatesNotTotal(t *testing.T) {
	lines := []string{
		spanDoneRow(1000, "weft/api", "api.Handle", "t1", "s1", "", 1500*time.Microsecond, "ok"),
	}
	rows := RenderSpanRollup(lines)
	out := RenderSpanRollupTable(rows, 100, false)
	header := stripANSI(out[0])
	for _, col := range []string{"NAME", "COUNT", "AVG", "P50", "P99", "MIN", "MAX", "ERRORS"} {
		if !strings.Contains(header, col) {
			t.Errorf("expected header to contain %q, got %q", col, header)
		}
	}
	for _, absent := range []string{"BAR", "TOTAL"} {
		if strings.Contains(header, absent) {
			t.Errorf("expected header NOT to contain %q in cross-trace mode, got %q", absent, header)
		}
	}
}

// TestSpanRollupColumns_RowsAreReverseVideoSafe locks in the property
// spanview's focused-row rendering depends on: a colored data row must
// never contain a full SGR reset ("\x1b[0m"), only fgReset
// ("\x1b[39;22m"). A full reset mid-row would cancel a caller's
// surrounding "\x1b[7m" reverse-video wrap early (see spanview/
// view.go's renderList), silently un-reversing the rest of a focused
// row — this test would catch that regression even though it isn't
// visible from string-content assertions alone.
func TestSpanRollupColumns_RowsAreReverseVideoSafe(t *testing.T) {
	lines := []string{
		spanDoneRow(1000, "weft/shard-a", "shard.Query", "t1", "s1", "", 10*time.Millisecond, "ok"),
		spanErrorRow(1000, "weft/shard-a", "shard.Query", "t2", "s2"),
	}
	rows := RenderSpanRollup(lines)
	_, rowTexts := SpanRollupColumns(rows, 100, true)
	for i, r := range rowTexts {
		if strings.Contains(r, "\x1b[0m") {
			t.Errorf("row %d contains a full SGR reset, which would cancel an outer reverse-video wrap: %q", i, r)
		}
	}
}

// TestRenderSpanBar_ScalesRelativeToMax checks the AVG-relative bar:
// the row with the larger average duration should render more filled
// cells than the row with the smaller one, and a single row with
// nothing to compare against should render no filled cells at all
// (matching renderGapBar's "no misleading sliver" convention).
func TestRenderSpanBar_ScalesRelativeToMax(t *testing.T) {
	fullyFilled := stripANSI(renderSpanBar(10*time.Millisecond, 10*time.Millisecond, spanBarWidth))
	halfFilled := stripANSI(renderSpanBar(5*time.Millisecond, 10*time.Millisecond, spanBarWidth))
	if strings.Count(fullyFilled, "█") <= strings.Count(halfFilled, "█") {
		t.Errorf("expected the larger average to render more filled cells: full=%q half=%q", fullyFilled, halfFilled)
	}

	solo := stripANSI(renderSpanBar(5*time.Millisecond, 0, spanBarWidth))
	if strings.Contains(solo, "█") {
		t.Errorf("expected no filled cells when maxAvg is 0, got %q", solo)
	}
}

// TestSpanRollupColumns_RowWidthMatchesTermWidthExactly is the
// regression test for the width bug: an earlier version of nameWidth's
// budget undercounted the cross-trace 9-field row by one duration-width
// column and one separator (it has 5 duration columns —
// avg/p50/p99/min/max, not 4 — needing 8 separators for 9 fields, not
// 7), leaving every row 10 visible characters wider than termWidth. In
// a redrawing interactive view (spanview) that overflow doesn't just
// look wrong — it wraps onto the next line and throws off the next
// frame's cursor positioning, which is what actually produced the
// garbled output. This checks the header and every data row's VISIBLE
// width (stripping color) is exactly termWidth, for both column sets
// (singleTrace's 5-field NAME/BAR/COUNT/TOTAL/ERRORS and cross-trace's
// 9-field layout) and both a wide and a name-truncating narrow width.
func TestSpanRollupColumns_RowWidthMatchesTermWidthExactly(t *testing.T) {
	lines := []string{
		spanDoneRow(1000, "weft/api", "api.access", "t1", "s1", "", 669006*time.Nanosecond, "ok"),
		spanDoneRow(1000, "weft/coordinator", "coordinator.Execute", "t2", "s2", "", 332681*time.Nanosecond, "ok"),
		spanDoneRow(1000, "weft/shard-a", "shard.Query", "t3", "s3", "", 102305*time.Nanosecond, "ok"),
		spanDoneRow(1000, "weft/shard-a", "shard.Query", "t4", "s4", "", 408665*time.Nanosecond, "ok"),
	}
	rows := RenderSpanRollup(lines)
	for _, singleTrace := range []bool{true, false} {
		durCols, seps := 5, 8
		if singleTrace {
			durCols, seps = 1, 4
		}
		// The fixed (non-NAME) column budget plus spanNameMin's floor:
		// below this, nameWidth gets floored to spanNameMin and the row
		// legitimately exceeds termWidth (a deliberate "some name is
		// better than none" tradeoff, not the bug this test targets) —
		// so the honest expected width is max(w, floorWidth), not always w.
		floorWidth := spanBarWidth + spanCountWidth + durCols*spanDurWidth + spanErrWidth + seps*spanColSpacing + spanNameMin
		for _, w := range []int{100, 60} {
			want := w
			if want < floorWidth {
				want = floorWidth
			}
			header, rowTexts := SpanRollupColumns(rows, w, singleTrace)
			// Rune count, not len(): stripped rows still contain multi-byte
			// runes (the bar's "█", durations' "µ"), which len() would count
			// as multiple bytes each and misreport as an overflow that isn't
			// really there.
			if got := len([]rune(stripANSI(header))); got != want {
				t.Errorf("singleTrace=%v termWidth=%d: header visible width = %d, want %d: %q", singleTrace, w, got, want, header)
			}
			for i, r := range rowTexts {
				if got := len([]rune(stripANSI(r))); got != want {
					t.Errorf("singleTrace=%v termWidth=%d: row %d visible width = %d, want %d: %q", singleTrace, w, i, got, want, r)
				}
			}
		}
	}
}

// TestSpanRollupColumns_MicrosecondDurationsKeepAllDigits is the
// regression test for the byte-vs-visible-width truncation bug:
// time.Duration.String() renders a microsecond value like "669.006µs"
// as 9 VISIBLE characters but 10 bytes (µ is a 2-byte UTF-8 rune), and
// spanDurWidth is 9 — a byte-length check (len(s) > w) wrongly decided
// that 9-visible-char, already-exactly-fitting string needed
// truncating, silently dropping its leading digit ("69.006µs"). Every
// duration column (AVG/P50/P99/MIN/MAX) goes through the same pad
// function, so this only needs to check one.
func TestSpanRollupColumns_MicrosecondDurationsKeepAllDigits(t *testing.T) {
	lines := []string{
		spanDoneRow(1000, "weft/api", "api.access", "t1", "s1", "", 669006*time.Nanosecond, "ok"),
	}
	rows := RenderSpanRollup(lines)
	_, rowTexts := SpanRollupColumns(rows, 100, true)
	if !strings.Contains(stripANSI(rowTexts[0]), "669.006µs") {
		t.Errorf("expected full, untruncated duration %q in row, got %q", "669.006µs", stripANSI(rowTexts[0]))
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
