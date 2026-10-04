package coordinator

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"time"
)

// CronSlot is one scheduled firing of one cron. Slot k of interval I fires at
// k*I + offset(watch) on the Unix epoch, so every coordinator derives the same
// slot, key and query window from (watch, interval, k) alone, regardless of
// when or where the run happens. That is what lets a claim on Key() make a
// firing exactly-once, and what makes consecutive windows tile without the
// gaps and overlaps that "[now-interval, now]" leaves after a handoff.
type CronSlot struct {
	WatchID  uint
	Interval time.Duration
	Index    int64
}

// cronOffset places a watch inside its interval so crons sharing an interval
// do not all fire on the same second. FNV-1a over the big-endian watch ID is
// stable across processes and Go versions; changing it moves every cron's
// phase (see the golden test). interval must be positive.
func cronOffset(watchID uint, interval time.Duration) time.Duration {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(watchID))
	h := fnv.New64a()
	h.Write(b[:])
	return time.Duration(h.Sum64() % uint64(interval))
}

// Nominal is the instant this slot is scheduled to fire.
func (s CronSlot) Nominal() time.Time {
	return time.Unix(0, s.Index*int64(s.Interval)+int64(cronOffset(s.WatchID, s.Interval))).UTC()
}

// Window is the span of data this firing covers: the interval ending at the
// slot's nominal time. Slot k's window ends exactly where slot k+1's begins.
func (s CronSlot) Window() TimeRange {
	n := s.Nominal()
	return TimeRange{Start: n.Add(-s.Interval), End: n}
}

// Key names this firing for a claim. It includes the interval so a schedule
// edit starts a fresh keyspace instead of colliding with old slot numbers.
func (s CronSlot) Key() string {
	return fmt.Sprintf("cron:%d:%s:%d", s.WatchID, s.Interval, s.Index)
}

// LatestDueSlot returns the most recent slot whose nominal time plus settle
// has passed at now. Only the latest is returned, so after an outage the
// missed slots are skipped rather than replayed. settle lets late-arriving
// rows land before a window is queried. ok is false for a non-positive
// interval, which has no slots.
func LatestDueSlot(watchID uint, interval, settle time.Duration, now time.Time) (CronSlot, bool) {
	if interval <= 0 {
		return CronSlot{}, false
	}
	t := now.Add(-settle).UnixNano() - int64(cronOffset(watchID, interval))
	return CronSlot{WatchID: watchID, Interval: interval, Index: t / int64(interval)}, true
}
