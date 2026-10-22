package store

import (
	"math"
	"testing"
	"time"
)

// Segments tile the window: oldest first, each meeting the next at a bucket
// boundary, the newest open-ended raw, and the oldest edge rounded inward by
// less than one bucket of the tier it falls in.
func TestWindowSegments_TileTheWindow(t *testing.T) {
	now := time.Date(2026, 10, 21, 13, 37, 12, 0, time.UTC)
	day := 24 * time.Hour
	cases := []struct {
		name   string
		window time.Duration
		grans  []string
	}{
		{"1h", time.Hour, []string{"raw"}},
		{"7d", 7 * day, []string{"raw"}},
		{"10d", 10 * day, []string{"hourly", "raw"}},
		{"20d", 20 * day, []string{"4h", "hourly", "raw"}},
		{"100d", 100 * day, []string{"daily", "4h", "hourly", "raw"}},
		{"400d", 400 * day, []string{"daily", "4h", "hourly", "raw"}},
	}
	align := map[string]time.Duration{"hourly": time.Hour, "4h": 4 * time.Hour, "daily": day}
	for _, c := range cases {
		since := now.Add(-c.window)
		segs := windowSegments(now, since)
		var grans []string
		for _, s := range segs {
			grans = append(grans, s.granularity)
		}
		if len(grans) != len(c.grans) {
			t.Errorf("%s: tiers %v, want %v", c.name, grans, c.grans)
			continue
		}
		for i := range grans {
			if grans[i] != c.grans[i] {
				t.Errorf("%s: tiers %v, want %v", c.name, grans, c.grans)
			}
		}
		for i, s := range segs {
			if i+1 < len(segs) && !s.to.Equal(segs[i+1].from) {
				t.Errorf("%s: %s ends %s but %s starts %s", c.name, s.granularity, s.to, segs[i+1].granularity, segs[i+1].from)
			}
			if a, ok := align[s.granularity]; ok && (!s.from.Equal(s.from.Truncate(a)) || (!s.to.IsZero() && !s.to.Equal(s.to.Truncate(a)))) {
				t.Errorf("%s: %s segment [%s, %s) not aligned to %s", c.name, s.granularity, s.from, s.to, a)
			}
		}
		last := segs[len(segs)-1]
		if last.granularity != "raw" || !last.to.IsZero() {
			t.Errorf("%s: newest segment should be open-ended raw, got %+v", c.name, last)
		}
		first := segs[0]
		slack := time.Duration(0)
		if a, ok := align[first.granularity]; ok {
			slack = a
		}
		if first.from.Before(since) || first.from.Sub(since) > slack {
			t.Errorf("%s: window starts %s, asked for %s (slack %s)", c.name, first.from, since, slack)
		}
	}
}

// A window stitched across all four tiers returns the same statistics as
// one computed from the individual values.
func TestCronWindowStats_StitchesTiers(t *testing.T) {
	db, err := Open(TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	tx, rollback := db.BeginTest()
	t.Cleanup(func() { rollback(); db.Close() })
	org, err := tx.FindOrCreateOrg("window-stats-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.SaveCronWatch(org.ID, "", "w", "every 1m", "echo 1", true, nil); err != nil {
		t.Fatal(err)
	}
	w, err := tx.GetCronWatch(org.ID, "w")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	day := 24 * time.Hour
	// Values chosen so each tier contributes: 2 raw values, one hourly bucket
	// (3 values: 10,20,30), one 4h bucket (2: 100,200), one daily bucket (4: 1,2,3,4).
	for _, v := range []float64{5, 7} {
		if err := tx.ExecForTest(`INSERT INTO cron_results (cron_watch_id, ran_at, value, tag, stdout, stderr)
			VALUES (?, ?, ?, '', ?, '')`, w.ID, now.Add(-time.Hour), v, "x"); err != nil {
			t.Fatal(err)
		}
	}
	bucket := func(table string, at time.Time, count int64, sum, sumSq, min, max float64) {
		t.Helper()
		if err := tx.ExecForTest(`INSERT INTO `+table+` (cron_watch_id, tag, bucket_start, count, sum, sum_sq, min, max)
			VALUES (?, '', ?, ?, ?, ?, ?, ?)`, w.ID, at, count, sum, sumSq, min, max); err != nil {
			t.Fatal(err)
		}
	}
	bucket("cron_results_hourly", now.Add(-10*day).Truncate(time.Hour), 3, 60, 1400, 10, 30)
	bucket("cron_results_4h", now.Add(-20*day).Truncate(4*time.Hour), 2, 300, 50000, 100, 200)
	bucket("cron_results_daily", now.Add(-100*day).Truncate(day), 4, 10, 30, 1, 4)

	got, err := tx.CronWindowStats(w.ID, now.Add(-200*day))
	if err != nil {
		t.Fatal(err)
	}
	vals := []float64{5, 7, 10, 20, 30, 100, 200, 1, 2, 3, 4}
	var sum, sumSq float64
	for _, v := range vals {
		sum += v
		sumSq += v * v
	}
	mean := sum / float64(len(vals))
	want := math.Sqrt(sumSq/float64(len(vals)) - mean*mean)
	if got.Count != int64(len(vals)) || got.Sum != sum || got.Min != 1 || got.Max != 200 {
		t.Errorf("stats = %+v, want count %d sum %v min 1 max 200", got, len(vals), sum)
	}
	if math.Abs(got.Stddev()-want) > 1e-9 {
		t.Errorf("stddev = %v, want %v", got.Stddev(), want)
	}

	// A shorter window leaves the older tiers out.
	short, _ := tx.CronWindowStats(w.ID, now.Add(-12*day))
	if short.Count != 5 { // 2 raw + 3 hourly
		t.Errorf("12d window count = %d, want 5", short.Count)
	}
}
