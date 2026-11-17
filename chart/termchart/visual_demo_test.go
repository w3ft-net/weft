package termchart

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// TestVisualDemoLine prints a few representative chart renderings so a
// human reviewer (`go test -run VisualDemo -v`) can eyeball them. Always
// passes — output goes through t.Log so `go test` shows it under -v.
func TestVisualDemoLine(t *testing.T) {
	pts := make([]Point, 30)
	for i := range pts {
		pts[i] = Point{
			X: float64(i),
			Y: math.Sin(float64(i)*0.4)*5 + 10,
		}
	}
	out, _ := Line(60, 14).Title("sine wave (sampled)").Points(pts).Render()
	fmt.Println()
	fmt.Println(out)
}

func TestVisualDemoLineSparse(t *testing.T) {
	pts := []Point{
		{0, 12}, {1, 18}, {2, 5}, {3, 22}, {4, 17}, {5, 9}, {6, 14},
	}
	out, _ := Line(50, 12).Title("error rate").Points(pts).
		YThreshold(20, SeverityWarn).Render()
	fmt.Println()
	fmt.Println(out)
}

func TestVisualDemoBar(t *testing.T) {
	out, _ := Bar(60, 12).
		Title("disk pct by host").
		BarValues(
			[]string{"alpha", "bravo", "charlie", "delta", "echo"},
			[]float64{42, 88, 17, 65, 31},
		).Render()
	fmt.Println()
	fmt.Println(out)
}

func TestVisualDemoSparkline(t *testing.T) {
	values := []float64{1, 3, 7, 5, 8, 4, 9, 6, 10, 2, 8, 3, 7, 5, 12}
	fmt.Printf("\nplain:   %s\n", Sparkline(values))
	fmt.Printf("colored: %s\n", SparklineColored(values))
}

// TestVisualDemoLineTime exercises the time-axis path with realistic
// per-minute data points. Visual output via -v; assertions verify time
// labels actually rendered in HH:MM form.
func TestVisualDemoLineTime(t *testing.T) {
	// Use a fixed reference time so assertions are deterministic.
	t0 := mustParseTime("2026-04-25T14:00:00Z")
	pts := make([]Point, 20)
	for i := range pts {
		pts[i] = Point{
			X: float64(t0.Add(time.Duration(i) * time.Minute).Unix()),
			Y: 10 + float64(i%5)*3,
		}
	}
	out, _ := Line(70, 14).
		Title("error rate / 1m").
		XAxisTime().
		Points(pts).
		YThreshold(20, SeverityWarn).
		Render()
	plain := stripANSILocal(out)

	// Time labels are HH:MM colon-separated. Without the time path,
	// labels would be plain numbers like "1.778e+09".
	if !strings.Contains(plain, ":") {
		t.Errorf("time-axis chart missing HH:MM labels:\n%s", plain)
	}
	// Threshold dashed line should be present.
	if !strings.Contains(plain, "╌") {
		t.Errorf("threshold dashes missing:\n%s", plain)
	}
	fmt.Println()
	fmt.Println(out)
}

func mustParseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}
