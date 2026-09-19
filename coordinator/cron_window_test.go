package coordinator

import (
	"testing"
	"time"
)

// TestWatchQueryRange pins the fix for the cold-shard fan-out incident:
// executeWatch hardcoded tr = [2000-01-01 .. 2100-01-01], so every cron
// tick fanned out to every in-retention shard — including archived
// cold-Lambda shards — producing a relentless 403 storm and (once auth
// was fixed) a compute OOM crash-loop at cron cadence.
//
// A cron's actual fire cadence is exactly parseSchedule(schedule) (see
// runWatch: time.NewTicker(parseSchedule(...))). The query window must
// therefore be [now-interval, now] — it tiles the gap between fires
// exactly: never larger than the interval (so it can't reach old
// archived windows) and never smaller (no data gap between runs).
func TestWatchQueryRange(t *testing.T) {
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		schedule string
		wantSpan time.Duration
	}{
		{"*/5 * * * *", 5 * time.Minute},
		{"*/15 * * * *", 15 * time.Minute},
		{"every 2h", 2 * time.Hour},
		{"* * * * *", time.Minute}, // bare "*" minute field: every minute
		{"0 * * * *", time.Hour},   // fixed minute + "*" hour: hourly
		{"garbage-not-a-schedule", 5 * time.Minute},
	}

	for _, c := range cases {
		got := watchQueryRange(c.schedule, now)

		if !got.End.Equal(now) {
			t.Errorf("%q: End = %v, want now (%v)", c.schedule, got.End, now)
		}
		if span := got.End.Sub(got.Start); span != c.wantSpan {
			t.Errorf("%q: span = %s, want %s", c.schedule, span, c.wantSpan)
		}
		// Regression guard: must never be the old all-time window.
		if got.Start.Year() <= 2000 {
			t.Errorf("%q: Start = %v — cron is still querying all-time; "+
				"this is the bug that fanned every tick onto cold shards",
				c.schedule, got.Start)
		}
		if span := got.End.Sub(got.Start); span > 24*time.Hour {
			t.Errorf("%q: span %s exceeds a day — cron window must not reach archived shards",
				c.schedule, span)
		}
	}
}
