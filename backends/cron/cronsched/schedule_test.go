package cronsched

import (
	"testing"
	"time"
)

// TestParseScheduleInterval_BareStar pins the fix for a bug where any
// 5-field cron expression other than "*/N * * * *" fell through to the
// 5-minute default. In production this silently downgraded every cron
// saved with schedule "* * * * *" (documented, and enforced by
// MinWatchInterval for paid tiers, as "every minute") to actually firing
// every 5 minutes — a resolution loss customers on Yard/Bolt tiers are
// explicitly paying to avoid.
//
// It also affected fixed-minute-of-the-hour schedules like "0 * * * *"
// (currently used by disk-fill-rate and christen-failure-rate-trend),
// which should tick hourly but were instead ticking every 5 minutes.
func TestParseScheduleInterval_BareStar(t *testing.T) {
	cases := []struct {
		schedule string
		want     time.Duration
	}{
		{"* * * * *", time.Minute},
		{"*/5 * * * *", 5 * time.Minute},
		{"*/15 * * * *", 15 * time.Minute},
		{"0 * * * *", time.Hour},
		{"30 * * * *", time.Hour},
		{"0 */2 * * *", 2 * time.Hour},
		{"every 90s", 90 * time.Second},
		{"garbage-not-a-schedule", 5 * time.Minute},
	}

	for _, c := range cases {
		got := ParseScheduleInterval(c.schedule)
		if got != c.want {
			t.Errorf("ParseScheduleInterval(%q) = %s, want %s", c.schedule, got, c.want)
		}
	}
}
