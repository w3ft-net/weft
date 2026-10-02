package store

import (
	"time"
)

// StripeUsageReport is the audit log for one day of ingestion usage
// posted to Stripe's billing/meter_events API. Written only on
// successful API response — absence signals "needs report" on the
// next reporter tick. UNIQUE(org_id, day) enforces weft-side
// idempotency; Stripe's `identifier` field is the defense in depth.
type StripeUsageReport struct {
	ID            uint      `gorm:"primaryKey"`
	OrgID         OrgID     `gorm:"not null;index:idx_stripe_usage_reports_customer_day,unique,priority:1"`
	Day           time.Time `gorm:"type:date;not null;index:idx_stripe_usage_reports_customer_day,unique,priority:2;index:idx_stripe_usage_reports_day"`
	KbReported    int64     `gorm:"not null"`
	StripeEventID string    `gorm:"not null"`
	ReportedAt    time.Time `gorm:"not null;default:now()"`
}

// TableName overrides GORM's pluralization (stripe_usage_report_s
// would drift from the migration).
func (StripeUsageReport) TableName() string { return "stripe_usage_reports" }

// UsageForDay is one row returned by UsageForDay — the per-customer
// ingestion total for one calendar day, pulled from usage_daily.
type UsageForDay struct {
	OrgID         OrgID
	BytesIngested int64
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
func (s *Store) QueryUsage(orgID OrgID, periodStart, periodEnd time.Time) (int64, error) {
	var result struct{ Total int64 }
	err := s.db.Raw(`
		SELECT COALESCE(SUM(b), 0) AS total FROM (
			-- Complete days from the rolled-up table. Upper bound is the
			-- earlier of (periodEnd, start-of-today), so today's partial
			-- daily row (if any) is ignored here and picked up below.
			SELECT bytes_ingested AS b FROM usage_daily
			WHERE org_id = ?
			  AND day >= ?::date
			  AND day <  LEAST(?::date, (date_trunc('day', now()))::date)
			UNION ALL
			-- Today's live partial. Lower bound is the later of
			-- (periodStart, start-of-today); upper is periodEnd. If the
			-- caller's window ends before today, this half returns zero
			-- rows.
			SELECT bytes_ingested AS b FROM usage_hourly
			WHERE org_id = ?
			  AND hour >= GREATEST(?::timestamptz, date_trunc('day', now()))
			  AND hour <  ?
		) combined`,
		orgID, periodStart, periodEnd,
		orgID, periodStart, periodEnd).Scan(&result).Error
	return result.Total, err
}
