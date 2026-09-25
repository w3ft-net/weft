package theme

import (
	"fmt"
	"strings"
	"testing"
)

func traceRow(tsUnixNs int64, label, msg string) string {
	return fmt.Sprintf("%d\t%s\t%s", tsUnixNs, label, msg)
}

// stripANSI removes color codes so tests can assert on visible text
// without hardcoding escape sequences.
func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		if r == '\x1b' {
			inEsc = true
			continue
		}
		if inEsc {
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func TestRenderTraceColumns_NonDataLinesPassThroughUnchanged(t *testing.T) {
	lines := []string{
		"# 2 line(s) across 1 service(s), 2026-09-24T00:16:29Z to 2026-09-24T00:16:29Z",
	}
	got := RenderTraceColumns(lines, 80)
	if len(got) != 1 || got[0] != lines[0] {
		t.Fatalf("expected summary line unchanged, got %v", got)
	}
}

func TestRenderTraceColumns_FirstRowHasNoGapBar(t *testing.T) {
	lines := []string{
		traceRow(1000, "weft/cold-shard", "first line"),
	}
	got := RenderTraceColumns(lines, 80)
	if len(got) != 1 {
		t.Fatalf("expected 1 output line, got %d: %v", len(got), got)
	}
	plain := stripANSI(got[0])
	if strings.Contains(plain, "█") {
		t.Errorf("first row should have an empty (zero-gap) bar, got %q", plain)
	}
	if !strings.Contains(plain, "first line") {
		t.Errorf("expected message text in output, got %q", plain)
	}
}

func TestRenderTraceColumns_BarScalesWithGap(t *testing.T) {
	lines := []string{
		traceRow(0, "weft/coordinator", "a"),
		traceRow(1_000_000, "weft/coordinator", "small gap"),   // +1ms
		traceRow(1_000_000_000, "weft/coordinator", "big gap"), // +999ms, much bigger
	}
	got := RenderTraceColumns(lines, 80)
	if len(got) != 3 {
		t.Fatalf("expected 3 output lines, got %d: %v", len(got), got)
	}
	barLen := func(s string) int {
		return strings.Count(stripANSI(s), "█")
	}
	smallBar := barLen(got[1])
	bigBar := barLen(got[2])
	if bigBar <= smallBar {
		t.Errorf("expected the bigger gap to render a longer bar: small=%d big=%d", smallBar, bigBar)
	}
}

func TestRenderTraceColumns_WrapsLongMessagesWithBlankContinuationColumns(t *testing.T) {
	longMsg := strings.Repeat("word ", 30) // way longer than any reasonable width
	lines := []string{
		traceRow(0, "weft/api", longMsg),
	}
	got := RenderTraceColumns(lines, 40)
	if len(got) < 2 {
		t.Fatalf("expected the long message to wrap into multiple lines, got %d: %v", len(got), got)
	}
	// Every output line must fit within the requested terminal width.
	for i, l := range got {
		if w := len([]rune(stripANSI(l))); w > 40 {
			t.Errorf("line %d is %d chars wide, wider than termWidth=40: %q", i, w, stripANSI(l))
		}
	}
	// Continuation lines shouldn't repeat the service badge text.
	for i := 1; i < len(got); i++ {
		if strings.Contains(stripANSI(got[i]), "weft/api") {
			t.Errorf("continuation line %d unexpectedly repeats the service badge: %q", i, stripANSI(got[i]))
		}
	}
}

func TestRenderTraceColumns_PreservesRowOrderAndCount(t *testing.T) {
	lines := []string{
		"# 3 line(s) across 2 service(s), t0 to t2",
		traceRow(0, "weft/api", "one"),
		traceRow(500_000_000, "weft/coordinator", "two"),
		traceRow(1_000_000_000, "weft/cold-shard", "three"),
	}
	got := RenderTraceColumns(lines, 100)
	if len(got) != 4 {
		t.Fatalf("expected 4 output lines (1 summary + 3 data, no wrapping at width 100), got %d: %v", len(got), got)
	}
	if got[0] != lines[0] {
		t.Errorf("summary line should pass through first and unchanged, got %q", got[0])
	}
	for i, want := range []string{"one", "two", "three"} {
		if !strings.Contains(stripANSI(got[i+1]), want) {
			t.Errorf("output line %d missing expected message %q: %q", i+1, want, stripANSI(got[i+1]))
		}
	}
}

func TestRenderTraceColumns_NoDataRowsReturnsInputUnchanged(t *testing.T) {
	lines := []string{"# no lines found"}
	got := RenderTraceColumns(lines, 80)
	if len(got) != 1 || got[0] != lines[0] {
		t.Fatalf("expected passthrough for input with no data rows, got %v", got)
	}
}

func TestRenderTraceColumns_NarrowTerminalStillProducesOutput(t *testing.T) {
	lines := []string{traceRow(0, "weft/compute", "a message that is somewhat long")}
	got := RenderTraceColumns(lines, 15) // pathologically narrow
	if len(got) == 0 {
		t.Fatal("expected at least one output line even at a very narrow width")
	}
}

// TestParseTraceRows_GapsAndPassthrough pins the shared parse step
// RenderTraceColumns and the browser's wasmParseTraceRows export both
// build on: passthrough lines carry IsData=false and only Raw, data
// rows carry the parsed fields plus a gap computed relative to the
// previous data row (skipping over any passthrough line in between).
func TestParseTraceRows_GapsAndPassthrough(t *testing.T) {
	lines := []string{
		"# 2 line(s) across 1 service(s)",
		traceRow(1000, "weft/coordinator", "first"),
		"# an unrelated passthrough line",
		traceRow(1_500_000, "weft/coordinator", "second"),
	}
	rows := ParseTraceRows(lines)
	if len(rows) != 4 {
		t.Fatalf("expected 4 rows, got %d", len(rows))
	}
	if rows[0].IsData || rows[0].Raw != lines[0] {
		t.Errorf("row 0: expected passthrough with Raw=%q, got %+v", lines[0], rows[0])
	}
	if !rows[1].IsData || rows[1].TsNs != 1000 || rows[1].Label != "weft/coordinator" || rows[1].Message != "first" {
		t.Errorf("row 1: unexpected parse, got %+v", rows[1])
	}
	if rows[1].GapNs != 0 {
		t.Errorf("row 1 (first data row): expected GapNs=0, got %d", rows[1].GapNs)
	}
	if rows[2].IsData {
		t.Errorf("row 2: expected passthrough, got %+v", rows[2])
	}
	wantGap := int64(1_500_000 - 1000)
	if rows[3].GapNs != wantGap {
		t.Errorf("row 3: expected GapNs=%d (gap measured against the previous DATA row, not the passthrough line), got %d", wantGap, rows[3].GapNs)
	}
	if rows[3].ColorIdx != TraceLabelColorIndex("weft/coordinator") {
		t.Errorf("row 3: ColorIdx should match TraceLabelColorIndex(Label); got %d want %d", rows[3].ColorIdx, TraceLabelColorIndex("weft/coordinator"))
	}
}
