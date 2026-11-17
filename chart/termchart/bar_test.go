package termchart

import (
	"fmt"
	"strings"
	"testing"
)

// barColumns returns, for the bottom plot row of a rendered bar chart,
// the 0-based columns holding a bar cell.
func barColumns(t *testing.T, out string) []int {
	t.Helper()
	rows := strings.Split(strings.TrimSuffix(stripANSILocal(out), "\n"), "\n")
	bottom := []rune(rows[len(rows)-1-xAxisHeight])
	var cols []int
	for i, r := range bottom {
		if r == fullBlock {
			cols = append(cols, i)
		}
	}
	return cols
}

func barInput(n int) ([]string, []float64) {
	labels := make([]string, n)
	values := make([]float64, n)
	for i := range values {
		labels[i] = fmt.Sprint(i)
		values[i] = 1
	}
	values[n-1] = 100
	return labels, values
}

// More bars than half the plot width drop the inter-bar gap so each
// gets a column, rather than clipping the tail past the right edge.
func TestBarDensePacksWithoutGap(t *testing.T) {
	const w = 100
	labels, values := barInput(BarSlots(w))
	out, _ := Bar(w, 20).BarValues(labels, values).Render()
	if got := len(barColumns(t, out)); got != BarSlots(w) {
		t.Errorf("drew %d bars, want all %d", got, BarSlots(w))
	}
}

// More bars than columns are averaged into one bar per column; the
// last value still lands in the last column and sets the y scale.
func TestBarBinsOverflow(t *testing.T) {
	const w = 100
	labels, values := barInput(BarSlots(w) * 3)
	out, _ := Bar(w, 20).BarValues(labels, values).Render()
	cols := barColumns(t, out)
	if len(cols) != BarSlots(w) {
		t.Fatalf("drew %d bars, want %d", len(cols), BarSlots(w))
	}
	rows := strings.Split(stripANSILocal(out), "\n")
	top := []rune(rows[0])
	if top[len(top)-1] != fullBlock {
		t.Errorf("last bin (holding the 100 spike) should reach the top row:\n%s", stripANSILocal(out))
	}
}

// Dense bars are too narrow to hold a label each; labels print whole
// at the bars they belong to, skipping ones that would overlap,
// instead of being truncated to the 1-column bar width.
func TestBarDenseLabelsPrintWhole(t *testing.T) {
	const w = 100
	labels, values := barInput(BarSlots(w))
	for i := range labels {
		labels[i] = fmt.Sprintf("%04d", i)
	}
	out, _ := Bar(w, 20).BarValues(labels, values).Render()
	rows := strings.Split(strings.TrimSuffix(stripANSILocal(out), "\n"), "\n")
	axis := rows[len(rows)-1]
	if strings.Contains(axis, "…") || !strings.Contains(axis, "0000") {
		t.Errorf("want whole labels under dense bars, got %q", axis)
	}
}
