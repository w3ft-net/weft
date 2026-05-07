package store

import (
	"time"

	"gorm.io/gorm/clause"
)

// StripeUsageReport is the audit log for one day of ingestion usage
// posted to Stripe's billing/meter_events API. Written only on
// successful API response — absence signals "needs report" on the
// next reporter tick. UNIQUE(customer_name, day) enforces weft-side
// idempotency; Stripe's `identifier` field is the defense in depth.
type StripeUsageReport struct {
	ID            uint      `gorm:"primaryKey"`
	CustomerName  string    `gorm:"not null;index:idx_stripe_usage_reports_customer_day,unique,priority:1"`
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
	CustomerName  string
	BytesIngested int64
}

// UsageDailyForDay returns every (customer, bytes) row for a single
// calendar day from usage_daily. Used by the Stripe reporter to
// enumerate who needs a meter event posted for yesterday's usage.
func (s *Store) UsageDailyForDay(day time.Time) ([]UsageForDay, error) {
	var rows []UsageForDay
	err := s.db.Raw(`
		SELECT customer_name, bytes_ingested
		FROM usage_daily
		WHERE day = ?::date
		ORDER BY customer_name`, day,
	).Scan(&rows).Error
	return rows, err
}

// HasUsageReportFor returns true if a (customer, day) usage report
// has already been successfully posted to Stripe. Cheap — hits the
// UNIQUE index on stripe_usage_reports.
func (s *Store) HasUsageReportFor(customerName string, day time.Time) (bool, error) {
	var n int64
	err := s.db.Raw(`
		SELECT COUNT(*) FROM stripe_usage_reports
		WHERE customer_name = ? AND day = ?::date`,
		customerName, day,
	).Scan(&n).Error
	return n > 0, err
}

// InsertUsageReport records a successful Stripe meter-event post.
// Uses ON CONFLICT DO NOTHING so a concurrent reporter run (or a
// retry that crossed the Stripe API success line before failing
// afterward) doesn't raise; the UNIQUE constraint is the source of
// truth for whether a given (customer, day) has been reported.
func (s *Store) InsertUsageReport(r *StripeUsageReport) error {
	return s.db.Clauses(clause.OnConflict{DoNothing: true}).Create(r).Error
}

// CallRollupUsageDaily invokes the rollup_usage_daily() stored
// procedure from migration 028. Used by tests to deterministically
// drive the hourly→daily rollup without waiting on pg_cron. Not
// used in production — the ticker fires the proc via pg_cron.
func (s *Store) CallRollupUsageDaily() error {
	return s.db.Exec("CALL public.rollup_usage_daily()").Error
}

// StripeCustomerIDForCustomerName returns any Stripe customer ID
// associated with a weft customer. For multi-user customers we pick
// the first user who has a populated StripeCustomerID; in practice
// one weft customer maps to one Stripe customer (the one created via
// ensureStripeCustomer at signup), so "first" is deterministic
// enough. Returns "" with no error when no users have a Stripe
// customer ID — that's a signal to the reporter to skip.
func (s *Store) StripeCustomerIDForCustomerName(customerName string) (string, error) {
	var id string
	err := s.db.Raw(`
		SELECT u.stripe_customer_id
		FROM users u
		JOIN team_members tm ON tm.user_id = u.id
		JOIN customers c ON tm.customer_id = c.id
		WHERE c.name = ? AND u.stripe_customer_id <> ''
		ORDER BY u.created_at ASC
		LIMIT 1`, customerName,
	).Scan(&id).Error
	if err != nil {
		return "", err
	}
	return id, nil
}

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
