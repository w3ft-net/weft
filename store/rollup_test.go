package store

import (
	"math"
	"testing"
	"time"
)

// TestRollupHourly_AggregatesRawCorrectly confirms rollup_cron_hourly
// produces the right count/sum/sum_sq/min/max for a known raw set,
// groups correctly by tag, and is idempotent (re-running doesn't
// double-count).
func TestRollupHourly_AggregatesRawCorrectly(t *testing.T) {
	db := testOpen(t)

	cust, err := db.FindOrCreateCustomer("rollup-test")
	if err != nil {
		t.Fatal(err)
	}

	if err := db.SaveCronWatch(cust.ID, "", "rollup-w", "*/1 * * * *", "echo 1"); err != nil {
		t.Fatal(err)
	}
	var watchID uint
	if err := db.db.Raw("SELECT id FROM cron_watches WHERE customer_id = ? AND name = 'rollup-w'", cust.ID).Scan(&watchID).Error; err != nil {
		t.Fatal(err)
	}

	// Target hour: two hours ago (closed from the rollup's perspective).
	hour := time.Now().Truncate(time.Hour).Add(-2 * time.Hour)

	// Empty tag: ten rows with values 10, 20, … 100.
	for i := 1; i <= 10; i++ {
		ranAt := hour.Add(time.Duration(i) * 5 * time.Minute)
		v := float64(i * 10)
		if err := db.db.Exec(
			"INSERT INTO cron_results (cron_watch_id, ran_at, value, tag) VALUES (?, ?, ?, '')",
			watchID, ranAt, v,
		).Error; err != nil {
			t.Fatal(err)
		}
	}
	// Tag "shard-a": five rows with values 100, 200, … 500.
	for i := 1; i <= 5; i++ {
		ranAt := hour.Add(time.Duration(i) * 5 * time.Minute)
		v := float64(i * 100)
		if err := db.db.Exec(
			"INSERT INTO cron_results (cron_watch_id, ran_at, value, tag) VALUES (?, ?, ?, 'shard-a')",
			watchID, ranAt, v,
		).Error; err != nil {
			t.Fatal(err)
		}
	}

	if err := db.db.Exec("CALL public.rollup_cron_hourly()").Error; err != nil {
		t.Fatalf("rollup_cron_hourly: %v", err)
	}

	type row struct {
		Tag    string
		Count  int64
		Sum    float64
		SumSq  float64 `gorm:"column:sum_sq"`
		Min    float64
		Max    float64
	}
	var rows []row
	if err := db.db.Raw(`
		SELECT tag, count, sum, sum_sq, min, max
		FROM cron_results_hourly
		WHERE cron_watch_id = ? AND bucket_start = ?
		ORDER BY tag`, watchID, hour,
	).Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}

	if len(rows) != 2 {
		t.Fatalf("expected 2 tag buckets, got %d: %+v", len(rows), rows)
	}

	// Empty tag: sum 550, sum_sq 38500, min 10, max 100.
	empty := rows[0]
	if empty.Tag != "" {
		t.Errorf("expected empty tag first, got %q", empty.Tag)
	}
	checkAgg(t, "empty-tag", empty.Count, empty.Sum, empty.SumSq, empty.Min, empty.Max,
		10, 550, 38500, 10, 100)

	// shard-a: 1500, 550000, 100, 500.
	shardA := rows[1]
	if shardA.Tag != "shard-a" {
		t.Errorf("expected shard-a second, got %q", shardA.Tag)
	}
	checkAgg(t, "shard-a", shardA.Count, shardA.Sum, shardA.SumSq, shardA.Min, shardA.Max,
		5, 1500, 550000, 100, 500)

	// Second invocation should be idempotent — still 10 samples, not 20.
	if err := db.db.Exec("CALL public.rollup_cron_hourly()").Error; err != nil {
		t.Fatalf("second rollup call: %v", err)
	}
	rows = nil
	if err := db.db.Raw(`
		SELECT tag, count, sum, sum_sq, min, max
		FROM cron_results_hourly
		WHERE cron_watch_id = ? AND bucket_start = ? AND tag = ''`,
		watchID, hour,
	).Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Count != 10 {
		t.Errorf("rollup not idempotent: %+v", rows)
	}
}

// TestRollup_4hCombinesHourly validates the hourly → 4h step produces
// correct aggregates when multiple hourly buckets fall into one 4h
// bucket. This is the first "aggregate-from-aggregate" hop and the
// most error-prone (need to SUM(count)+SUM(sum_sq), not COUNT(*)).
func TestRollup_4hCombinesHourly(t *testing.T) {
	db := testOpen(t)

	cust, err := db.FindOrCreateCustomer("rollup-4h-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveCronWatch(cust.ID, "", "w", "*/1 * * * *", "echo 1"); err != nil {
		t.Fatal(err)
	}
	var watchID uint
	if err := db.db.Raw("SELECT id FROM cron_watches WHERE customer_id = ? AND name = 'w'", cust.ID).Scan(&watchID).Error; err != nil {
		t.Fatal(err)
	}

	// Target 4h bucket: 8 hours ago, aligned to 4h boundary. Populate
	// all four of its hourly children.
	now := time.Now().Truncate(time.Hour)
	bucket4h := now.Add(-8 * time.Hour)
	bucket4h = bucket4h.Add(-time.Duration(bucket4h.Hour()%4) * time.Hour)

	for h := 0; h < 4; h++ {
		hourStart := bucket4h.Add(time.Duration(h) * time.Hour)
		if err := db.db.Exec(`
			INSERT INTO cron_results_hourly
				(cron_watch_id, tag, bucket_start, count, sum, sum_sq, min, max)
			VALUES (?, '', ?, 10, 100, 1000, ?, ?)`,
			watchID, hourStart, float64(h), float64(h+10),
		).Error; err != nil {
			t.Fatal(err)
		}
	}

	if err := db.db.Exec("CALL public.rollup_cron_4h()").Error; err != nil {
		t.Fatalf("rollup_cron_4h: %v", err)
	}

	var got struct {
		Count int64
		Sum   float64
		SumSq float64 `gorm:"column:sum_sq"`
		Min   float64
		Max   float64
	}
	if err := db.db.Raw(`
		SELECT count, sum, sum_sq, min, max
		FROM cron_results_4h
		WHERE cron_watch_id = ? AND bucket_start = ?`,
		watchID, bucket4h,
	).Scan(&got).Error; err != nil {
		t.Fatal(err)
	}
	// Expect count=40, sum=400, sum_sq=4000, min=0 (of 0,1,2,3), max=13 (of 10,11,12,13).
	checkAgg(t, "4h", got.Count, got.Sum, got.SumSq, got.Min, got.Max,
		40, 400, 4000, 0, 13)
}

func checkAgg(t *testing.T, label string, count int64, sum, sumSq, min, max float64,
	wantCount int64, wantSum, wantSumSq, wantMin, wantMax float64,
) {
	t.Helper()
	if count != wantCount {
		t.Errorf("%s: count = %d, want %d", label, count, wantCount)
	}
	if !nearly(sum, wantSum) {
		t.Errorf("%s: sum = %v, want %v", label, sum, wantSum)
	}
	if !nearly(sumSq, wantSumSq) {
		t.Errorf("%s: sum_sq = %v, want %v", label, sumSq, wantSumSq)
	}
	if !nearly(min, wantMin) {
		t.Errorf("%s: min = %v, want %v", label, min, wantMin)
	}
	if !nearly(max, wantMax) {
		t.Errorf("%s: max = %v, want %v", label, max, wantMax)
	}
}

func nearly(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

// TestCronAggregate_PicksTierByWindow verifies the router reads raw
// for short windows and hourly for longer ones. We seed distinctive
// values in both tiers for the same watch so you can tell which
// table the result came from.
func TestCronAggregate_PicksTierByWindow(t *testing.T) {
	db := testOpen(t)

	cust, err := db.FindOrCreateCustomer("tier-pick-test")
	if err != nil {
		t.Fatal(err)
	}
	// Give them a bolt subscription so LogRetention doesn't clamp.
	if err := db.db.Exec(`
		INSERT INTO subscriptions (id, customer_id, tier, status, starts_at, created_at, updated_at)
		VALUES (?, ?, 'bolt', 'active', now(), now(), now())`, "sub-"+cust.Name, cust.Name).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.SaveCronWatch(cust.ID, "", "w", "*/1 * * * *", "echo 1"); err != nil {
		t.Fatal(err)
	}
	var watchID uint
	if err := db.db.Raw("SELECT id FROM cron_watches WHERE customer_id = ? AND name = 'w'", cust.ID).Scan(&watchID).Error; err != nil {
		t.Fatal(err)
	}

	// Raw: two rows yesterday with values 7 and 7 — marker avg 7.
	for i := 0; i < 2; i++ {
		if err := db.db.Exec(
			"INSERT INTO cron_results (cron_watch_id, ran_at, value, tag) VALUES (?, ?, 7, '')",
			watchID, time.Now().Add(-24*time.Hour+time.Duration(i)*time.Minute),
		).Error; err != nil {
			t.Fatal(err)
		}
	}

	// Hourly: one bucket 10 days ago with sum=77, count=11 → avg 7.
	// Distinct marker is that NO raw row exists here (past raw 7d).
	if err := db.db.Exec(`
		INSERT INTO cron_results_hourly (cron_watch_id, tag, bucket_start, count, sum, sum_sq, min, max)
		VALUES (?, '', ?, 11, 77, 539, 5, 9)`,
		watchID, time.Now().Add(-10*24*time.Hour).Truncate(time.Hour),
	).Error; err != nil {
		t.Fatal(err)
	}

	// Short window (1d) → raw tier.
	short, err := db.CronAggregate(cust.ID, watchID, time.Now().Add(-2*24*time.Hour), "")
	if err != nil {
		t.Fatalf("CronAggregate short: %v", err)
	}
	if short.Samples != 2 {
		t.Errorf("short window: expected 2 samples from raw, got %d", short.Samples)
	}
	if short.Avg == nil || !nearly(*short.Avg, 7) {
		t.Errorf("short window: avg = %v, want 7", short.Avg)
	}

	// 12-day window → hourly tier. Raw rows from yesterday are NOT in
	// hourly (they're still raw), so the only data is the hourly
	// bucket we seeded — samples = 11 (count field), avg = 7.
	long, err := db.CronAggregate(cust.ID, watchID, time.Now().Add(-12*24*time.Hour), "")
	if err != nil {
		t.Fatalf("CronAggregate long: %v", err)
	}
	if long.Samples != 11 {
		t.Errorf("long window: expected 11 samples from hourly tier, got %d", long.Samples)
	}
	if long.Avg == nil || !nearly(*long.Avg, 7) {
		t.Errorf("long window: avg = %v, want 7", long.Avg)
	}
}

// TestQueryCronSeries_PicksTierAndReturnsBuckets confirms the series
// endpoint routes to the right tier by window width and returns the
// buckets in ascending time order with (count, min, max) intact.
// Seeds distinctive values in raw and hourly for the same watch so
// you can tell which tier the result came from.
func TestQueryCronSeries_PicksTierAndReturnsBuckets(t *testing.T) {
	db := testOpen(t)

	cust, err := db.FindOrCreateCustomer("series-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.db.Exec(`
		INSERT INTO subscriptions (id, customer_id, tier, status, starts_at, created_at, updated_at)
		VALUES (?, ?, 'bolt', 'active', now(), now(), now())`,
		"sub-"+cust.Name, cust.Name).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.SaveCronWatch(cust.ID, "", "w", "*/1 * * * *", "echo 1"); err != nil {
		t.Fatal(err)
	}
	var watchID uint
	if err := db.db.Raw("SELECT id FROM cron_watches WHERE customer_id = ? AND name = 'w'", cust.ID).Scan(&watchID).Error; err != nil {
		t.Fatal(err)
	}

	// Two raw rows yesterday: values 5 and 15 in the empty tag.
	for i, v := range []float64{5, 15} {
		ranAt := time.Now().Add(-24*time.Hour + time.Duration(i)*time.Minute)
		if err := db.db.Exec(
			"INSERT INTO cron_results (cron_watch_id, ran_at, value, tag) VALUES (?, ?, ?, '')",
			watchID, ranAt, v,
		).Error; err != nil {
			t.Fatal(err)
		}
	}

	// One hourly bucket 10 days ago: marker value only accessible via hourly tier.
	if err := db.db.Exec(`
		INSERT INTO cron_results_hourly (cron_watch_id, tag, bucket_start, count, sum, sum_sq, min, max)
		VALUES (?, '', ?, 11, 110, 1210, 1, 20)`,
		watchID, time.Now().Add(-10*24*time.Hour).Truncate(time.Hour),
	).Error; err != nil {
		t.Fatal(err)
	}

	// Short window -> raw tier, two buckets with count=1 each.
	buckets, gran, err := db.QueryCronSeries(cust.ID, watchID, time.Now().Add(-2*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if gran != "raw" {
		t.Errorf("short window: granularity = %q, want raw", gran)
	}
	if len(buckets) != 2 {
		t.Fatalf("short window: expected 2 raw buckets, got %d", len(buckets))
	}
	if buckets[0].Count != 1 || buckets[0].Sum != 5 {
		t.Errorf("short window: first bucket = %+v, want count=1 sum=5", buckets[0])
	}

	// 12-day window -> hourly tier, hits the seeded rollup row.
	buckets, gran, err = db.QueryCronSeries(cust.ID, watchID, time.Now().Add(-12*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if gran != "hourly" {
		t.Errorf("long window: granularity = %q, want hourly", gran)
	}
	if len(buckets) != 1 {
		t.Fatalf("long window: expected 1 hourly bucket, got %d", len(buckets))
	}
	b := buckets[0]
	if b.Count != 11 || b.Sum != 110 || b.Min != 1 || b.Max != 20 {
		t.Errorf("long window: bucket = %+v, want count=11 sum=110 min=1 max=20", b)
	}
}

// TestCronAggregate_ClampsToPlanRetention confirms a swatch customer
// (30d cap) asking for 60d of history sees only the last 30 days'
// data, not the older rows — even though those older rows are still
// in Postgres. This is the query-side "illusion" of enforcement from
// docs/plans/retention-enforcement.md.
func TestCronAggregate_ClampsToPlanRetention(t *testing.T) {
	db := testOpen(t)

	cust, err := db.FindOrCreateCustomer("clamp-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.db.Exec(`
		INSERT INTO subscriptions (id, customer_id, tier, status, starts_at, created_at, updated_at)
		VALUES (?, ?, 'swatch', 'active', now(), now(), now())`, "sub-"+cust.Name, cust.Name).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.SaveCronWatch(cust.ID, "", "w", "*/1 * * * *", "echo 1"); err != nil {
		t.Fatal(err)
	}
	var watchID uint
	if err := db.db.Raw("SELECT id FROM cron_watches WHERE customer_id = ? AND name = 'w'", cust.ID).Scan(&watchID).Error; err != nil {
		t.Fatal(err)
	}

	// Seed two 4h rollup rows: one at 20d ago (should be returned),
	// one at 45d ago (should be hidden by the 30d clamp).
	for _, age := range []time.Duration{20 * 24 * time.Hour, 45 * 24 * time.Hour} {
		bucket := time.Now().Add(-age).Truncate(4 * time.Hour)
		if err := db.db.Exec(`
			INSERT INTO cron_results_4h (cron_watch_id, tag, bucket_start, count, sum, sum_sq, min, max)
			VALUES (?, '', ?, 10, 500, 25500, 40, 60)`, watchID, bucket,
		).Error; err != nil {
			t.Fatal(err)
		}
	}

	// Request a 60d window — should clamp to 30d, returning only the
	// 20d-old row (10 samples). The 45d row is hidden by retention.
	stats, err := db.CronAggregate(cust.ID, watchID, time.Now().Add(-60*24*time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	if stats.Samples != 10 {
		t.Errorf("swatch 60d → expected 10 samples (30d clamp), got %d", stats.Samples)
	}
}
