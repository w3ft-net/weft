package shellcore

import (
	"testing"
	"time"
)

func TestParseRange_RelativeStart_OpenEnd(t *testing.T) {
	before := time.Now()
	start, end, err := parseRange("2h..")
	after := time.Now()

	if err != nil {
		t.Fatal(err)
	}

	// Start should be ~2 hours ago.
	expectedStart := before.Add(-2 * time.Hour)
	if start.Before(expectedStart.Add(-time.Second)) || start.After(expectedStart.Add(time.Second)) {
		t.Errorf("start=%v, expected ~%v", start, expectedStart)
	}

	// End should be ~now.
	if end.Before(before) || end.After(after) {
		t.Errorf("end=%v, expected between %v and %v", end, before, after)
	}
}

func TestParseRange_RelativeBothSides(t *testing.T) {
	start, end, err := parseRange("1h..10m")
	if err != nil {
		t.Fatal(err)
	}
	if !start.Before(end) {
		t.Errorf("start (%v) should be before end (%v)", start, end)
	}
	dur := end.Sub(start)
	// Should be ~50 minutes.
	if dur < 49*time.Minute || dur > 51*time.Minute {
		t.Errorf("expected ~50m span, got %v", dur)
	}
}

func TestParseRange_ISODates(t *testing.T) {
	start, end, err := parseRange("2026-04-01..2026-04-12")
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
	start, end, err := parseRange("2026-04-12T10:00..2026-04-12T11:00")
	if err != nil {
		t.Fatal(err)
	}
	dur := end.Sub(start)
	if dur != time.Hour {
		t.Errorf("expected 1h span, got %v", dur)
	}
}

func TestParseRange_Days(t *testing.T) {
	start, end, err := parseRange("7d..")
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
		_, _, err := parseRange(c)
		if err == nil {
			t.Errorf("expected error for %q, got nil", c)
		}
	}
}

func TestFormatRange(t *testing.T) {
	start := time.Date(2026, 4, 12, 10, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 12, 12, 0, 0, 0, time.UTC)
	out := formatRange(start, end)
	if out == "" {
		t.Error("empty output")
	}
	// Should contain the formatted times and a 2h duration.
	if !contains(out, "2026-04-12 10:00:00") {
		t.Errorf("missing start time in: %s", out)
	}
	if !contains(out, "2h") {
		t.Errorf("missing duration in: %s", out)
	}
}

func contains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
