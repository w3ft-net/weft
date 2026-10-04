package coordinator

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"time"
)

// cronJitterMax caps how long past its window's close a cron may wait to fire.
const cronJitterMax = 30 * time.Second

// CronSlot is one scheduled firing of one cron. Slot k of interval I covers
// the epoch-aligned window that closes at k*I (for example 12:01:00 to
// 12:02:00 for a one-minute cron), and fires shortly after, at a per-watch
// delay. Every coordinator derives the same slot, key and window from
// (watch, interval, k) alone, regardless of when or where the run happens, so
// a claim on Key() makes a firing exactly-once, and consecutive windows tile
// without the gaps and overlaps that "[now-interval, now]" leaves after a
// handoff. All crons of one interval share identical windows; only the fire
// time differs, which keeps them from querying the shards on the same second.
type CronSlot struct {
	WatchID  uint
	Interval time.Duration
	Index    int64
}

// cronJitter is a stable per-watch delay in [0, min(interval/2, cronJitterMax))
// added after the window closes. FNV-1a over the big-endian watch ID is stable
// across processes and Go versions; changing it moves every cron's fire time
// (see the golden test). interval must be positive.
func cronJitter(watchID uint, interval time.Duration) time.Duration {
	span := interval / 2
	if span > cronJitterMax {
		span = cronJitterMax
	}
	if span < 1 {
		span = 1
	}
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(watchID))
	h := fnv.New64a()
	h.Write(b[:])
	return time.Duration(h.Sum64() % uint64(span))
}

// Boundary is the epoch-aligned instant this slot's window closes.
func (s CronSlot) Boundary() time.Time {
	return time.Unix(0, s.Index*int64(s.Interval)).UTC()
}

// Window is the span of data this firing covers. The planner's time bounds are
// closed (ts >= start AND ts <= end) at microsecond resolution, so End is one
// microsecond before the boundary: the window is [boundary-interval, boundary)
// and the next slot's window starts exactly where this one stops, with no row
// counted twice.
func (s CronSlot) Window() TimeRange {
	b := s.Boundary()
	return TimeRange{Start: b.Add(-s.Interval), End: b.Add(-time.Microsecond)}
}

// FireAt is when this slot becomes due: the boundary, plus settle (so rows
// still in flight can land before the window is queried), plus this watch's
// jitter.
func (s CronSlot) FireAt(settle time.Duration) time.Time {
	return s.Boundary().Add(settle + cronJitter(s.WatchID, s.Interval))
}

// Key names this firing for a claim. It includes the interval so a schedule
// edit starts a fresh keyspace instead of reusing old slot numbers.
func (s CronSlot) Key() string {
	return fmt.Sprintf("cron:%d:%s:%d", s.WatchID, s.Interval, s.Index)
}

// LatestDueSlot returns the most recent slot whose FireAt has passed at now.
// Only the latest is returned, so after an outage the missed slots are skipped
// rather than replayed. ok is false for an interval shorter than one
// microsecond (the resolution of the shard's ts column), which has no usable
// window.
func LatestDueSlot(watchID uint, interval, settle time.Duration, now time.Time) (CronSlot, bool) {
	if interval < time.Microsecond {
		return CronSlot{}, false
	}
	t := now.Add(-settle).UnixNano() - int64(cronJitter(watchID, interval))
	return CronSlot{WatchID: watchID, Interval: interval, Index: t / int64(interval)}, true
}
