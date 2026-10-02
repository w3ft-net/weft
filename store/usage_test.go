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

	cust := mustOrg(t, db, "usage-rollup-test")
	// Two customers' rows to prove the GROUP BY scopes correctly.
	other := mustOrg(t, db, "usage-rollup-other")

	// Yesterday, 05:00..09:00 UTC — five hourly rows of 100 bytes each
	// for our customer, plus a single row for the other customer so
	// we can confirm the rollup scopes per-customer.
	yesterday := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	for h := 5; h < 10; h++ {
		hour := yesterday.Add(time.Duration(h) * time.Hour)
		if err := upsertUsageHourly(db, cust, hour, 100); err != nil {
			t.Fatal(err)
		}
	}
	if err := upsertUsageHourly(db, other, yesterday.Add(5*time.Hour), 42); err != nil {
		t.Fatal(err)
	}

	if err := db.db.Exec("CALL public.rollup_usage_daily()").Error; err != nil {
		t.Fatalf("first rollup: %v", err)
	}

	want := func(t *testing.T, customer OrgID, wantBytes int64) {
		t.Helper()
		var got int64
		if err := db.db.Raw(`
			SELECT bytes_ingested FROM usage_daily
			WHERE org_id = ? AND day = ?::date`,
			customer, yesterday,
		).Scan(&got).Error; err != nil {
			t.Fatal(err)
		}
		if got != wantBytes {
			t.Errorf("org %d: usage_daily = %d, want %d", customer, got, wantBytes)
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
	if err := upsertUsageHourly(db, cust, yesterday.Add(15*time.Hour), 100); err != nil {
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

	cust := mustOrg(t, db, "usage-query-split")
	todayStart := time.Now().UTC().Truncate(24 * time.Hour)
	yesterday := todayStart.Add(-24 * time.Hour)

	// Yesterday — exists in BOTH hourly and daily (the idempotent
	// rollup keeps hourly rows around until partman drops the
	// partition). QueryUsage must read yesterday's value from daily
	// only.
	if err := upsertUsageHourly(db, cust, yesterday.Add(10*time.Hour), 1000); err != nil {
		t.Fatal(err)
	}
	if err := db.db.Exec("CALL public.rollup_usage_daily()").Error; err != nil {
		t.Fatal(err)
	}

	// Today — only exists in hourly. QueryUsage must find it there
	// because the day isn't "complete" yet.
	if err := upsertUsageHourly(db, cust, todayStart.Add(2*time.Hour), 42); err != nil {
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

// upsertUsageHourly mirrors production's append-only flush (coordinator.go
// flushUsage, migration 074): usage_hourly has no uniqueness constraint on
// (customer_name, hour), so every call inserts its own row rather than
// accumulating into one. Readers SUM across rows, so repeated calls for
// the same customer/hour behave identically to the old upsert-based
// helper as far as every assertion in this file is concerned. Name kept
// for now since callers still think of it as "record this hour's usage".
func upsertUsageHourly(db *Store, orgID OrgID, hour time.Time, bytes int64) error {
	return db.db.Exec(`
		INSERT INTO usage_hourly (org_id, hour, bytes_ingested)
		VALUES (?, ?, ?)`,
		orgID, hour, bytes).Error
}

// mustOrg returns the id of the (find-or-created) org with this name.
func mustOrg(t *testing.T, db *Store, name string) OrgID {
	t.Helper()
	o, err := db.FindOrCreateOrg(name)
	if err != nil {
		t.Fatalf("FindOrCreateOrg(%q): %v", name, err)
	}
	return o.ID
}
