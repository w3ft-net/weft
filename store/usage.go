package store

import "time"

// UpsertUsageHourly inserts or increments an hourly usage record.
func (s *Store) UpsertUsageHourly(customerName string, hour time.Time, bytes int64) error {
	return s.db.Exec(`
		INSERT INTO usage_hourly (customer_name, hour, bytes_ingested)
		VALUES (?, ?, ?)
		ON CONFLICT (customer_name, hour) DO UPDATE
		SET bytes_ingested = usage_hourly.bytes_ingested + EXCLUDED.bytes_ingested`,
		customerName, hour, bytes).Error
}

// QueryUsage returns total bytes ingested for a customer in a time range.
//
// Split-on-today semantics: usage_daily carries complete totals for every
// day with at least one rollup tick behind it (migration 028 runs the
// rollup hourly); usage_hourly carries the live, partial total for the
// current day. Reading daily for [start..today) and hourly for
// [today..end) avoids the overlap that would otherwise double-count
// days where the rollup has run but pg_partman hasn't yet dropped the
// hourly partition.
//
// Worst-case staleness is the rollup cadence: a completed day D can
// lag reality by up to one hour (the interval between rollup ticks).
// That's fine for the dashboard and the daily Stripe report; see
// docs/plans/stripe.md for the reporter's own timing.
func (s *Store) QueryUsage(customerName string, periodStart, periodEnd time.Time) (int64, error) {
	var result struct{ Total int64 }
	err := s.db.Raw(`
		SELECT COALESCE(SUM(b), 0) AS total FROM (
			-- Complete days from the rolled-up table. Upper bound is the
			-- earlier of (periodEnd, start-of-today), so today's partial
			-- daily row (if any) is ignored here and picked up below.
			SELECT bytes_ingested AS b FROM usage_daily
			WHERE customer_name = ?
			  AND day >= ?::date
			  AND day <  LEAST(?::date, (date_trunc('day', now()))::date)
			UNION ALL
			-- Today's live partial. Lower bound is the later of
			-- (periodStart, start-of-today); upper is periodEnd. If the
			-- caller's window ends before today, this half returns zero
			-- rows.
			SELECT bytes_ingested AS b FROM usage_hourly
			WHERE customer_name = ?
			  AND hour >= GREATEST(?::timestamptz, date_trunc('day', now()))
			  AND hour <  ?
		) combined`,
		customerName, periodStart, periodEnd,
		customerName, periodStart, periodEnd).Scan(&result).Error
	return result.Total, err
}
