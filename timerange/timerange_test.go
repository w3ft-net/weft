package timerange

import (
	"strings"
	"testing"
	"time"
)

func TestParseRange_RelativeStart_OpenEnd(t *testing.T) {
	before := time.Now()
	start, end, err := ParseRange("2h..")
	after := time.Now()

	if err != nil {
		t.Fatal(err)
	}

	expectedStart := before.Add(-2 * time.Hour)
	if start.Before(expectedStart.Add(-time.Second)) || start.After(expectedStart.Add(time.Second)) {
		t.Errorf("start=%v, expected ~%v", start, expectedStart)
	}

	if end.Before(before) || end.After(after) {
		t.Errorf("end=%v, expected between %v and %v", end, before, after)
	}
}

func TestParseRange_RelativeBothSides(t *testing.T) {
	start, end, err := ParseRange("1h..10m")
	if err != nil {
		t.Fatal(err)
	}
	if !start.Before(end) {
		t.Errorf("start (%v) should be before end (%v)", start, end)
	}
	dur := end.Sub(start)
	if dur < 49*time.Minute || dur > 51*time.Minute {
		t.Errorf("expected ~50m span, got %v", dur)
	}
}

func TestParseRange_ISODates(t *testing.T) {
	start, end, err := ParseRange("2026-04-01..2026-04-12")
	if err != nil {
		t.Fatal(err)
	}
	if start.Format("2006-01-02") != "2026-04-01" {
		t.Errorf("start=%v", start)
	}
	if end.Format("2006-01-02") != "2026-04-12" {
		t.Errorf("end=%v", end)
	}
}

func TestParseRange_ISODateTimes(t *testing.T) {
	start, end, err := ParseRange("2026-04-12T10:00..2026-04-12T11:00")
	if err != nil {
		t.Fatal(err)
	}
	dur := end.Sub(start)
	if dur != time.Hour {
		t.Errorf("expected 1h span, got %v", dur)
	}
}

func TestParseRange_Days(t *testing.T) {
	start, end, err := ParseRange("7d..")
	if err != nil {
		t.Fatal(err)
	}
	dur := end.Sub(start)
	if dur < 6*24*time.Hour+23*time.Hour || dur > 7*24*time.Hour+time.Minute {
		t.Errorf("expected ~7d span, got %v", dur)
	}
}

func TestParseRange_BadFormat(t *testing.T) {
	cases := []string{
		"",
		"2h",         // no ..
		"..2h",       // empty start
		"2h..1h..3h", // too many ..
		"abc..def",   // unparseable
		"10m..1h",    // start after end
	}
	for _, c := range cases {
		_, _, err := ParseRange(c)
		if err == nil {
			t.Errorf("expected error for %q, got nil", c)
		}
	}
}

// TestParseTimeSpec_YearLessShorthand pins the MM-DD and MM-DDTHH:MM
// shorthands. The year defaults to now.Year() and an omitted time
// component defaults to 00:00:00 (start of day in now.Location()). These
// are additions — explicit-year ISO forms keep working.
func TestParseTimeSpec_YearLessShorthand(t *testing.T) {
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		spec string
		want time.Time
	}{
		{"05-14", time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC)},
		{"04-01", time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)},
		{"01-15", time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)}, // no past-month roll-forward
		{"12-31", time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)},
		{"05-14T09:30", time.Date(2026, 5, 14, 9, 30, 0, 0, time.UTC)},
		{"05-14T17:00", time.Date(2026, 5, 14, 17, 0, 0, 0, time.UTC)},
		// Explicit-year forms still work — the shorthand is additive.
		{"2025-12-31", time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC)},
		{"2026-04-12T11:00", time.Date(2026, 4, 12, 11, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		got, err := parseTimeSpec(c.spec, now)
		if err != nil {
			t.Errorf("parseTimeSpec(%q): unexpected error %v", c.spec, err)
			continue
		}
		if !got.Equal(c.want) {
			t.Errorf("parseTimeSpec(%q) = %v, want %v", c.spec, got, c.want)
		}
	}
}

// TestParseRange_YearlessBothSides locks in the round-trip so a user-typed
// `range 04-01..05-14` resolves both ends to current-year midnight without
// falling back to relative interpretation.
func TestParseRange_YearlessBothSides(t *testing.T) {
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	start, end, err := parseRange("04-01..05-14", now)
	if err != nil {
		t.Fatal(err)
	}
	if !start.Equal(time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("start = %v, want 2026-04-01T00:00:00", start)
	}
	if !end.Equal(time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("end = %v, want 2026-05-14T00:00:00", end)
	}
	dur := end.Sub(start)
	want := 43 * 24 * time.Hour
	if dur != want {
		t.Errorf("04-01..05-14 span = %v, want %v", dur, want)
	}
}

func TestFormatRangePlain(t *testing.T) {
	start := time.Date(2026, 4, 12, 10, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 12, 12, 0, 0, 0, time.UTC)
	out := FormatRangePlain(start, end)
	if !strings.Contains(out, "2026-04-12 10:00:00") {
		t.Errorf("missing start time in: %s", out)
	}
	if !strings.Contains(out, "2h") {
		t.Errorf("missing duration in: %s", out)
	}
}
