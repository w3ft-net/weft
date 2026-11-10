package coordinator

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/store"
)

// longWindowCron seeds a cron on a bolt plan (so retention doesn't trim the
// fixtures) with data in every tier: 2 raw values, plus rollup buckets 10, 20
// and 100 days back.
func longWindowCron(t *testing.T) (*WeftFuncs, string) {
	t.Helper()
	db := testQueryStore(t, "testcust")
	cust, err := db.GetOrgByName("testcust")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveCronWatch(cust.ID, 0, "", "lw", "every 1m", "echo 1", true, nil); err != nil {
		t.Fatal(err)
	}
	w, err := db.GetCronWatch(cust.ID, 0, "lw")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	day := 24 * time.Hour
	for i, v := range []float64{5, 7} {
		if err := saveCronResult(db, &store.CronResult{CronWatchID: w.ID, RanAt: now.Add(-time.Hour + time.Duration(i)*time.Minute), Stdout: floatStr(v), Value: &v}); err != nil {
			t.Fatal(err)
		}
	}
	bucket := func(table string, at time.Time, count int64, sum, sumSq, min, max float64) {
		t.Helper()
		if err := db.ExecForTest(`INSERT INTO `+table+` (cron_watch_id, tag, bucket_start, count, sum, sum_sq, min, max)
			VALUES (?, '', ?, ?, ?, ?, ?, ?)`, w.ID, at, count, sum, sumSq, min, max); err != nil {
			t.Fatal(err)
		}
	}
	bucket("cron_results_hourly", now.Add(-10*day).Truncate(time.Hour), 3, 60, 1400, 10, 30)
	bucket("cron_results_4h", now.Add(-20*day).Truncate(4*time.Hour), 2, 300, 50000, 100, 200)
	bucket("cron_results_daily", now.Add(-100*day).Truncate(day), 4, 10, 30, 1, 4)
	return &WeftFuncs{Store: db, OrgID: cust.ID}, "lw"
}

func floatStr(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// The sum-based funcs reach past 24h and past the 7 days of raw results by
// reading the rollup tiers, and agree with a calculation over every value.
func TestWeftFuncs_AggregatesReachRollupTiers(t *testing.T) {
	wf, name := longWindowCron(t)
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

	// A 12-day window sees only the raw values and the hourly bucket.
	g, err = wf.Count(name, 12*24*time.Hour)
	check("count 12d", g, err, 5)

	if _, err := wf.Avg(name, 401*24*time.Hour); err == nil {
		t.Error("avg over 401d: want a cap error")
	}
}

// Funcs that need individual results can only see raw data, and say so.
func TestWeftFuncs_RawOnlyFuncsStopAtSevenDays(t *testing.T) {
	wf, name := longWindowCron(t)
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
