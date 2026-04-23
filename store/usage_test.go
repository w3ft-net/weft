package store

import (
	"testing"
	"time"
)

// TestRollupUsageDaily_AggregatesHourlyCorrectly confirms the stored
// procedure sums usage_hourly into usage_daily, groups by day/customer,
// and is idempotent (a second CALL doesn't double-count).
func TestRollupUsageDaily_AggregatesHourlyCorrectly(t *testing.T) {
	db := testOpen(t)

	cust := "usage-rollup-test"
	// Two customers' rows to prove the GROUP BY scopes correctly.
	other := "usage-rollup-other"

	// Yesterday, 05:00..09:00 UTC — five hourly rows of 100 bytes each
	// for our customer, plus a single row for the other customer so
	// we can confirm the rollup scopes per-customer.
	yesterday := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	for h := 5; h < 10; h++ {
		hour := yesterday.Add(time.Duration(h) * time.Hour)
		if err := db.UpsertUsageHourly(cust, hour, 100); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.UpsertUsageHourly(other, yesterday.Add(5*time.Hour), 42); err != nil {
		t.Fatal(err)
	}

	if err := db.db.Exec("CALL public.rollup_usage_daily()").Error; err != nil {
		t.Fatalf("first rollup: %v", err)
	}

	want := func(t *testing.T, customer string, wantBytes int64) {
		t.Helper()
		var got int64
		if err := db.db.Raw(`
			SELECT bytes_ingested FROM usage_daily
			WHERE customer_name = ? AND day = ?::date`,
			customer, yesterday,
		).Scan(&got).Error; err != nil {
			t.Fatal(err)
		}
		if got != wantBytes {
			t.Errorf("%s: usage_daily = %d, want %d", customer, got, wantBytes)
		}
	}
	want(t, cust, 500)
	want(t, other, 42)

	// Idempotency: a second call must not double the totals. This is
	// the whole point of the new model — the old DELETE-after-INSERT
	// couldn't survive overlap; the new ON CONFLICT DO UPDATE
	// overwrites with the same sum.
	if err := db.db.Exec("CALL public.rollup_usage_daily()").Error; err != nil {
		t.Fatalf("second rollup: %v", err)
	}
	want(t, cust, 500)
	want(t, other, 42)

	// Now append one more hourly row for our customer (a late-arriving
	// ingest within the look-back window) and confirm the next rollup
	// folds it in — daily total goes 500 → 600.
	if err := db.UpsertUsageHourly(cust, yesterday.Add(15*time.Hour), 100); err != nil {
		t.Fatal(err)
	}
	if err := db.db.Exec("CALL public.rollup_usage_daily()").Error; err != nil {
		t.Fatalf("third rollup: %v", err)
	}
	want(t, cust, 600)
}

// TestQueryUsage_SplitsOnTodayBoundary is the read-side guarantee:
// with the new rollup model, usage_hourly and usage_daily overlap
// for up to 7 days. QueryUsage must NOT double-count that overlap.
// It reads daily for completed days and hourly for today only.
func TestQueryUsage_SplitsOnTodayBoundary(t *testing.T) {
	db := testOpen(t)

	cust := "usage-query-split"
	todayStart := time.Now().UTC().Truncate(24 * time.Hour)
	yesterday := todayStart.Add(-24 * time.Hour)

	// Yesterday — exists in BOTH hourly and daily (the idempotent
	// rollup keeps hourly rows around until partman drops the
	// partition). QueryUsage must read yesterday's value from daily
	// only.
	if err := db.UpsertUsageHourly(cust, yesterday.Add(10*time.Hour), 1000); err != nil {
		t.Fatal(err)
	}
	if err := db.db.Exec("CALL public.rollup_usage_daily()").Error; err != nil {
		t.Fatal(err)
	}

	// Today — only exists in hourly. QueryUsage must find it there
	// because the day isn't "complete" yet.
	if err := db.UpsertUsageHourly(cust, todayStart.Add(2*time.Hour), 42); err != nil {
		t.Fatal(err)
	}

	// Total across both days: yesterday (1000, via daily) + today
	// (42, via hourly) = 1042. If we were still reading both tables
	// UNION-style for yesterday, it'd come back 2042.
	total, err := db.QueryUsage(cust, yesterday, todayStart.Add(25*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if total != 1042 {
		t.Errorf("split query = %d, want 1042 (double-count would show 2042)", total)
	}

	// Range ending before today: daily-only path.
	total, err = db.QueryUsage(cust, yesterday, todayStart)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1000 {
		t.Errorf("yesterday-only = %d, want 1000", total)
	}

	// Range starting at today: hourly-only path.
	total, err = db.QueryUsage(cust, todayStart, todayStart.Add(25*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if total != 42 {
		t.Errorf("today-only = %d, want 42", total)
	}
}
