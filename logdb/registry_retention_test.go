package logdb

import (
	"testing"
	"time"

	"github.com/kevlarcon/weft/store"
)

// TestMatchingEntries_ClampsToPlanRetention is an **intentionally failing**
// test that demonstrates the log-query analog of
// `TestCronAggregate_ClampsToPlanRetention` (store/rollup_test.go).
//
// On the metrics side we already clamp `since` up to
// now − LogRetention(tier) before reading rollup rows, so a swatch
// customer (30d cap) asking for a year back only sees the last 30d of
// metric buckets.
//
// The log-query path does not clamp. The coordinator hands the caller's
// raw TimeRange straight to Registry.MatchingEntries, which filters by
// overlap with the query window and app membership — neither field
// knows about the caller's plan. A swatch customer asking for a year
// back will therefore reach shards whose windows fall outside their
// advertised 30-day retention, contradicting the pricing-page promise.
//
// This test sets up three read-only shards owning the same app for a
// single (hypothetically-swatch) tenant:
//   - recent:   [-10d, -5d]      — entirely inside 30d retention
//   - spanning: [-45d, -20d]     — straddles the 30d boundary
//   - old:      [-200d, -195d]   — entirely outside 30d retention
//
// It calls MatchingEntries with a 1-year window and asserts that:
//   - `recent`   matches (trivially in-window)
//   - `spanning` matches (some rows are still visible, -30d..-20d)
//   - `old`      does NOT match (every row is past retention)
//
// The spanning case is a guard: the eventual fix must not over-prune
// by dropping every shard whose *start* is before the cap. A shard
// that straddles the boundary still has visible rows inside it; the
// SQL predicate sent to that shard is separately responsible for
// hiding the portion before the cap.
//
// The `old` assertion fails today because no tier-aware clamp exists
// on this path.
//
// Fix direction (for the future change that makes this pass): either
// clamp tr.Start to now − LogRetention(tier) in coordinator.query
// before the MatchingEntries call (mirroring clampRetention in
// store/shell.go), or add a tier argument to MatchingEntries itself so
// the filter happens at shard-selection time.
//
// Until then, this test documents the gap.
func TestMatchingEntries_ClampsToPlanRetention(t *testing.T) {
	registry := NewShardRegistry()

	now := time.Now()
	recent := &ShardEntry{
		ID:          "recent",
		Apps:        []string{"ops"},
		WindowStart: now.Add(-10 * 24 * time.Hour),
		WindowEnd:   now.Add(-5 * 24 * time.Hour),
		State:       StateReadOnly,
		Shard:       &failingShard{id: "recent"},
		RowCount:    1,
	}
	spanning := &ShardEntry{
		ID:          "spanning",
		Apps:        []string{"ops"},
		WindowStart: now.Add(-45 * 24 * time.Hour),
		WindowEnd:   now.Add(-20 * 24 * time.Hour),
		State:       StateReadOnly,
		Shard:       &failingShard{id: "spanning"},
		RowCount:    1,
	}
	old := &ShardEntry{
		ID:          "old",
		Apps:        []string{"ops"},
		WindowStart: now.Add(-200 * 24 * time.Hour),
		WindowEnd:   now.Add(-195 * 24 * time.Hour),
		State:       StateReadOnly,
		Shard:       &failingShard{id: "old"},
		RowCount:    1,
	}
	registry.Add(recent)
	registry.Add(spanning)
	registry.Add(old)

	// Mirror the coordinator.query call site: pass the caller's raw
	// TimeRange (one year back) into MatchingEntries. When tier-aware
	// clamping lands, the old shard must drop out.
	tier := store.TierSwatch // 30-day retention
	tr := TimeRange{
		Start: now.Add(-365 * 24 * time.Hour),
		End:   now,
	}

	matched := registry.MatchingEntries(tr, servingFor(registry, "ops"), tier)

	ids := make(map[string]bool, len(matched))
	for _, e := range matched {
		ids[e.ID] = true
	}

	if ids["old"] {
		t.Errorf(
			"swatch tenant (retention %s) reached a shard whose window "+
				"ends %v ago — outside their plan. "+
				"Matched IDs: %v. Expected {recent, spanning}.",
			store.LogRetention(tier),
			now.Sub(old.WindowEnd).Round(24*time.Hour),
			keys(ids),
		)
	}
	if !ids["recent"] {
		t.Errorf("expected the in-retention shard to match; got %v", keys(ids))
	}
	if !ids["spanning"] {
		t.Errorf(
			"expected the boundary-spanning shard to match — rows from "+
				"the tail of its window (−20d) are still inside the %s "+
				"retention cap; got %v",
			store.LogRetention(tier), keys(ids),
		)
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
