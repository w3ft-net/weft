package store

import (
	"strings"
	"testing"
	"time"
)

const (
	day  = 24 * time.Hour
	week = 7 * day
)

// TestValidateSchedule pins the strict schedule grammar. Crons fire on a
// UTC-aligned grid of uniform intervals, so a schedule that names a time of
// day, or that the parser can't read, is an error rather than being quietly
// run at some other cadence (it used to become "every 5 minutes").
func TestValidateSchedule(t *testing.T) {
	ok := []struct {
		schedule string
		want     time.Duration
	}{
		{"* * * * *", time.Minute},
		{"*/5 * * * *", 5 * time.Minute},
		{"0 * * * *", time.Hour},
		{"0 */2 * * *", 2 * time.Hour},
		{"0 0 * * *", day},
		{"0 0 */3 * *", 3 * day},
		{"0 0 * * 1", week},
		{"every 90s", 90 * time.Second},
		{"every 1h30m", 90 * time.Minute},
		{"every 1d", day},
		{"every 2d", 2 * day},
		{"every 1w", week},
		{"every 1w2d", 9 * day},
	}
	for _, c := range ok {
		got, err := ValidateSchedule(c.schedule)
		if err != nil || got != c.want {
			t.Errorf("ValidateSchedule(%q) = %s, %v; want %s", c.schedule, got, err, c.want)
		}
	}

	bad := []struct{ schedule, msgContains string }{
		{"garbage-not-a-schedule", "unrecognized"},
		{"every banana", "unrecognized"},
		{"every 0d", "positive"},
		{"every 1x", "unrecognized"},
		{"30 * * * *", "minute"},
		{"0 9 * * *", "00:00 UTC"},
		{"0 0 * * 3", "Monday"},
		{"0 0 1 * *", "unrecognized"},
		{"0 0 * 6 *", "unrecognized"},
		{"*/5 * 1 * *", "unrecognized"},
	}
	for _, c := range bad {
		_, err := ValidateSchedule(c.schedule)
		if err == nil || !strings.Contains(err.Error(), c.msgContains) {
			t.Errorf("ValidateSchedule(%q) err = %v; want one containing %q", c.schedule, err, c.msgContains)
		}
	}
}

// TestParseScheduleInterval_Days keeps the tolerant reader (used for rows
// saved before validation existed) in step with the validator for the new
// day/week forms.
func TestParseScheduleInterval_Days(t *testing.T) {
	for s, want := range map[string]time.Duration{
		"every 1d": day, "every 1w": week, "0 0 * * *": day, "0 0 */2 * *": 2 * day, "0 0 * * 1": week,
	} {
		if got := ParseScheduleInterval(s); got != want {
			t.Errorf("ParseScheduleInterval(%q) = %s, want %s", s, got, want)
		}
	}
}
