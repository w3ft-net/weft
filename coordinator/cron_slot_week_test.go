package coordinator

import (
	"testing"
	"time"
)

// TestCronSlotWeeklyBoundaryIsMonday: the Unix epoch is a Thursday, so a
// plain epoch-aligned 7d grid would close its windows on Thursdays. Weekly
// (and multiple-of-weekly) slots are shifted so they close Monday 00:00 UTC.
func TestCronSlotWeeklyBoundaryIsMonday(t *testing.T) {
	week := 7 * 24 * time.Hour
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC) // a Wednesday
	slot, ok := LatestDueSlot(1, week, 0, now)
	if !ok {
		t.Fatal("no slot")
	}
	b := slot.Boundary()
	if b.Weekday() != time.Monday || b.Hour() != 0 || b.Minute() != 0 {
		t.Fatalf("boundary = %s (%s), want a Monday 00:00 UTC", b, b.Weekday())
	}
	if want := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC); !b.Equal(want) {
		t.Fatalf("boundary = %s, want %s", b, want)
	}
	if w := slot.Window(); !w.Start.Equal(b.Add(-week)) {
		t.Fatalf("window start = %s, want %s", w.Start, b.Add(-week))
	}
}

// Daily slots close at 00:00 UTC.
func TestCronSlotDailyBoundaryIsUTCMidnight(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	slot, _ := LatestDueSlot(1, 24*time.Hour, 0, now)
	if want := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC); !slot.Boundary().Equal(want) {
		t.Fatalf("boundary = %s, want %s", slot.Boundary(), want)
	}
}
