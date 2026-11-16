package coordinator

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/w3ft-net/weft/shape/shapetest"
)

// TestDrain_CompletedHourRemoved_CurrentHourReset confirms Drain's core
// contract: a past hour's bucket is returned and deleted (it will never
// accumulate more), while the current hour's bucket is returned but reset
// to zero rather than deleted, so Record calls after Drain keep landing
// in the same bucket instead of silently starting a new one.
func TestDrain_CompletedHourRemoved_CurrentHourReset(t *testing.T) {
	u := NewCustomerUsageStats()

	pastHour := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Hour)
	pastKey := usageKey{owner: shapetest.Added(5), hour: pastHour}
	pastCounter := &atomic.Int64{}
	pastCounter.Store(100)
	u.counts[pastKey] = pastCounter // seed a "past" bucket directly; Record always uses now()

	u.Record(shapetest.Added(5), 50) // lands in the current hour

	entries := u.Drain()
	if len(entries) != 2 {
		t.Fatalf("Drain() returned %d entries, want 2", len(entries))
	}

	var gotPast, gotCurrent int64
	for _, e := range entries {
		switch {
		case e.Hour.Equal(pastHour):
			gotPast = e.Bytes
		default:
			gotCurrent = e.Bytes
		}
	}
	if gotPast != 100 {
		t.Errorf("past hour delta = %d, want 100", gotPast)
	}
	if gotCurrent != 50 {
		t.Errorf("current hour delta = %d, want 50", gotCurrent)
	}

	// Past hour's bucket must be gone; current hour's must remain (reset).
	if _, ok := u.counts[pastKey]; ok {
		t.Error("past hour bucket still present after Drain, want removed")
	}
	if got := u.CurrentHour(shapetest.Added(5)); got != 0 {
		t.Errorf("current hour after Drain = %d, want 0 (reset, not deleted)", got)
	}

	// Recording again after Drain must accumulate from zero, in the same
	// bucket — proving the current-hour key survived rather than being
	// silently recreated with stale state.
	u.Record(shapetest.Added(5), 7)
	if got := u.CurrentHour(shapetest.Added(5)); got != 7 {
		t.Errorf("current hour after post-drain Record = %d, want 7", got)
	}
}

// TestDrain_ZeroByteBucketsOmitted mirrors the old DrainCompleted
// behavior: a bucket that never accumulated anything (bytes == 0)
// shouldn't produce a UsageEntry, since flushUsage would otherwise
// insert a pointless zero-byte row on every tick for idle customers.
func TestDrain_ZeroByteBucketsOmitted(t *testing.T) {
	u := NewCustomerUsageStats()
	u.Record(shapetest.Added(99), 0)

	entries := u.Drain()
	if len(entries) != 0 {
		t.Errorf("Drain() returned %d entries for a zero-byte bucket, want 0", len(entries))
	}
}

// TestDrain_SecondCallOnlyReturnsNewDelta confirms Drain never
// double-reports: calling it twice in a row with no Record in between
// returns nothing the second time, since the counter was already reset
// to zero by the first call.
func TestDrain_SecondCallOnlyReturnsNewDelta(t *testing.T) {
	u := NewCustomerUsageStats()
	u.Record(shapetest.Added(5), 30)

	first := u.Drain()
	if len(first) != 1 || first[0].Bytes != 30 {
		t.Fatalf("first Drain() = %+v, want one entry of 30 bytes", first)
	}

	second := u.Drain()
	if len(second) != 0 {
		t.Errorf("second Drain() with no intervening Record returned %+v, want none", second)
	}

	u.Record(shapetest.Added(5), 5)
	third := u.Drain()
	if len(third) != 1 || third[0].Bytes != 5 {
		t.Fatalf("third Drain() = %+v, want one entry of 5 bytes (not 35)", third)
	}
}
