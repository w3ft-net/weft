package ingest

import (
	"reflect"
	"testing"
)

// inferLayouts deduces a Go time layout from the texts alone: no table of
// known formats, only the calendar and the clock, and the requirement that
// time.Parse accepts every text with the result.
func TestInferLayouts(t *testing.T) {
	for name, c := range map[string]struct {
		texts []string
		want  []string
	}{
		"date and time, comma fraction": {
			[]string{"2026-10-15 12:00:06,850", "2026-10-15 12:00:07,001"},
			[]string{"2006-01-02 15:04:05,000"},
		},
		"date and time, dot fraction": {
			[]string{"2026-10-15 12:00:06.850", "2026-10-15 12:00:07.001"},
			[]string{"2006-01-02 15:04:05.000"},
		},
		"no fraction": {
			[]string{"2026-10-15 12:00:06", "2026-10-15 12:00:07"},
			[]string{"2006-01-02 15:04:05"},
		},
		"slash-separated year first": {
			[]string{"2026/10/15 12:00:06", "2026/10/15 12:00:07"},
			[]string{"2006/01/02 15:04:05"},
		},
		"T separator and Z": {
			[]string{"2026-10-15T12:00:06Z", "2026-10-15T12:00:07Z"},
			[]string{"2006-01-02T15:04:05Z07:00"},
		},
		"T separator and numeric offset": {
			[]string{"2026-10-15T12:00:06+02:00", "2026-10-15T12:00:07+02:00"},
			[]string{"2006-01-02T15:04:05-07:00"},
		},
		"month name, colon before the clock, numeric zone": {
			[]string{"15/Oct/2026:12:00:06 +0000", "15/Oct/2026:12:00:07 +0000"},
			[]string{"02/Jan/2006:15:04:05 -0700"},
		},
		"weekday, month name, zone abbreviation": {
			[]string{"Thu, 15 Oct 2026 12:00:06 UTC", "Fri, 16 Oct 2026 08:30:00 UTC"},
			[]string{"Mon, 02 Jan 2006 15:04:05 MST"},
		},
		"twelve-hour clock": {
			[]string{"2026-10-15 09:00:06 PM", "2026-10-15 10:00:07 PM"},
			[]string{"2006-01-02 03:04:05 PM"},
		},
		"date only": {
			[]string{"2026-10-15", "2026-10-16"},
			[]string{"2006-01-02"},
		},
		"a first component above 12 can only be the day": {
			[]string{"13/02/2026 10:00:00", "14/02/2026 11:00:00"},
			[]string{"02/01/2006 15:04:05"},
		},
		"a second component above 12 can only be the day, so the first is the month": {
			[]string{"02/13/2026 10:00:00", "02/14/2026 11:00:00"},
			[]string{"01/02/2006 15:04:05"},
		},
		"unpadded day and month": {
			[]string{"2026-1-5 12:00:06", "2026-10-15 12:00:07"},
			[]string{"2006-1-2 15:04:05"},
		},
	} {
		got := inferLayouts(c.texts)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n  got  %q\n  want %q", name, got, c.want)
		}
	}
}

// Things that are not timestamps give nothing.
func TestInferLayoutsRejects(t *testing.T) {
	for name, texts := range map[string][]string{
		"different shapes":        {"2026-10-15 12:00:06", "garbage"},
		"no year (time only)":     {"12:00:06", "12:00:07"},
		"no year (month and day)": {"Oct 15 12:00:06", "Oct 15 12:00:07"},
		"an epoch number":         {"1760529606", "1760529607"},
		"a version string":        {"1.2.3", "1.2.4"},
		"out-of-range clock":      {"2026-10-15 25:61:61", "2026-10-15 25:61:62"},
		"empty":                   {},
	} {
		if got := inferLayouts(texts); len(got) != 0 {
			t.Errorf("%s: got %q, want none", name, got)
		}
	}
}

// When day-first and month-first both parse, the one under which the lines
// come out in chronological order wins; if both do, both are returned.
func TestInferLayoutsRanksByChronology(t *testing.T) {
	day := "02/01/2006 15:04:05"
	month := "01/02/2006 15:04:05"

	// 03/02, 03/03, 03/04: day-first runs 3 Feb, 3 Mar, 3 Apr; month-first
	// runs Mar 2, Mar 3, Mar 4 -> both chronological.
	both := inferLayouts([]string{"03/02/2026 10:00:00", "03/03/2026 10:00:00", "03/04/2026 10:00:00"})
	if !reflect.DeepEqual(both, []string{day, month}) && !reflect.DeepEqual(both, []string{month, day}) {
		t.Errorf("ambiguous run should list both orders, got %q", both)
	}

	// 01/12 then 02/01: day-first reads 1 Dec then 2 Jan (backwards in
	// time); month-first reads Jan 12 then Feb 1 (forwards).
	got := inferLayouts([]string{"01/12/2026 10:00:00", "02/01/2026 10:00:00"})
	if len(got) == 0 || got[0] != month {
		t.Errorf("chronology should prefer month-first here, got %q", got)
	}
}
