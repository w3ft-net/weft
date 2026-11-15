package cronstats

import (
	"math"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/cron"
)

func TestPercentile(t *testing.T) {
	vals := []float64{40, 10, 30, 20}
	for _, c := range []struct{ p, want float64 }{{0, 10}, {50, 25}, {100, 40}, {25, 17.5}, {90, 37}} {
		if got, ok := Percentile(vals, c.p); !ok || math.Abs(got-c.want) > 1e-9 {
			t.Errorf("Percentile(p=%v) = %v ok=%v, want %v", c.p, got, ok, c.want)
		}
	}
	if vals[0] != 40 {
		t.Error("Percentile sorted its input")
	}
	if _, ok := Percentile(nil, 50); ok {
		t.Error("an empty set has a percentile")
	}
	if v, ok := Percentile([]float64{7}, 99); !ok || v != 7 {
		t.Errorf("one value: %v %v", v, ok)
	}
}

func TestSeasonalBaseline(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	at := func(age time.Duration, v float64) cron.Point { return cron.Point{At: now.Add(-age), Value: v} }
	pts := []cron.Point{
		at(day, 10), at(2*day+2*time.Minute, 20), at(3*day-4*time.Minute, 30), // same hour, within 5 minutes
		at(day+time.Hour, 1000),    // an hour off
		at(2*day+6*time.Minute, 5), // just outside the tolerance
	}
	got := SeasonalBaseline(pts, now, day)
	if got.Samples != 3 || got.Avg != 20 || math.Abs(got.Stddev-10) > 1e-9 {
		t.Errorf("baseline = %+v, want 3 samples, avg 20, stddev 10", got)
	}
	if got := SeasonalBaseline(pts[:1], now, day); got.Samples != 1 || got.Stddev != 0 {
		t.Errorf("one sample: %+v", got)
	}
	if got := SeasonalBaseline(nil, now, day); got != (Seasonal{}) {
		t.Errorf("no points: %+v", got)
	}
	// A short period has a proportionally short tolerance: a twentieth of ten
	// minutes is thirty seconds.
	short := []cron.Point{at(10*time.Minute+20*time.Second, 1), at(10*time.Minute+40*time.Second, 2)}
	if got := SeasonalBaseline(short, now, 10*time.Minute); got.Samples != 1 || got.Avg != 1 {
		t.Errorf("short period: %+v", got)
	}
}

func TestWindowStats(t *testing.T) {
	s := cron.WindowStats{Count: 4, Sum: 20, SumSq: 120, Min: 2, Max: 8}
	if s.Mean() != 5 || math.Abs(s.Stddev()-math.Sqrt(5)) > 1e-9 {
		t.Errorf("mean %v stddev %v", s.Mean(), s.Stddev())
	}
	// Float cancellation never yields a NaN.
	if got := (cron.WindowStats{Count: 3, Sum: 0.3, SumSq: 0.03}).Stddev(); math.IsNaN(got) {
		t.Error("stddev of a constant series is NaN")
	}
}
