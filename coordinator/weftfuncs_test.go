package coordinator

import (
	"math"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/cron"
)

func TestWeftFuncs_Latest(t *testing.T) {
	env := newWFEnv(t)
	env.seedWatch(t, "latest-test", []string{"10", "20", "30"}, time.Minute)

	wf := env.wf()
	val, err := wf.Latest("ops/latest-test")
	if err != nil {
		t.Fatal(err)
	}
	if val != "30" {
		t.Errorf("expected 30, got %q", val)
	}
}

func TestWeftFuncs_Latest_NotFound(t *testing.T) {
	env := newWFEnv(t)
	wf := env.wf()
	_, err := wf.Latest("ops/nonexistent-watch")
	if err == nil {
		t.Error("expected error for nonexistent watch")
	}
}

func TestWeftFuncs_Avg(t *testing.T) {
	env := newWFEnv(t)
	env.seedWatch(t, "avg-test", []string{"10", "20", "30"}, time.Minute)

	wf := env.wf()
	val, err := wf.Avg("ops/avg-test", 1*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if val != 20.0 {
		t.Errorf("expected 20, got %f", val)
	}
}

func TestWeftFuncs_Max(t *testing.T) {
	env := newWFEnv(t)
	env.seedWatch(t, "max-test", []string{"5", "42", "17"}, time.Minute)

	wf := env.wf()
	val, err := wf.Max("ops/max-test", 1*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if val != 42.0 {
		t.Errorf("expected 42, got %f", val)
	}
}

func TestWeftFuncs_Min(t *testing.T) {
	env := newWFEnv(t)
	env.seedWatch(t, "min-test", []string{"5", "42", "17"}, time.Minute)

	wf := env.wf()
	val, err := wf.Min("ops/min-test", 1*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if val != 5.0 {
		t.Errorf("expected 5, got %f", val)
	}
}

func TestWeftFuncs_Sum(t *testing.T) {
	env := newWFEnv(t)
	env.seedWatch(t, "sum-test", []string{"10", "20", "30"}, time.Minute)

	wf := env.wf()
	val, err := wf.Sum("ops/sum-test", 1*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if val != 60.0 {
		t.Errorf("expected 60, got %f", val)
	}
}

func TestWeftFuncs_Stddev(t *testing.T) {
	env := newWFEnv(t)
	// Values: 10, 20, 30 → mean=20, variance=((100+0+100)/3)=66.67, stddev≈8.165
	env.seedWatch(t, "stddev-test", []string{"10", "20", "30"}, time.Minute)

	wf := env.wf()
	val, err := wf.Stddev("ops/stddev-test", 1*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	expected := math.Sqrt(200.0 / 3.0) // ≈8.165
	if math.Abs(val-expected) > 0.01 {
		t.Errorf("expected ~%.3f, got %.3f", expected, val)
	}
}

func TestWeftFuncs_Stddev_NeedsTwoResults(t *testing.T) {
	env := newWFEnv(t)
	env.seedWatch(t, "stddev-one", []string{"42"}, time.Minute)

	wf := env.wf()
	_, err := wf.Stddev("ops/stddev-one", 1*time.Hour)
	if err == nil {
		t.Error("expected error for single result")
	}
}

func TestWeftFuncs_SkipsNonNumeric(t *testing.T) {
	env := newWFEnv(t)
	env.seedWatch(t, "mixed-test", []string{"10", "not-a-number", "30", "ERROR", "20"}, time.Minute)

	wf := env.wf()

	avg, err := wf.Avg("ops/mixed-test", 1*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if avg != 20.0 { // (10+30+20)/3
		t.Errorf("avg: expected 20, got %f", avg)
	}

	max, _ := wf.Max("ops/mixed-test", 1*time.Hour)
	if max != 30.0 {
		t.Errorf("max: expected 30, got %f", max)
	}

	min, _ := wf.Min("ops/mixed-test", 1*time.Hour)
	if min != 10.0 {
		t.Errorf("min: expected 10, got %f", min)
	}

	sum, _ := wf.Sum("ops/mixed-test", 1*time.Hour)
	if sum != 60.0 {
		t.Errorf("sum: expected 60, got %f", sum)
	}
}

func TestWeftFuncs_DurationCap(t *testing.T) {
	env := newWFEnv(t)
	wf := env.wf()

	_, err := wf.Avg("ops/any-watch", 25*time.Hour)
	if err == nil {
		t.Error("expected error for duration > 24h")
	}

	_, err = wf.Avg("ops/any-watch", -1*time.Hour)
	if err == nil {
		t.Error("expected error for negative duration")
	}
}

func TestWeftFuncs_TimeRangeFiltering(t *testing.T) {
	env := newWFEnv(t)
	// Seed with values spread over 2 hours. Query only last 30m.
	w := env.saveWatch(t, "range-filter-test")
	now := time.Now()

	// Old values (2h ago) — should be excluded from 30m query.
	for _, v := range []string{"100", "200"} {
		env.saveResult(t, cron.Result{WatchID: w.ID, RanAt: now.Add(-2 * time.Hour), Stdout: v})
	}
	// Recent values (5m ago) — should be included.
	for _, v := range []string{"10", "20"} {
		env.saveResult(t, cron.Result{WatchID: w.ID, RanAt: now.Add(-5 * time.Minute), Stdout: v})
	}

	wf := env.wf()
	avg, err := wf.Avg("ops/range-filter-test", 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if avg != 15.0 { // (10+20)/2, not including old 100/200
		t.Errorf("expected 15 (recent only), got %f", avg)
	}
}

func TestWeftFuncs_FloatValues(t *testing.T) {
	env := newWFEnv(t)
	env.seedWatch(t, "float-test", []string{"1.5", "2.5", "3.0"}, time.Minute)

	wf := env.wf()

	avg, _ := wf.Avg("ops/float-test", 1*time.Hour)
	expected := (1.5 + 2.5 + 3.0) / 3.0
	if math.Abs(avg-expected) > 0.001 {
		t.Errorf("avg: expected %.3f, got %.3f", expected, avg)
	}

	sum, _ := wf.Sum("ops/float-test", 1*time.Hour)
	if math.Abs(sum-7.0) > 0.001 {
		t.Errorf("sum: expected 7.0, got %.3f", sum)
	}
}

func TestWeftFuncs_EmptyResults(t *testing.T) {
	env := newWFEnv(t)
	// Create watch with no results.
	env.saveWatch(t, "empty-test")

	wf := env.wf()

	_, err := wf.Avg("ops/empty-test", 1*time.Hour)
	if err == nil {
		t.Error("expected error for empty results")
	}

	_, err = wf.Max("ops/empty-test", 1*time.Hour)
	if err == nil {
		t.Error("expected error for empty results")
	}
}

func TestParseAggDuration(t *testing.T) {
	tests := []struct {
		input string
		want  time.Duration
		err   bool
	}{
		{"30m", 30 * time.Minute, false},
		{"1h", time.Hour, false},
		{"6h", 6 * time.Hour, false},
		{"24h", 24 * time.Hour, false},
		{"25h", 25 * time.Hour, false},
		{"30d", 30 * 24 * time.Hour, false},
		{"401d", 0, true}, // exceeds cap
		{"-1h", 0, true},  // negative
		{"abc", 0, true},  // invalid
		{"0s", 0, true},   // zero
	}
	for _, tt := range tests {
		d, err := ParseAggDuration(tt.input)
		if tt.err && err == nil {
			t.Errorf("ParseAggDuration(%q): expected error", tt.input)
		}
		if !tt.err && err != nil {
			t.Errorf("ParseAggDuration(%q): unexpected error: %v", tt.input, err)
		}
		if !tt.err && d != tt.want {
			t.Errorf("ParseAggDuration(%q): got %v, want %v", tt.input, d, tt.want)
		}
	}
}

func TestParseRawDuration_CapsAtRawRetention(t *testing.T) {
	if d, err := ParseRawDuration("48h"); err != nil || d != 48*time.Hour {
		t.Errorf("ParseRawDuration(48h) = %v, %v", d, err)
	}
	// 192h = 8d, just over the 7d of raw results.
	if _, err := ParseRawDuration("192h"); err == nil {
		t.Error("ParseRawDuration(8d): expected reject, got nil")
	}
}

func TestWeftFuncs_First(t *testing.T) {
	env := newWFEnv(t)
	// seedWatch lays values left-to-right with the LAST element at now.
	// So "10" is oldest, "30" is newest.
	env.seedWatch(t, "first-test", []string{"10", "20", "30"}, time.Minute)

	wf := env.wf()
	got, err := wf.First("ops/first-test", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got != 10.0 {
		t.Errorf("First: expected 10, got %f", got)
	}
}

func TestWeftFuncs_First_AcceptsLongerWindow(t *testing.T) {
	// First should accept a window > 24h, up to the raw retention.
	env := newWFEnv(t)
	env.seedWatch(t, "first-long", []string{"5"}, time.Minute)
	wf := env.wf()
	if _, err := wf.First("ops/first-long", 48*time.Hour); err != nil {
		t.Errorf("First with 48h window: unexpected error: %v", err)
	}
	if _, err := wf.First("ops/first-long", 8*24*time.Hour); err == nil {
		t.Error("First with 8d window: expected cap error, got nil")
	}
}

func TestWeftFuncs_Count(t *testing.T) {
	env := newWFEnv(t)
	// Mix numeric and non-numeric — Count counts numeric results only, since
	// the rollups that serve long windows don't keep the others.
	env.seedWatch(t, "count-test", []string{"10", "ERROR", "30", "20"}, time.Minute)
	wf := env.wf()
	got, err := wf.Count("ops/count-test", 1*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got != 3.0 {
		t.Errorf("Count: expected 3, got %f", got)
	}
}

func TestWeftFuncs_Percentile(t *testing.T) {
	env := newWFEnv(t)
	env.seedWatch(t, "pct-test", []string{"10", "20", "30", "40", "50"}, time.Minute)
	wf := env.wf()

	cases := []struct {
		p    float64
		want float64
	}{
		{0, 10},   // min
		{50, 30},  // median (middle of 5)
		{100, 50}, // max
		{25, 20},  // exact rank 1
		{75, 40},  // exact rank 3
	}
	for _, c := range cases {
		got, err := wf.Percentile("ops/pct-test", 1*time.Hour, c.p)
		if err != nil {
			t.Fatalf("p=%v: %v", c.p, err)
		}
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("p=%v: got %f, want %f", c.p, got, c.want)
		}
	}
}

func TestWeftFuncs_Percentile_LinearInterpolation(t *testing.T) {
	env := newWFEnv(t)
	// 4 values: ranks 0..3. p=33.33 → rank 1.0 = value at index 1.
	// p=50 → rank 1.5 = midpoint between index 1 and index 2.
	env.seedWatch(t, "pct-interp", []string{"10", "20", "30", "40"}, time.Minute)
	wf := env.wf()
	got, err := wf.Percentile("ops/pct-interp", 1*time.Hour, 50)
	if err != nil {
		t.Fatal(err)
	}
	// rank = 0.5 * 3 = 1.5 → vals[1] + 0.5*(vals[2]-vals[1]) = 20 + 5 = 25.
	if math.Abs(got-25.0) > 1e-9 {
		t.Errorf("p=50 of [10,20,30,40]: got %f, want 25", got)
	}
}

func TestWeftFuncs_Percentile_OutOfRange(t *testing.T) {
	env := newWFEnv(t)
	env.seedWatch(t, "pct-bounds", []string{"1", "2", "3"}, time.Minute)
	wf := env.wf()
	if _, err := wf.Percentile("ops/pct-bounds", 1*time.Hour, -1); err == nil {
		t.Error("expected error for p < 0")
	}
	if _, err := wf.Percentile("ops/pct-bounds", 1*time.Hour, 101); err == nil {
		t.Error("expected error for p > 100")
	}
}

func TestWeftFuncs_Delta(t *testing.T) {
	env := newWFEnv(t)
	// Oldest=10, newest=50.
	env.seedWatch(t, "delta-test", []string{"10", "20", "30", "40", "50"}, time.Minute)
	wf := env.wf()
	got, err := wf.Delta("ops/delta-test", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got != 40.0 { // 50 - 10
		t.Errorf("Delta: expected 40, got %f", got)
	}
}

func TestWeftFuncs_Delta_NeedsTwoResults(t *testing.T) {
	env := newWFEnv(t)
	env.seedWatch(t, "delta-one", []string{"42"}, time.Minute)
	wf := env.wf()
	if _, err := wf.Delta("ops/delta-one", 1*time.Hour); err == nil {
		t.Error("expected error for single result")
	}
}

func TestWeftFuncs_Slope(t *testing.T) {
	env := newWFEnv(t)
	// Delta = 50 - 10 = 40, over a window of 600s → slope = 40/600.
	env.seedWatch(t, "slope-test", []string{"10", "20", "30", "40", "50"}, time.Minute)
	wf := env.wf()
	got, err := wf.Slope("ops/slope-test", 600*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := 40.0 / 600.0
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("Slope: got %f, want %f", got, want)
	}
}

func TestWeftFuncs_Rate(t *testing.T) {
	env := newWFEnv(t)
	// 3 values over 2 minutes wall time, 10-minute window so all fall in.
	// Sum = 10+20+30 = 60; window = 600s → rate = 0.1/sec.
	env.seedWatch(t, "rate-test", []string{"10", "20", "30"}, time.Minute)
	wf := env.wf()
	got, err := wf.Rate("ops/rate-test", 600*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := 0.1
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("Rate: got %f, want %f", got, want)
	}
}
