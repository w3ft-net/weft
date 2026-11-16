package coordinator

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/cron"
)

// longWindowEnv is a cron with a result at each age the weft:: functions can
// reach back to: 5 and 7 an hour ago, then 10, 20, 30 ten days ago, 100 and 200
// twenty days ago, and 1 to 4 a hundred days ago. The local stores keep every
// raw result, so the totals are what a store with rollup tiers must agree with.
func longWindowEnv(t *testing.T) (*WeftFuncs, string) {
	t.Helper()
	env := newWFEnv(t)
	w := env.saveWatch(t, "lw")
	now := time.Now()
	day := 24 * time.Hour
	for i, v := range []string{"5", "7"} {
		env.saveResult(t, cron.Result{WatchID: w.ID, RanAt: now.Add(-time.Hour + time.Duration(i)*time.Minute), Stdout: v})
	}
	for age, vals := range map[time.Duration][]string{10 * day: {"10", "20", "30"}, 20 * day: {"100", "200"}, 100 * day: {"1", "2", "3", "4"}} {
		for i, v := range vals {
			env.saveResult(t, cron.Result{WatchID: w.ID, RanAt: now.Add(-age + time.Duration(i)*time.Minute), Stdout: v})
		}
	}
	return env.wf(), "ops/lw"
}

// The sum-based funcs reach past 24h and past the 7 days of raw results, and
// agree with a calculation over every value.
func TestWeftFuncs_AggregatesOverLongWindows(t *testing.T) {
	wf, name := longWindowEnv(t)
	vals := []float64{5, 7, 10, 20, 30, 100, 200, 1, 2, 3, 4}
	var sum, sumSq float64
	for _, v := range vals {
		sum += v
		sumSq += v * v
	}
	mean := sum / float64(len(vals))
	win := 200 * 24 * time.Hour

	check := func(fn string, got float64, err error, want float64) {
		t.Helper()
		if err != nil || math.Abs(got-want) > 1e-9 {
			t.Errorf("%s = %v, %v; want %v", fn, got, err, want)
		}
	}
	g, err := wf.Sum(name, win)
	check("sum", g, err, sum)
	g, err = wf.Avg(name, win)
	check("avg", g, err, mean)
	g, err = wf.Min(name, win)
	check("min", g, err, 1)
	g, err = wf.Max(name, win)
	check("max", g, err, 200)
	g, err = wf.Count(name, win)
	check("count", g, err, float64(len(vals)))
	g, err = wf.Stddev(name, win)
	check("stddev", g, err, math.Sqrt(sumSq/float64(len(vals))-mean*mean))
	g, err = wf.Rate(name, win)
	check("rate", g, err, sum/win.Seconds())

	// A 12-day window sees the hour-old values and the ten-day-old ones.
	g, err = wf.Count(name, 12*24*time.Hour)
	check("count 12d", g, err, 5)

	if _, err := wf.Avg(name, 401*24*time.Hour); err == nil {
		t.Error("avg over 401d: want a cap error")
	}
}

// Funcs that need individual results stop at the raw retention, and say so.
func TestWeftFuncs_RawOnlyFuncsStopAtSevenDaysLocal(t *testing.T) {
	wf, name := longWindowEnv(t)
	over := 8 * 24 * time.Hour
	if _, err := wf.Percentile(name, over, 50); err == nil || !strings.Contains(err.Error(), "exceeds maximum") {
		t.Errorf("percentile over 8d: %v", err)
	}
	if _, err := wf.Delta(name, over); err == nil {
		t.Error("delta over 8d: want a cap error")
	}
	if _, err := wf.First(name, over); err == nil {
		t.Error("first over 8d: want a cap error")
	}
	if _, err := wf.SeasonalStats(name, time.Hour, over); err == nil {
		t.Error("seasonal over 8d: want a cap error")
	}
	if got, err := wf.Percentile(name, 2*time.Hour, 50); err != nil || got != 6 {
		t.Errorf("median of raw values 5,7 = %v, %v; want 6", got, err)
	}
	if got, err := wf.Delta(name, 2*time.Hour); err != nil || got != 2 {
		t.Errorf("delta of 5,7 = %v, %v; want 2", got, err)
	}
}
