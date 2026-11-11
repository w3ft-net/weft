package retention

import (
	"testing"
	"time"

	"github.com/graybeardcorp/weft/logdb"
	"github.com/graybeardcorp/weft/shard"
	"github.com/graybeardcorp/weft/store"
)

// now is a fixed reference instant for the classification tests so the
// cutoff math is deterministic.
var janitorNow = time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)

const testGrace = 7 * 24 * time.Hour // matches RetentionGrace

// Stand-in org ids for the classification tests: acme is mapped to a tier,
// ghost is deliberately absent from the resolver.
const (
	acme  store.OrgID = 5
	ghost store.OrgID = 14
)

func days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

// stat builds a ShardArchiveStat spanning [now-maxAgeDays, now-minAgeDays]
// for a customer (older bound first). row_count is irrelevant to
// classification, so it's fixed.
func stat(customer store.OrgID, minAgeDays, maxAgeDays int) ShardArchiveStat {
	return ShardArchiveStat{
		OrgID:    customer,
		MaxTs:    janitorNow.Add(-days(minAgeDays)), // youngest row
		MinTs:    janitorNow.Add(-days(maxAgeDays)), // oldest row
		RowCount: 1,
	}
}

func TestClassifyArchivedShard(t *testing.T) {
	// swatch = 30d retention; +7d grace → cutoff at 37d. acme is mapped
	// swatch; "ghost" is intentionally absent (no active subscription).
	resolver := NewRetentionResolver(map[store.OrgID]store.Tier{acme: store.TierSwatch})

	cases := []struct {
		name  string
		stats []ShardArchiveStat
		want  ShardDisposition
	}{
		{"no stats → unknown", nil, DispositionUnknown},
		{"all past cutoff → delete", []ShardArchiveStat{stat(acme, 40, 50)}, DispositionDelete},
		{"all within retention → keep", []ShardArchiveStat{stat(acme, 1, 10)}, DispositionKeep},
		{"straddles cutoff → rewrite", []ShardArchiveStat{stat(acme, 10, 50)}, DispositionRewrite},
		{
			"expired mapped + unmapped present → rewrite (preserve unmapped)",
			[]ShardArchiveStat{stat(acme, 40, 50), stat(ghost, 40, 50)},
			DispositionRewrite,
		},
		{
			"only unmapped → keep (never delete an unmapped customer)",
			[]ShardArchiveStat{stat(ghost, 40, 50)},
			DispositionKeep,
		},
		{
			"one customer expired, one live → rewrite",
			[]ShardArchiveStat{stat(acme, 40, 50), stat(acme, 1, 5)},
			DispositionRewrite,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyArchivedShard(tc.stats, resolver, janitorNow, testGrace)
			if got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// TestClassifyArchivedShard_GraceBoundary pins the inclusive/exclusive
// edge: a span whose youngest row sits exactly at the cutoff is NOT
// expired (kept); one nanosecond older flips it.
func TestClassifyArchivedShard_GraceBoundary(t *testing.T) {
	resolver := NewRetentionResolver(map[store.OrgID]store.Tier{acme: store.TierSwatch})
	cutoffAge := store.LogRetention(store.TierSwatch) + testGrace // 37d

	atCutoff := ShardArchiveStat{
		OrgID: 5,
		MinTs: janitorNow.Add(-cutoffAge),
		MaxTs: janitorNow.Add(-cutoffAge),
	}
	if got := ClassifyArchivedShard([]ShardArchiveStat{atCutoff}, resolver, janitorNow, testGrace); got != DispositionKeep {
		t.Fatalf("row exactly at cutoff: got %s, want keep", got)
	}

	justOlder := atCutoff
	justOlder.MinTs = janitorNow.Add(-cutoffAge - time.Nanosecond)
	justOlder.MaxTs = justOlder.MinTs
	if got := ClassifyArchivedShard([]ShardArchiveStat{justOlder}, resolver, janitorNow, testGrace); got != DispositionDelete {
		t.Fatalf("row just past cutoff: got %s, want delete", got)
	}
}

func TestRetentionResolver(t *testing.T) {
	r := NewRetentionResolver(map[store.OrgID]store.Tier{acme: store.TierBolt})
	if d, ok := r.Retention(acme); !ok || d != store.LogRetention(store.TierBolt) {
		t.Fatalf("acme: got (%v, %v), want (%v, true)", d, ok, store.LogRetention(store.TierBolt))
	}
	if _, ok := r.Retention(ghost); ok {
		t.Fatal("unmapped customer should resolve ok=false")
	}
}

func TestRetentionResolver_Cutoffs(t *testing.T) {
	r := NewRetentionResolver(map[store.OrgID]store.Tier{acme: store.TierSwatch})
	stats := []ShardArchiveStat{stat(acme, 1, 10), stat(ghost, 1, 10)}

	cutoffs := r.Cutoffs(stats, janitorNow, testGrace)
	if len(cutoffs) != 1 {
		t.Fatalf("got %d cutoffs, want 1 (ghost is unmapped → omitted): %+v", len(cutoffs), cutoffs)
	}
	if cutoffs[0].CustomerID != int64(acme) {
		t.Fatalf("cutoff customer = %d, want acme", cutoffs[0].CustomerID)
	}
	want := janitorNow.Add(-store.LogRetention(store.TierSwatch) - testGrace)
	if !cutoffs[0].Cutoff.Equal(want) {
		t.Fatalf("cutoff = %v, want %v", cutoffs[0].Cutoff, want)
	}
}

// TestPlanRetention_EndToEnd seeds real customers, subscriptions, archived
// shards, and archive stats, then asserts PlanRetention classifies each
// shard correctly against live subscription state — exercising
// ActiveCustomerTiers + RecordArchiveStats + the planner together.
func TestPlanRetention_EndToEnd(t *testing.T) {
	db := openTestStore(t)
	bucket := seedBucket(t, db)
	ss := newStore(db)

	// A paying swatch customer (30d) and a customer with NO subscription.
	paying := seedPayingCo(t, db)
	if err := db.CreateSubscription(&store.Subscription{
		ID: "sub-paying", OrgID: paying.ID, Tier: "swatch",
		Status: "active", StartsAt: janitorNow.Add(-days(60)), CreatedAt: janitorNow.Add(-days(60)),
	}); err != nil {
		t.Fatalf("subscription: %v", err)
	}
	store.TestOrg(t, db, "unmapped-co")

	// Three archived shards: paying-co fully expired, paying-co live, and
	// an unmapped-co shard whose rows are ancient.
	seedArchived := func(id string, stats []shard.CustomerStat) {
		t.Helper()
		if err := ss.SaveShard(&logdb.Shard{
			ID: id, State: logdb.StateArchived, BucketID: bucket.ID, Tier: "small",
			ParquetPath: "s3://weft/x/" + id + ".parquet",
			WindowStart: janitorNow.Add(-days(60)), WindowEnd: janitorNow.Add(-days(53)),
			CreatedAt: janitorNow.Add(-days(60)),
		}); err != nil {
			t.Fatalf("seed shard %s: %v", id, err)
		}
		if err := ss.RecordArchiveStats(id, stats); err != nil {
			t.Fatalf("record stats %s: %v", id, err)
		}
	}
	expired := []shard.CustomerStat{{CustomerID: int64(payingCo), MinTs: janitorNow.Add(-days(60)), MaxTs: janitorNow.Add(-days(50)), RowCount: 9}}
	live := []shard.CustomerStat{{CustomerID: int64(payingCo), MinTs: janitorNow.Add(-days(5)), MaxTs: janitorNow.Add(-days(1)), RowCount: 9}}
	unmapped := []shard.CustomerStat{{CustomerID: 13, MinTs: janitorNow.Add(-days(60)), MaxTs: janitorNow.Add(-days(50)), RowCount: 9}}
	seedArchived("arch-expired", expired)
	seedArchived("arch-live", live)
	seedArchived("arch-unmapped", unmapped)

	plan, err := ss.PlanRetention(janitorNow, testGrace)
	if err != nil {
		t.Fatalf("PlanRetention: %v", err)
	}

	got := make(map[string]ShardDisposition, len(plan))
	for _, p := range plan {
		got[p.ShardID] = p.Disposition
	}
	if d, ok := got["arch-expired"]; !ok || d != DispositionDelete {
		t.Errorf("arch-expired: got (%s, present=%v), want delete", d, ok)
	}
	if _, ok := got["arch-live"]; ok {
		t.Errorf("arch-live should be Keep (omitted from plan), got %s", got["arch-live"])
	}
	if _, ok := got["arch-unmapped"]; ok {
		t.Errorf("arch-unmapped should be Keep (unmapped preserved), got %s", got["arch-unmapped"])
	}
}
