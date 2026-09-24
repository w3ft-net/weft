package logdb

import "time"

// Test-only base times. Shared across logdb tests so window-bounded
// fixtures (shard windows, archival cutoffs) line up regardless of
// which file constructs them. Matches the constants in
// coordinator/coordinator_test.go so coordinator-side tests that pull
// these forward continue to read the same wall-clock instants.
//
// Relative to time.Now(), not a fixed calendar date: a query's time
// range gets floored at (now - LogRetention(tier)) — see
// coordinator.go's clampTimeRange / store.ClampRetention — and a fixed
// absolute date here eventually falls outside that floor as real time
// advances, no matter how far out it's set. This bit on 2026-09-24: a
// prior fixed date of 2026-03-28 was exactly 180 days (TierBolt's
// retention) before that day, so every fixture query clamped past the
// shard windows and every test relying on them failed with "no
// matching shards" / equivalent zero-row results, for a day nobody
// touched this code. -3h keeps it comfortably inside even the 24h
// trial-tier default.
var (
	t0 = time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour) // shard 0 start
	t1 = t0.Add(1 * time.Hour)                                    // shard 1 start / shard 0 end
	t2 = t1.Add(1 * time.Hour)                                    // shard 1 end
)

// fullRange covers both shards. Same purpose as the coordinator-side
// equivalent in coordinator_test.go.
var fullRange = TimeRange{Start: t0, End: t2}
