package shellcore

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// parseRange parses a range spec like "2h..", "2026-04-01..2026-04-12",
// "30m..10m", or "2026-04-12T10:00..".
// The format is <start>..<end>, where end is optional (defaults to now).
// Each side can be an ISO timestamp (any resolution) or a relative duration
// like 30m, 2h, 7d (meaning "that long ago from now").
// ParseRange parses a range spec like "2h..", "2026-04-01..2026-04-12", etc.
func ParseRange(spec string) (time.Time, time.Time, error) {
	return parseRange(spec)
}

func parseRange(spec string) (time.Time, time.Time, error) {
	spec = strings.TrimSpace(spec)
	parts := strings.SplitN(spec, "..", 2)
	if len(parts) != 2 {
		return time.Time{}, time.Time{}, fmt.Errorf("expected <start>..<end> (e.g., 2h.. or 2026-04-01..2026-04-12)")
	}

	now := time.Now()

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

// parseTimeSpec parses either a relative duration (30m, 2h, 7d) or an ISO
// timestamp at any resolution.
func parseTimeSpec(s string, now time.Time) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("empty time spec")
	}

	// Named time references.
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	switch strings.ToLower(s) {
	case "now":
		return now, nil
	case "today":
		return today, nil
	case "yesterday":
		return today.AddDate(0, 0, -1), nil
	}

	// Try relative duration: <n><unit> where unit is m, h, or d.
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

	// Try ISO timestamp formats, most specific first.
	formats := []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02T15",
		"2006-01-02",
		"2006-01",
		"2006",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t, nil
		}
	}

	return time.Time{}, fmt.Errorf("cannot parse %q (expected ISO date/time or relative like 30m, 2h, 7d)", s)
}

// FormatRangePlain renders the range without ANSI colors (for non-interactive output).
func FormatRangePlain(start, end time.Time) string {
	dur := end.Sub(start)
	return fmt.Sprintf("%s .. %s  [%s]\n",
		formatTimeSide(start), formatTimeSide(end), formatDuration(dur))
}

// formatRange renders the current time range for display.
func formatRange(start, end time.Time) string {
	now := time.Now()
	var b strings.Builder

	b.WriteString(DimText("range: "))
	b.WriteString(formatTimeSide(start))
	b.WriteString(DimText(" .. "))
	b.WriteString(formatTimeSide(end))

	// If end is very close to now, annotate it.
	if now.Sub(end).Abs() < 2*time.Second {
		b.WriteString(DimText("  (now)"))
	}

	// Show the span.
	dur := end.Sub(start)
	b.WriteString(DimText(fmt.Sprintf("  [%s]", formatDuration(dur))))
	b.WriteByte('\n')

	return b.String()
}

func formatTimeSide(t time.Time) string {
	return t.Format("2006-01-02 15:04:05")
}

func formatDuration(d time.Duration) string {
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
