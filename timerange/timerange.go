// Package timerange parses the user-typed range specs that weft's shell and
// API server use to bound queries. Specs are strings like "1d..", "30m..",
// "2026-04-01..2026-04-12", or the year-less "04-01..05-14" shorthand. The
// canonical resolver runs on the API server against its own clock per
// request, so a long-lived shell pane never queries against a stale window.
//
// This used to live in warp/pkg/shell but the parser is weft domain
// logic — warp itself never called it. Moved here to keep warp
// weft-agnostic.
package timerange

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseRange parses a range spec like "2h..", "2026-04-01..2026-04-12", etc.
// The format is <start>..<end>, where end is optional (defaults to now).
// Each side can be an ISO timestamp (any resolution), a year-less
// MM-DD[THH:MM] shorthand, a relative duration (30m, 2h, 7d), or a named
// reference (now, today, yesterday).
func ParseRange(spec string) (time.Time, time.Time, error) {
	return parseRange(spec, time.Now())
}

// parseRange is the now-injectable inner form for tests.
func parseRange(spec string, now time.Time) (time.Time, time.Time, error) {
	spec = strings.TrimSpace(spec)
	parts := strings.SplitN(spec, "..", 2)
	if len(parts) != 2 {
		return time.Time{}, time.Time{}, fmt.Errorf("expected <start>..<end> (e.g., 2h.. or 2026-04-01..2026-04-12)")
	}

	start, err := parseTimeSpec(strings.TrimSpace(parts[0]), now)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("start: %w", err)
	}

	var end time.Time
	endStr := strings.TrimSpace(parts[1])
	if endStr == "" {
		end = now
	} else {
		end, err = parseTimeSpec(endStr, now)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("end: %w", err)
		}
	}

	if !start.Before(end) {
		return time.Time{}, time.Time{}, fmt.Errorf("start must be before end")
	}

	return start, end, nil
}

// parseTimeSpec parses a single side of a range — relative duration, named
// reference, full ISO timestamp, or year-less MM-DD[THH:MM] shorthand.
func parseTimeSpec(s string, now time.Time) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("empty time spec")
	}

	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	switch strings.ToLower(s) {
	case "now":
		return now, nil
	case "today":
		return today, nil
	case "yesterday":
		return today.AddDate(0, 0, -1), nil
	}

	// Relative duration: <n><unit> where unit is m, h, or d.
	if len(s) >= 2 {
		unit := s[len(s)-1]
		if unit == 'm' || unit == 'h' || unit == 'd' {
			n, err := strconv.Atoi(s[:len(s)-1])
			if err == nil && n > 0 {
				switch unit {
				case 'm':
					return now.Add(-time.Duration(n) * time.Minute), nil
				case 'h':
					return now.Add(-time.Duration(n) * time.Hour), nil
				case 'd':
					return now.AddDate(0, 0, -n), nil
				}
			}
		}
	}

	// ISO timestamp formats, most specific first.
	isoFormats := []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02T15",
		"2006-01-02",
		"2006-01",
		"2006",
	}
	for _, f := range isoFormats {
		if t, err := time.Parse(f, s); err == nil {
			return t, nil
		}
	}

	// Year-less shorthand. MM-DD and MM-DDTHH:MM default the year to
	// now.Year() and the time (when absent) to 00:00:00 in now.Location().
	// No past-month roll-forward — "01-15" in March means current-year
	// January, not next January. Server-local timezone is fine because
	// we're comparing against log lines shipped from the same server.
	yearlessFormats := []struct {
		layout string
		hasTOD bool
	}{
		{"01-02T15:04", true},
		{"01-02T15", true},
		{"01-02", false},
	}
	for _, yf := range yearlessFormats {
		if t, err := time.Parse(yf.layout, s); err == nil {
			h, m, sec := 0, 0, 0
			if yf.hasTOD {
				h, m, sec = t.Hour(), t.Minute(), t.Second()
			}
			return time.Date(now.Year(), t.Month(), t.Day(), h, m, sec, 0, now.Location()), nil
		}
	}

	return time.Time{}, fmt.Errorf("cannot parse %q (expected ISO date/time or relative like 30m, 2h, 7d)", s)
}

// FormatRangePlain renders the range without ANSI colors (for
// non-interactive output, the browser chip, and the CLI one-shot path).
func FormatRangePlain(start, end time.Time) string {
	dur := end.Sub(start)
	return fmt.Sprintf("%s .. %s  [%s]\n",
		formatTimeSide(start), formatTimeSide(end), FormatDuration(dur))
}

func formatTimeSide(t time.Time) string {
	return t.Format("2006-01-02 15:04:05")
}

// FormatDuration renders a duration like "1d", "2h30m", "45m". Exported so
// the colored shell-side FormatRange (which depends on warp's ANSI helpers)
// can reuse it without duplicating the rules.
func FormatDuration(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	}
	days := int(d.Hours()) / 24
	h := int(d.Hours()) % 24
	if h == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd%dh", days, h)
}

// FormatTimeSide is exported for the colored shell-side renderer so it
// can keep formatting identical without duplicating the layout string.
func FormatTimeSide(t time.Time) string {
	return formatTimeSide(t)
}
