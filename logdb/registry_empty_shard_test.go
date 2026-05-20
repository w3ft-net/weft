package logdb

import (
	"testing"
	"time"

	"github.com/kevlarcon/weft/store"
)

// TestMatchingEntries_SkipsEmptyColdShards pins the fix for pointless
// fan-out to empty archived/read-only shards. shard-20260506T0900 had
// row_count 0 yet was still dialed on every matching query — a wasted
// cold-Lambda invocation (which, post-auth-fix, OOM-crash-looped
// compute). An archived or read-only shard with zero rows has nothing
// to return and must not be dialed at all. Live (INGESTING) shards with
// a transient 0 count are NOT filtered — they may be mid-fill.
func TestMatchingEntries_SkipsEmptyColdShards(t *testing.T) {
	now := time.Now()
	reg := NewShardRegistry()

	mk := func(id string, st ShardState, rows int64) *ShardEntry {
		return &ShardEntry{
			ID:          id,
			Apps:        nil,
			WindowStart: now.Add(-30 * time.Minute),
			WindowEnd:   now.Add(-20 * time.Minute),
			State:       st,
			Addr:        "10.0.0.1:9000", // non-empty so the addr/Shard guard passes
			RowCount:    rows,
		}
	}
	reg.Add(mk("archived-empty", StateArchived, 0))   // must be skipped
	reg.Add(mk("archived-full", StateArchived, 340))  // must match
	reg.Add(mk("readonly-empty", StateReadOnly, 0))   // must be skipped
	reg.Add(mk("ingesting-empty", StateIngesting, 0)) // must match (transient)

	tr := TimeRange{Start: now.Add(-time.Hour), End: now}
	got := map[string]bool{}
	for _, e := range reg.MatchingEntries(tr, "", store.Tier("")) {
		got[e.ID] = true
	}

	if got["archived-empty"] {
		t.Error("empty ARCHIVED shard was returned — it has 0 rows and only causes a wasted cold-Lambda dial")
	}
	if got["readonly-empty"] {
		t.Error("empty READ_ONLY shard was returned — 0 rows, nothing to query")
	}
	if !got["archived-full"] {
		t.Error("non-empty ARCHIVED shard (340 rows) was wrongly excluded — real data must still be queryable")
	}
	if !got["ingesting-empty"] {
		t.Error("INGESTING shard with transient 0 rows was excluded — live shards must not be filtered on row count")
	}
}
