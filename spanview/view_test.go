package spanview

import (
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
