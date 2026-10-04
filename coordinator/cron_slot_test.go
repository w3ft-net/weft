package coordinator

import (
	"math/rand"
	"testing"
	"time"
)

// A slot is one scheduled firing of one cron: slot k of interval I covers the
// epoch-aligned window closing at k*I and fires shortly after, at a per-watch
// delay. Its key and window depend only on (watch, interval, k), never on when
// or where the run happens, so any number of coordinators, handoffs and
// restarts agree on what "this firing" is. These tests pin that contract.

var slotNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

const testSettle = 5 * time.Second

func mustSlot(t *testing.T, watchID uint, interval, settle time.Duration, now time.Time) CronSlot {
	t.Helper()
	s, ok := LatestDueSlot(watchID, interval, settle, now)
	if !ok {
		t.Fatalf("LatestDueSlot(%d, %s, %s, %s): not ok", watchID, interval, settle, now)
	}
	return s
}

// The jitter must land in [0, min(interval/2, 30s)) for every watch, so a
// slot always fires well before the next window closes.
func TestCronJitterWithinSpan(t *testing.T) {
	for _, interval := range []time.Duration{time.Second, 10 * time.Second, time.Minute, 5 * time.Minute, time.Hour, 6 * time.Hour} {
		span := interval / 2
		if span > 30*time.Second {
			span = 30 * time.Second
		}
		for id := uint(0); id < 2000; id++ {
			if j := cronJitter(id, interval); j < 0 || j >= span {
				t.Fatalf("cronJitter(%d, %s) = %s, want in [0, %s)", id, interval, j, span)
			}
		}
	}
}

// Jitter decides every cron's fire time. If the hash or the span rule ever
// changes, every cron shifts at the deploy, so the values are pinned. They
// were cross-checked against a separate hand-rolled FNV-1a 64 (offset basis
// 14695981039346656037, prime 1099511628211) over the 8 big-endian bytes of the
// ID, mod min(interval/2, 30s). Note 1m and 1h share a span, so they agree.
func TestCronJitterGolden(t *testing.T) {
	cases := []struct {
		id       uint
		interval time.Duration
		want     time.Duration
	}{
		{1, time.Minute, 23530546194},
		{42, time.Minute, 15739840919},
		{42, 10 * time.Second, 739840919},
		{42, time.Hour, 15739840919},
	}
	for _, c := range cases {
		if got := cronJitter(c.id, c.interval); got != c.want {
			t.Errorf("cronJitter(%d, %s) = %d ns, want %d ns", c.id, c.interval, got, c.want)
		}
	}
}

// The 35 dogfood crons share a 1-minute interval. They must not all fire on
// the same second: that would be a thundering herd on the shards every minute.
func TestCronJitterSpreadsSimilarCrons(t *testing.T) {
	seconds := map[int]bool{}
	for id := uint(1); id <= 35; id++ {
		seconds[int(cronJitter(id, time.Minute)/time.Second)] = true
	}
	if len(seconds) < 15 {
		t.Fatalf("35 one-minute crons spread over only %d distinct seconds, want >= 15", len(seconds))
	}
}

// Window boundaries are plain multiples of the interval since the epoch, the
// same for every watch, so crons of one interval cover identical windows and a
// one-minute window starts on a whole minute.
func TestWindowsAreEpochAligned(t *testing.T) {
	for _, interval := range []time.Duration{time.Minute, 5 * time.Minute, time.Hour} {
		index := slotNow.UnixNano() / int64(interval)
		want := CronSlot{WatchID: 1, Interval: interval, Index: index}
		for id := uint(1); id <= 20; id++ {
			s := CronSlot{WatchID: id, Interval: interval, Index: index}
			if got := s.Boundary().UnixNano() % int64(interval); got != 0 {
				t.Fatalf("interval %s watch %d: boundary %s is not a multiple of the interval (remainder %d ns)", interval, id, s.Boundary(), got)
			}
			if s.Window() != want.Window() {
				t.Fatalf("interval %s: watch %d has window %v, watch 1 has %v", interval, id, s.Window(), want.Window())
			}
		}
	}
	s := CronSlot{WatchID: 9, Interval: time.Minute, Index: slotNow.UnixNano() / int64(time.Minute)}
	if w := s.Window(); w.Start.Second() != 0 || w.Start.Nanosecond() != 0 {
		t.Fatalf("a one-minute window starts at %s, want a whole minute", w.Start)
	}
}

// A slot is due at boundary + settle + jitter and not a nanosecond before.
func TestLatestDueSlotBoundaries(t *testing.T) {
	const id, interval = uint(7), time.Minute
	first := mustSlot(t, id, interval, testSettle, slotNow)
	due := first.FireAt(testSettle)

	before := mustSlot(t, id, interval, testSettle, due.Add(-time.Nanosecond))
	at := mustSlot(t, id, interval, testSettle, due)
	last := mustSlot(t, id, interval, testSettle, due.Add(interval-time.Nanosecond))

	if before.Index != first.Index-1 {
		t.Errorf("one ns before FireAt: index %d, want %d (previous slot)", before.Index, first.Index-1)
	}
	if at.Index != first.Index {
		t.Errorf("at FireAt: index %d, want %d", at.Index, first.Index)
	}
	if last.Index != first.Index {
		t.Errorf("one ns before the next slot is due: index %d, want %d (still this slot)", last.Index, first.Index)
	}
}

// Consecutive windows tile: slot k's window ends one microsecond (the shard's
// ts resolution) before slot k+1's begins, so with the planner's closed bounds
// no row lies in two windows and none falls between them.
func TestSlotWindowsTileExactly(t *testing.T) {
	const id, interval = uint(13), 5 * time.Minute
	s := mustSlot(t, id, interval, testSettle, slotNow)
	for i := 0; i < 50; i++ {
		next := CronSlot{WatchID: id, Interval: interval, Index: s.Index + 1}
		w, nw := s.Window(), next.Window()
		if got := nw.Start.Sub(w.End); got != time.Microsecond {
			t.Fatalf("slot %d window ends %s, slot %d starts %s: %s apart, want 1µs", s.Index, w.End, next.Index, nw.Start, got)
		}
		if got := w.End.Sub(w.Start) + time.Microsecond; got != interval {
			t.Fatalf("window plus its 1µs spans %s, want exactly %s", got, interval)
		}
		s = next
	}
}

// Two coordinators evaluating at different instants while the same slot is the
// latest due must derive the identical key and window; that is what makes a
// claim exactly-once.
func TestSameSlotSameKeyAndWindow(t *testing.T) {
	const id, interval = uint(21), time.Minute
	s := mustSlot(t, id, interval, testSettle, slotNow)
	a := mustSlot(t, id, interval, testSettle, s.FireAt(testSettle).Add(100*time.Millisecond))
	b := mustSlot(t, id, interval, testSettle, s.FireAt(testSettle).Add(interval-time.Millisecond))
	if a.Key() != b.Key() || a.Key() != s.Key() {
		t.Fatalf("keys differ within one slot: %q %q %q", a.Key(), b.Key(), s.Key())
	}
	if a.Window() != b.Window() {
		t.Fatalf("windows differ within one slot: %v vs %v", a.Window(), b.Window())
	}
}

// Keys must never collide across watches, across slots, or across a schedule
// edit (same watch, different interval).
func TestSlotKeysAreDistinct(t *testing.T) {
	seen := map[string]string{}
	add := func(label string, s CronSlot) {
		t.Helper()
		if prev, dup := seen[s.Key()]; dup {
			t.Fatalf("key %q collides: %s and %s", s.Key(), prev, label)
		}
		seen[s.Key()] = label
	}
	for _, id := range []uint{1, 2, 3} {
		for _, interval := range []time.Duration{time.Minute, 5 * time.Minute} {
			s := mustSlot(t, id, interval, testSettle, slotNow)
			for i := int64(0); i < 3; i++ {
				add("slot", CronSlot{WatchID: id, Interval: interval, Index: s.Index + i})
			}
		}
	}

	// Same watch and same index under a different interval must still be a
	// different key, so a schedule edit can never reuse an old claim. At real
	// timestamps the indexes differ anyway; the key must not rely on that.
	a := CronSlot{WatchID: 1, Interval: time.Minute, Index: 7}
	b := CronSlot{WatchID: 1, Interval: 5 * time.Minute, Index: 7}
	if a.Key() == b.Key() {
		t.Fatalf("interval does not distinguish keys: both %q", a.Key())
	}
}

// After an outage every missed slot is skipped: only the latest due slot is
// returned, never a backlog.
func TestLatestDueSlotSkipsMissed(t *testing.T) {
	const id, interval = uint(3), time.Minute
	before := mustSlot(t, id, interval, testSettle, slotNow)
	after := mustSlot(t, id, interval, testSettle, slotNow.Add(10*interval))
	if after.Index != before.Index+10 {
		t.Fatalf("index after a 10-slot outage = %d, want %d", after.Index, before.Index+10)
	}
}

// The index never goes backwards as time advances, for any watch or interval.
func TestLatestDueSlotMonotonic(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for trial := 0; trial < 200; trial++ {
		id := uint(rng.Intn(10000))
		interval := time.Duration(1+rng.Intn(3600)) * time.Second
		settle := time.Duration(rng.Intn(10)) * time.Second
		t0 := slotNow.Add(time.Duration(rng.Int63n(int64(24 * time.Hour))))
		prev := mustSlot(t, id, interval, settle, t0)
		for step := 0; step < 50; step++ {
			t0 = t0.Add(time.Duration(rng.Int63n(int64(2 * interval))))
			cur := mustSlot(t, id, interval, settle, t0)
			if cur.Index < prev.Index {
				t.Fatalf("id=%d interval=%s: index went backwards %d -> %d", id, interval, prev.Index, cur.Index)
			}
			prev = cur
		}
	}
}

// A slot is due only once its FireAt has passed, and the following slot is not
// due yet, whatever the watch, interval or settle.
func TestLatestDueSlotNeverEarlyOrLate(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	for trial := 0; trial < 500; trial++ {
		id := uint(rng.Intn(10000))
		interval := time.Duration(1+rng.Intn(7200)) * time.Second
		settle := time.Duration(rng.Intn(30)) * time.Second
		now := slotNow.Add(time.Duration(rng.Int63n(int64(48 * time.Hour))))
		s := mustSlot(t, id, interval, settle, now)
		if due := s.FireAt(settle); due.After(now) {
			t.Fatalf("id=%d interval=%s settle=%s: FireAt %s is after now %s", id, interval, settle, due, now)
		}
		next := CronSlot{WatchID: id, Interval: interval, Index: s.Index + 1}
		if due := next.FireAt(settle); !due.After(now) {
			t.Fatalf("id=%d interval=%s: the next slot (due %s) was already due at %s", id, interval, due, now)
		}
	}
}

// A schedule shorter than the shard's timestamp resolution has no usable
// window, so it has no slots.
func TestLatestDueSlotRejectsUnusableInterval(t *testing.T) {
	for _, interval := range []time.Duration{0, -time.Minute, 500 * time.Nanosecond} {
		if _, ok := LatestDueSlot(1, interval, 0, slotNow); ok {
			t.Errorf("interval %s: got a slot, want none", interval)
		}
	}
}
