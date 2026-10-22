package coordinator

import (
	"io"
	"strings"
	"testing"
	"time"
)

// Durations in weft:: functions take Go syntax plus d (24h) and w (7d), the
// units the docs and `range` already use. `weft::avg x 1d` and
// `is_seasonal ... 1d 4w` were rejected as invalid.
func TestParseDuration_DaysAndWeeks(t *testing.T) {
	ok := map[string]time.Duration{
		"30s":   30 * time.Second,
		"90m":   90 * time.Minute,
		"24h":   24 * time.Hour,
		"1d":    24 * time.Hour,
		"2d":    48 * time.Hour,
		"1w":    7 * 24 * time.Hour,
		"4w":    28 * 24 * time.Hour,
		"1d12h": 36 * time.Hour,
		"1w2d":  9 * 24 * time.Hour,
		"1.5d":  36 * time.Hour,
		"0.5w":  84 * time.Hour,
		"1h30m": 90 * time.Minute,
		"500ms": 500 * time.Millisecond,
	}
	for in, want := range ok {
		got, err := parseDuration(in)
		if err != nil || got != want {
			t.Errorf("parseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "d", "w", "1x", "1dd", "1days", "one day", "1 d", "d1", "1d-2h"} {
		if got, err := parseDuration(in); err == nil {
			t.Errorf("parseDuration(%q) = %v, want an error", in, got)
		}
	}
}

// Sum-based aggregates reach as far back as the daily rollups (400d); funcs
// that need individual results stop at the 7d of raw data. "1w" is exactly
// the raw cap and "2w" is over it.
func TestDurationCaps_WithDayAndWeekUnits(t *testing.T) {
	if d, err := ParseAggDuration("400d"); err != nil || d != 400*24*time.Hour {
		t.Errorf("ParseAggDuration(400d) = %v, %v", d, err)
	}
	if _, err := ParseAggDuration("401d"); err == nil || !strings.Contains(err.Error(), "exceeds maximum") {
		t.Errorf("ParseAggDuration(401d) = %v, want the 400d cap error", err)
	}
	if d, err := ParseRawDuration("1w"); err != nil || d != 7*24*time.Hour {
		t.Errorf("ParseRawDuration(1w) = %v, %v", d, err)
	}
	if _, err := ParseRawDuration("2w"); err == nil || !strings.Contains(err.Error(), "exceeds maximum") {
		t.Errorf("ParseRawDuration(2w) = %v, want the 7d cap error", err)
	}
}

func TestPredicateDurationArg_DaysAndWeeks(t *testing.T) {
	for _, in := range []string{"1d", "4w", "1d12h"} {
		if _, ok := parseDurArg(in, "window", io.Discard); !ok {
			t.Errorf("predicate duration %q should parse", in)
		}
	}
	if _, ok := parseDurArg("1fortnight", "window", io.Discard); ok {
		t.Error("an unknown unit must still be rejected")
	}
}
