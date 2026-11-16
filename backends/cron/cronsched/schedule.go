package cronsched

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseScheduleInterval returns the fixed cadence at which a stored schedule
// fires. Schedules that pass ValidateSchedule map exactly; anything else is a
// row saved before validation existed, and keeps the lenient reading it has
// always had so that it keeps firing as it did (see legacyScheduleInterval).
func ParseScheduleInterval(schedule string) time.Duration {
	if d, err := ValidateSchedule(schedule); err == nil {
		return d
	}
	return legacyScheduleInterval(schedule)
}

const (
	scheduleDay  = 24 * time.Hour
	scheduleWeek = 7 * scheduleDay
)

// ValidateSchedule parses a schedule the strict way, for save paths. Crons
// fire on a UTC-aligned grid of uniform intervals, never at a chosen time of
// day, so a schedule that names one (or that cannot be read at all) is an
// error rather than a cron quietly running at some other cadence.
//
// Accepted forms:
//
//   - "every <duration>", with w, d, h, m and s units that may be combined
//     ("every 90s", "every 1d", "every 1w2d")
//   - "* * * * *"      every minute
//   - "*/N * * * *"    every N minutes
//   - "0 * * * *"      every hour
//   - "0 */H * * *"    every H hours
//   - "0 0 * * *"      every day, closing 00:00 UTC
//   - "0 0 */N * *"    every N days
//   - "0 0 * * 1"      every week, closing Monday 00:00 UTC
func ValidateSchedule(schedule string) (time.Duration, error) {
	unrecognized := fmt.Errorf("unrecognized schedule %q: use a crontab such as */5 * * * *, 0 * * * *, 0 0 * * * or 0 0 * * 1, or an interval such as every 5m, every 1d or every 1w", schedule)

	if rest, ok := strings.CutPrefix(schedule, "every "); ok {
		d, ok := parseIntervalDuration(strings.TrimSpace(rest))
		if !ok {
			return 0, unrecognized
		}
		if d <= 0 {
			return 0, fmt.Errorf("schedule %q: interval must be positive", schedule)
		}
		return d, nil
	}

	f := strings.Fields(schedule)
	if len(f) != 5 || f[3] != "*" {
		return 0, unrecognized
	}
	minute, hour, dom, dow := f[0], f[1], f[2], f[4]

	if minute == "*" || strings.HasPrefix(minute, "*/") {
		if hour != "*" || dom != "*" || dow != "*" {
			return 0, unrecognized
		}
		if minute == "*" {
			return time.Minute, nil
		}
		n, ok := cronStep(minute)
		if !ok {
			return 0, unrecognized
		}
		return time.Duration(n) * time.Minute, nil
	}

	if !isFixedField(minute, 59) {
		return 0, unrecognized
	}
	if minute != "0" {
		return 0, fmt.Errorf("schedule %q: crons fire on the hour, so the minute field must be 0 (or use */N for every N minutes)", schedule)
	}

	if hour == "*" || strings.HasPrefix(hour, "*/") {
		if dom != "*" || dow != "*" {
			return 0, unrecognized
		}
		if hour == "*" {
			return time.Hour, nil
		}
		n, ok := cronStep(hour)
		if !ok {
			return 0, unrecognized
		}
		return time.Duration(n) * time.Hour, nil
	}

	if !isFixedField(hour, 23) {
		return 0, unrecognized
	}
	if hour != "0" {
		return 0, fmt.Errorf("schedule %q: daily and weekly crons close at 00:00 UTC and cannot run at a chosen time of day; use 0 0 * * * (or every 1d)", schedule)
	}

	switch {
	case dom == "*" && dow == "*":
		return scheduleDay, nil
	case dow == "*":
		n, ok := cronStep(dom)
		if !ok {
			return 0, unrecognized
		}
		return time.Duration(n) * scheduleDay, nil
	case dom == "*" && dow == "1":
		return scheduleWeek, nil
	case dom == "*" && isFixedField(dow, 7):
		return 0, fmt.Errorf("schedule %q: weekly crons close on Monday 00:00 UTC; use 0 0 * * 1 (or every 1w)", schedule)
	}
	return 0, unrecognized
}

// parseIntervalDuration reads a combined duration of w, d, h, m and s units
// ("1w2d", "1h30m"). It is time.ParseDuration plus days and weeks, without
// fractions.
func parseIntervalDuration(s string) (time.Duration, bool) {
	if s == "" {
		return 0, false
	}
	var total time.Duration
	for s != "" {
		i := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == 0 || i == len(s) {
			return 0, false
		}
		n, err := strconv.Atoi(s[:i])
		if err != nil {
			return 0, false
		}
		var unit time.Duration
		switch s[i] {
		case 'w':
			unit = scheduleWeek
		case 'd':
			unit = scheduleDay
		case 'h':
			unit = time.Hour
		case 'm':
			unit = time.Minute
		case 's':
			unit = time.Second
		default:
			return 0, false
		}
		total += time.Duration(n) * unit
		s = s[i+1:]
	}
	return total, true
}

// legacyScheduleInterval is the lenient reader schedules were parsed with
// before ValidateSchedule: only the minute and hour fields count, a fixed
// minute is ignored, and anything unrecognized is 5 minutes. It remains only
// so rows saved under those rules keep their cadence.
func legacyScheduleInterval(schedule string) time.Duration {
	if strings.HasPrefix(schedule, "every ") {
		if d, err := time.ParseDuration(strings.TrimPrefix(schedule, "every ")); err == nil {
			return d
		}
	}

	fields := strings.Fields(schedule)
	if len(fields) == 5 {
		minute, hour := fields[0], fields[1]
		if minute == "*" {
			return time.Minute
		}
		if n, ok := cronStep(minute); ok {
			return time.Duration(n) * time.Minute
		}
		if isFixedField(minute, 59) {
			if hour == "*" {
				return time.Hour
			}
			if n, ok := cronStep(hour); ok {
				return time.Duration(n) * time.Hour
			}
		}
	}

	return 5 * time.Minute
}

// cronStep parses a "*/N" cron field, returning (N, true) if it's a
// valid positive step, or (0, false) otherwise.
func cronStep(field string) (int, bool) {
	nStr, ok := strings.CutPrefix(field, "*/")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(nStr)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// isFixedField reports whether field is a literal integer in [0, max] —
// i.e. a specific minute/hour rather than "*" or a "*/N" step.
func isFixedField(field string, max int) bool {
	n, err := strconv.Atoi(field)
	return err == nil && n >= 0 && n <= max
}
