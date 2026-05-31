package api

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/stripe/stripe-go/v84"
	"github.com/stripe/stripe-go/v84/billing/meterevent"
	"gorm.io/gorm/clause"

	"github.com/kevlarcon/weft/store"
)

// meterEventName matches the event_name configured on the Stripe
// meter that every overage price references. See docs/plans/stripe.md.
const meterEventName = "weft_ingestion_kb"

// reportHour is the UTC hour at which the daily usage reporter fires.
// Chosen to be well clear of the hourly rollup ticks in migration
// 028, so yesterday's usage_daily row has settled by the time we
// read it.
const reportHour = 3

// postMeterEventFunc is the injection seam for Stripe's meter-event
// API. Production sets it to meterevent.New; tests override with a
// fake so the orchestration can be exercised without real HTTP.
type postMeterEventFunc func(*stripe.BillingMeterEventParams) (*stripe.BillingMeterEvent, error)

// StartUsageReporter launches the daily Stripe meter-event reporter.
// Runs until ctx is cancelled. Safe to call even when Stripe is not
// configured (empty secret key) — the loop is a no-op without the
// key, which is the expected state in dev.
func (s *Server) StartUsageReporter(ctx context.Context) {
	if s.postMeterEvent == nil {
		s.postMeterEvent = meterevent.New
	}
	go s.usageReporterLoop(ctx)
}

func (s *Server) usageReporterLoop(ctx context.Context) {
	for {
		next := nextReportTime(time.Now())
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			// Always bill for "yesterday in UTC" at wake time. Using
			// the wall-clock yesterday (rather than next - 24h) keeps
			// the logic simple if the machine was asleep across a day
			// boundary — we always report the most recent fully-closed
			// UTC day.
			yesterday := time.Now().UTC().AddDate(0, 0, -1)
			if err := s.ReportUsageForDay(ctx, startOfUTCDay(yesterday)); err != nil {
				log.Printf("usage reporter: %v", err)
			}
		}
	}
}

// ReportUsageForDay is the per-day entry point, broken out from the
// loop so tests can drive it deterministically without waiting on a
// timer. Day must be the start of a UTC day.
func (s *Server) ReportUsageForDay(ctx context.Context, day time.Time) error {
	if s.StripeSecretKey == "" {
		return nil // Stripe not configured (dev / test).
	}
	var rows []store.UsageForDay
	err := s.store.DB().Raw(`
		SELECT customer_name, bytes_ingested
		FROM usage_daily
		WHERE day = ?::date
		ORDER BY customer_name`, day,
	).Scan(&rows).Error
	if err != nil {
		return fmt.Errorf("load usage_daily for %s: %w", day.Format("2006-01-02"), err)
	}

	stripe.Key = s.StripeSecretKey

	for _, r := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.reportOneCustomer(r.CustomerName, day, r.BytesIngested); err != nil {
			// Per-customer failures are logged and skipped; one bad
			// customer can't block reports for the rest. Tomorrow's
			// run retries with the same idempotency identifier.
			log.Printf("usage reporter: customer=%s day=%s: %v",
				r.CustomerName, day.Format("2006-01-02"), err)
		}
	}
	return nil
}

func (s *Server) reportOneCustomer(customerName string, day time.Time, bytes int64) error {
	// weft-side idempotency — cheap UNIQUE-index lookup.
	var reportCount int64
	err := s.store.DB().Raw(`
		SELECT COUNT(*) FROM stripe_usage_reports
		WHERE customer_name = ? AND day = ?::date`,
		customerName, day,
	).Scan(&reportCount).Error
	if err != nil {
		return fmt.Errorf("check audit: %w", err)
	}
	if reportCount > 0 {
		return nil
	}

	// Skip customers without a Stripe customer ID — they never paid
	// through Stripe (dev tenants, legacy imports, etc.). Trials
	// that never converted also land here.
	var stripeCustID string
	if err := s.store.DB().Raw(`
		SELECT u.stripe_customer_id
		FROM users u
		JOIN team_members tm ON tm.user_id = u.id
		JOIN customers c ON tm.customer_id = c.id
		WHERE c.name = ? AND u.stripe_customer_id <> ''
		ORDER BY u.created_at ASC
		LIMIT 1`, customerName,
	).Scan(&stripeCustID).Error; err != nil {
		return fmt.Errorf("lookup stripe_customer_id: %w", err)
	}
	if stripeCustID == "" {
		return nil
	}

	cust, err := s.store.GetCustomerByName(customerName)
	if err != nil || cust == nil {
		return nil
	}
	sub, err := s.store.ActiveSubscription(cust.ID)
	if err != nil || sub == nil {
		return nil
	}
	prices, ok := s.StripePrices[store.Tier(sub.Tier)]
	if !ok || prices.Overage == "" {
		return nil // tier not configured for overage (flat-only pilot mode)
	}

	kb := bytes / 1000
	identifier := fmt.Sprintf("weft:%s:%s", customerName, day.Format("2006-01-02"))
	// Mid-day timestamp: unambiguously inside the reporting day, no
	// timezone edge cases at either boundary. Stripe attributes the
	// event to the billing period covering this timestamp.
	ts := day.Add(12 * time.Hour).Unix()

	event, err := s.postMeterEvent(&stripe.BillingMeterEventParams{
		EventName:  stripe.String(meterEventName),
		Identifier: stripe.String(identifier),
		Timestamp:  stripe.Int64(ts),
		Payload: map[string]string{
			"stripe_customer_id": stripeCustID,
			"value":              strconv.FormatInt(kb, 10),
		},
	})
	if err != nil {
		return fmt.Errorf("stripe meter_event: %w", err)
	}

	// Stripe echoes back `identifier` (either the one we sent or a
	// generated one). Store it so auditors can cross-reference.
	recordedID := identifier
	if event != nil && event.Identifier != "" {
		recordedID = event.Identifier
	}
	return s.store.DB().Clauses(clause.OnConflict{DoNothing: true}).Create(&store.StripeUsageReport{
		CustomerName:  customerName,
		Day:           day,
		KbReported:    kb,
		StripeEventID: recordedID,
		ReportedAt:    time.Now(),
	}).Error
}

// startOfUTCDay truncates t to 00:00:00 UTC of the same day.
func startOfUTCDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// nextReportTime returns the next UTC instant at which the reporter
// should wake — today's reportHour if still in the future, else
// tomorrow's.
func nextReportTime(now time.Time) time.Time {
	now = now.UTC()
	t := time.Date(now.Year(), now.Month(), now.Day(), reportHour, 0, 0, 0, time.UTC)
	if !t.After(now) {
		t = t.Add(24 * time.Hour)
	}
	return t
}
