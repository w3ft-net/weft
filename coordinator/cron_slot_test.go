package coordinator

import (
	"math/rand"
	"testing"
	"time"
)

// A slot is one scheduled firing of one cron, named the same way by every
// coordinator: slot k of interval I fires at k*I + offset(watch), where the
// per-watch offset spreads crons of the same interval across it. Because the
// name and the query window depend only on (watch, interval, k) and never on
// when or where the run happens, any number of coordinators, handoffs and
// restarts agree on what "this firing" is. These tests pin that contract.

var slotNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func mustSlot(t *testing.T, watchID uint, interval, settle time.Duration, now time.Time) CronSlot {
	t.Helper()
	s, ok := LatestDueSlot(watchID, interval, settle, now)
	if !ok {
		t.Fatalf("LatestDueSlot(%d, %s, %s, %s): not ok", watchID, interval, settle, now)
	}
	return s
}

// The offset must land in [0, interval) for every watch, or a slot's nominal
// time could drift outside its own interval.
func TestCronOffsetWithinInterval(t *testing.T) {
	for _, interval := range []time.Duration{time.Second, time.Minute, 5 * time.Minute, time.Hour, 6 * time.Hour} {
		for id := uint(0); id < 2000; id++ {
			if off := cronOffset(id, interval); off < 0 || off >= interval {
				t.Fatalf("cronOffset(%d, %s) = %s, want in [0, %s)", id, interval, off, interval)
			}
		}
	}
}

// Offsets decide every cron's phase. If the hash ever changes, every cron
// shifts at the deploy, so the values are pinned. They were cross-checked
// against a separate hand-rolled FNV-1a 64 (offset basis 14695981039346656037,
// prime 1099511628211) over the 8 big-endian bytes of the ID, mod the interval.
func TestCronOffsetGolden(t *testing.T) {
	cases := []struct {
		id       uint
		interval time.Duration
		want     time.Duration
	}{
		{1, time.Minute, 53530546194},
		{42, time.Minute, 45739840919},
		{42, time.Hour, 3225739840919},
	}
	for _, c := range cases {
		if got := cronOffset(c.id, c.interval); got != c.want {
			t.Errorf("cronOffset(%d, %s) = %d ns, want %d ns", c.id, c.interval, got, c.want)
		}
	}
}

// The 35 dogfood crons share a 1-minute interval. They must not all fire on
// the same second: that would be a thundering herd on the shards every minute.
func TestCronOffsetSpreadsSimilarCrons(t *testing.T) {
	seconds := map[int]bool{}
	for id := uint(1); id <= 35; id++ {
		seconds[int(cronOffset(id, time.Minute)/time.Second)] = true
	}
	if len(seconds) < 20 {
		t.Fatalf("35 one-minute crons fire on only %d distinct seconds, want >= 20", len(seconds))
	}
}

// A slot is due at its nominal time (plus settle) and not a nanosecond before.
func TestLatestDueSlotBoundaries(t *testing.T) {
	const id, interval = uint(7), time.Minute
	settle := 5 * time.Second

	// Find a slot's nominal time, then probe either side of nominal+settle.
	first := mustSlot(t, id, interval, 0, slotNow)
	due := first.Nominal().Add(settle)

	before := mustSlot(t, id, interval, settle, due.Add(-time.Nanosecond))
	at := mustSlot(t, id, interval, settle, due)
	after := mustSlot(t, id, interval, settle, due.Add(interval-time.Nanosecond))

	if before.Index != first.Index-1 {
		t.Errorf("just before nominal+settle: index %d, want %d (previous slot)", before.Index, first.Index-1)
	}
	if at.Index != first.Index {
		t.Errorf("at nominal+settle: index %d, want %d", at.Index, first.Index)
	}
	if after.Index != first.Index {
		t.Errorf("just before the next slot is due: index %d, want %d (still this slot)", after.Index, first.Index)
	}
}

// The window is the interval ending at the slot's nominal time, so
// consecutive slots abut exactly: no gap and no overlap, whoever ran them.
func TestSlotWindowsTileExactly(t *testing.T) {
	const id, interval = uint(13), 5 * time.Minute
	s := mustSlot(t, id, interval, 0, slotNow)
	for i := 0; i < 50; i++ {
		next := CronSlot{WatchID: id, Interval: interval, Index: s.Index + 1}
		w, nw := s.Window(), next.Window()
		if !w.End.Equal(nw.Start) {
			t.Fatalf("slot %d window ends %s but slot %d window starts %s", s.Index, w.End, next.Index, nw.Start)
		}
		if got := w.End.Sub(w.Start); got != interval {
			t.Fatalf("window spans %s, want exactly %s", got, interval)
		}
		if !w.End.Equal(s.Nominal()) {
			t.Fatalf("window end %s != nominal %s", w.End, s.Nominal())
		}
		s = next
	}
}

// Two coordinators evaluating at different instants inside the same slot must
// derive the identical key and window; that is what makes a claim exactly-once.
func TestSameSlotSameKeyAndWindow(t *testing.T) {
	const id, interval = uint(21), time.Minute
	s := mustSlot(t, id, interval, 0, slotNow)
	a := mustSlot(t, id, interval, 0, s.Nominal().Add(100*time.Millisecond))
	b := mustSlot(t, id, interval, 0, s.Nominal().Add(interval-time.Millisecond))
	if a.Key() != b.Key() || a.Key() != s.Key() {
		t.Fatalf("keys differ within one slot: %q %q %q", a.Key(), b.Key(), s.Key())
	}
	if a.Window() != b.Window() {
		t.Fatalf("windows differ within one slot: %v vs %v", a.Window(), b.Window())
	}
}

// Keys must never collide across watches, across slots, or across a schedule
// edit (same watch, same instant, different interval).
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
			s := mustSlot(t, id, interval, 0, slotNow)
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
	before := mustSlot(t, id, interval, 0, slotNow)
	after := mustSlot(t, id, interval, 0, slotNow.Add(10*interval))
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

// A slot is due only once its nominal time has passed, whatever the offset.
func TestLatestDueSlotNeverEarly(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	for trial := 0; trial < 500; trial++ {
		id := uint(rng.Intn(10000))
		interval := time.Duration(1+rng.Intn(7200)) * time.Second
		settle := time.Duration(rng.Intn(30)) * time.Second
		now := slotNow.Add(time.Duration(rng.Int63n(int64(48 * time.Hour))))
		s := mustSlot(t, id, interval, settle, now)
		if due := s.Nominal().Add(settle); due.After(now) {
			t.Fatalf("id=%d interval=%s settle=%s: slot nominal+settle %s is after now %s", id, interval, settle, due, now)
		}
		if next := (CronSlot{WatchID: id, Interval: interval, Index: s.Index + 1}).Nominal().Add(settle); !next.After(now) {
			t.Fatalf("id=%d interval=%s: the next slot (due %s) was already due at %s", id, interval, next, now)
		}
	}
}

// A schedule the parser cannot turn into a positive interval has no slots.
func TestLatestDueSlotRejectsNonPositiveInterval(t *testing.T) {
	for _, interval := range []time.Duration{0, -time.Minute} {
		if _, ok := LatestDueSlot(1, interval, 0, slotNow); ok {
			t.Errorf("interval %s: got a slot, want none", interval)
		}
	}
}
