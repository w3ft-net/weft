package logdb

import "time"

// TimeRange specifies a half-open time window used by query routing
// (coordinator-side, to compute which shards overlap the requested
// range) and by shard rotation. Half-open in the conventional sense:
// MatchingEntries treats Start as inclusive and End as exclusive when
// it compares against shard window boundaries.
type TimeRange struct {
	Start time.Time
	End   time.Time
}
