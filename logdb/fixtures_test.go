package logdb

import "time"

// Test-only base times. Shared across logdb tests so window-bounded
// fixtures (shard windows, archival cutoffs) line up regardless of
// which file constructs them. Matches the constants in
// coordinator/coordinator_test.go so coordinator-side tests that pull
// these forward continue to read the same wall-clock instants.
var (
	t0 = time.Date(2026, 3, 28, 10, 0, 0, 0, time.UTC) // shard 0 start
	t1 = time.Date(2026, 3, 28, 11, 0, 0, 0, time.UTC) // shard 1 start / shard 0 end
	t2 = time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC) // shard 1 end
)

// fullRange covers both shards. Same purpose as the coordinator-side
// equivalent in coordinator_test.go.
var fullRange = TimeRange{Start: t0, End: t2}
