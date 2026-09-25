package spanview

import (
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/theme"
)

func TestSpanView_MoveClampsToRowBounds(t *testing.T) {
	v := &SpanView{rows: make([]theme.SpanRollupRow, 3)}
	v.move(-1)
	if v.focus != 0 {
		t.Errorf("expected focus clamped to 0, got %d", v.focus)
	}
	v.move(1)
	v.move(1)
	v.move(1) // past the end
	if v.focus != 2 {
		t.Errorf("expected focus clamped to 2, got %d", v.focus)
	}
}

func TestSpanView_MoveOnEmptyRowsStaysZero(t *testing.T) {
	v := &SpanView{}
	v.move(1)
	if v.focus != 0 {
		t.Errorf("expected focus 0 on empty rows, got %d", v.focus)
	}
}

func TestSpanView_MoveSampleClampsToOccurrenceBounds(t *testing.T) {
	occ := []theme.SpanOccurrence{{Duration: time.Millisecond}, {Duration: 2 * time.Millisecond}}
	v := &SpanView{}
	v.moveSample(-1, occ)
	if v.sampleFocus != 0 {
		t.Errorf("expected sampleFocus clamped to 0, got %d", v.sampleFocus)
	}
	v.moveSample(5, occ)
	if v.sampleFocus != len(occ)-1 {
		t.Errorf("expected sampleFocus clamped to %d, got %d", len(occ)-1, v.sampleFocus)
	}
}

func TestSpanView_CurrentOccurrencesOutOfRangeReturnsNil(t *testing.T) {
	v := &SpanView{rows: make([]theme.SpanRollupRow, 2), focus: 5}
	if occ := v.currentOccurrences(); occ != nil {
		t.Errorf("expected nil for out-of-range focus, got %v", occ)
	}
}

// TestPadToWidth_MeasuresVisibleWidthNotByteLength is the regression
// test for theme.SpanRollupColumns' rows carrying embedded ANSI color
// (see its doc comment): padToWidth used to compare len(s) — which
// counts escape-sequence bytes as width — against w, so a colored row
// (more bytes than visible chars) would either wrongly skip padding or,
// worse, byte-slice s[:w] through the middle of an escape sequence.
// This pins both cases: a colored string shorter than w still gets
// padded based on its VISIBLE length, and a colored string already at
// or past w in visible width is left untouched rather than truncated.
func TestPadToWidth_MeasuresVisibleWidthNotByteLength(t *testing.T) {
	colored := "\x1b[1;38;5;214mname\x1b[39;22m" // 4 visible chars, far more bytes
	got := padToWidth(colored, 10)
	wantPad := 10 - 4
	if want := colored + strings.Repeat(" ", wantPad); got != want {
		t.Errorf("padToWidth(%q, 10) = %q, want %q", colored, got, want)
	}

	// Visible width already >= w: must not be byte-truncated (which
	// could cut an escape sequence in half and corrupt the terminal).
	exact := padToWidth(colored, 4)
	if exact != colored {
		t.Errorf("padToWidth at exact visible width should return input unchanged, got %q", exact)
	}
}
