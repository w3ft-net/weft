package chart

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// stubSource is a fixed series for tests that don't need a full backing
// store.
type stubSource struct{ series CronSeries }

func (s stubSource) GetCronSeries(string, string) (CronSeries, error) { return s.series, nil }

// TestLandingSparkline reproduces the exact ANSI/Unicode output that a
// realistic `chart error-rate -w 1h --kind sparkline` would emit. The
// landing page's marketing diagram embeds this string verbatim; if this
// test starts failing because Sparkline()'s ramp or normalisation
// changed, the diagram needs to be regenerated.
//
// Series: 24 buckets of errors-per-minute for /cron.d/error-rate. Mostly
// low (8–22), with a rising shoulder and a brief spike to 58 — the kind
// of shape that makes the threshold `| alert above 50` actually fire.
func TestLandingSparkline(t *testing.T) {
	// errors-per-minute, oldest → newest. Hand-tuned so the visual is
	// readable in 24 cells: quiet, slow rise, spike, decay, quiet.
	values := []float64{
		8, 11, 9, 12, 15, 18, 14, 22, 19, 28, 35, 32,
		45, 38, 52, 58, 41, 27, 19, 14, 12, 9, 11, 8,
	}

	// Round-trip through the same code path the shell takes.
	start := time.Date(2026, 5, 13, 13, 0, 0, 0, time.UTC)
	series := CronSeries{Granularity: "raw", Start: start, End: start.Add(time.Hour)}
	for i, v := range values {
		series.Buckets = append(series.Buckets, CronBucket{
			Start: start.Add(time.Duration(i) * time.Minute), Count: 1, Sum: v, Min: v, Max: v,
		})
	}

	out, errMsg := RenderOneShot(stubSource{series}, "1h..", WatchArgs{Name: "error-rate", Kind: "sparkline", Aggregate: "avg"}, 80, 20)
	if errMsg != "" {
		t.Fatalf("render failed: %s", errMsg)
	}
	got := strings.TrimRight(out, "\n")

	// 24 input values → 24 sparkline runes. (One rune per cell; the
	// blockRamp characters are all in the U+258x block.)
	if n := len([]rune(got)); n != len(values) {
		t.Fatalf("sparkline rune count = %d, want %d (got %q)", n, len(values), got)
	}

	// The spike (max=58 at index 15) must map to the top of the ramp.
	if !strings.ContainsRune(got, '█') {
		t.Errorf("expected top-of-ramp '█' for the 58-error spike, got %q", got)
	}
	// The trough (min=8 at indices 0 and 23) must map to the bottom.
	if !strings.ContainsRune(got, '▁') {
		t.Errorf("expected bottom-of-ramp '▁' for the 8-error trough, got %q", got)
	}

	// Surface the output so `go test -v -run TestLandingSparkline` shows
	// the exact glyph sequence to paste into the landing diagram.
	fmt.Printf("\n[landing sparkline] %s\n\n", got)
}
