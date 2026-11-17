package termchart

import (
	"regexp"
	"strings"
	"testing"
)

var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

// XRange draws the axis over the requested span, not the data's own: data
// in [40, 60] under XRange(0, 100) gets ticks out to 10 and 90.
func TestLineXRange(t *testing.T) {
	pts := []Point{{X: 40, Y: 1}, {X: 50, Y: 3}, {X: 60, Y: 2}}
	plain, err := Line(60, 12).Points(pts).Render()
	if err != nil {
		t.Fatal(err)
	}
	fixed, err := Line(60, 12).Points(pts).XRange(0, 100).Render()
	if err != nil {
		t.Fatal(err)
	}
	axis := func(s string) string {
		lines := strings.Split(strings.TrimRight(ansi.ReplaceAllString(s, ""), "\n"), "\n")
		return lines[len(lines)-1]
	}
	if got := axis(fixed); !strings.Contains(got, "10.00") || !strings.Contains(got, "90.00") || strings.Contains(axis(plain), "90.00") {
		t.Fatalf("x-axis with XRange(0, 100) = %q, without = %q", got, axis(plain))
	}
	if again, _ := Line(60, 12).Points(pts).XRange(5, 5).Render(); again != plain {
		t.Error("an empty XRange should be ignored")
	}
}
