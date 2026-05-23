package api

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v82"

	"github.com/kevlarcon/weft/store"
)

// fakeMeter records every Stripe meter event the reporter would have
// posted. Replaces meterevent.New via the postMeterEvent injection
// seam so we can exercise the full orchestration without real HTTP.
type fakeMeter struct {
	events []*stripe.BillingMeterEventParams
	// failNext causes the next call to return an error, used to
	// pin the "Stripe failure leaves no audit row; next run retries"
	// behavior.
	failNext bool
}

func (f *fakeMeter) post(p *stripe.BillingMeterEventParams) (*stripe.BillingMeterEvent, error) {
	if f.failNext {
		f.failNext = false
		return nil, fmt.Errorf("simulated Stripe outage")
	}
	f.events = append(f.events, p)
	return &stripe.BillingMeterEvent{Identifier: derefStr(p.Identifier)}, nil
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// seedReporterFixtures creates a customer with one user that has a
// Stripe customer ID, an active swatch subscription, tier-configured
// overage prices, and yesterday's usage_daily row. Returns the
// customer name + bytes ingested for assertions.
func seedReporterFixtures(t *testing.T, srv *Server, s *store.Store, customerName string, bytes int64, yesterday time.Time) {
	t.Helper()

	cust, err := s.FindOrCreateCustomer(customerName)
	if err != nil {
		t.Fatal(err)
	}
	user, err := s.CreateUser(customerName+"@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTeamMember(&store.TeamMember{
		ID:         "tm-" + customerName,
		CustomerID: cust.ID,
		Email:      user.Email,
		UserID:     &user.ID,
		Role:       "owner",
		Status:     "active",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().Model(&store.User{}).Where("id = ?", user.ID).Updates(map[string]any{
		"stripe_customer_id": "cus_" + customerName,
		"updated_at":         time.Now(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSubscription(&store.Subscription{
		ID:         "sub-" + customerName,
		CustomerID: cust.ID,
		Tier:       "swatch",
		Status:     "active",
		StartsAt:   time.Now().AddDate(0, -1, 0),
	}); err != nil {
		t.Fatal(err)
	}

	// Seed usage_daily directly. The reporter reads from here; the
	// rollup plumbing is out of scope for this test.
	if err := s.DB().Exec(`
		INSERT INTO usage_hourly (customer_name, hour, bytes_ingested)
		VALUES (?, ?, ?)
		ON CONFLICT (customer_name, hour) DO UPDATE
		SET bytes_ingested = usage_hourly.bytes_ingested + EXCLUDED.bytes_ingested`,
		customerName, yesterday.Add(12*time.Hour), bytes).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB().Exec("CALL public.rollup_usage_daily()").Error; err != nil {
		t.Fatal(err)
	}

	srv.StripePrices[store.TierSwatch] = TierPrices{
		Flat:    "price_flat_test",
		Overage: "price_overage_test",
	}
}

// newReporterStore opens a test store in a rolled-back transaction,
// same pattern as testServer in api_test.go but without an HTTP
// server wrapper — reporter tests don't exercise handlers.
func newReporterStore(t *testing.T) (*store.Store, func()) {
	t.Helper()
	dsn := testDSN(t)
	s, err := store.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	tx, rollback := s.BeginTest()
	return tx, func() {
		rollback()
		s.Close()
	}
}

// TestUsageReporter_PostsAndAudits exercises the full happy path:
// seed a customer with bytes, run the reporter, expect one meter
// event and one audit row.
func TestUsageReporter_PostsAndAudits(t *testing.T) {
	s, cleanup := newReporterStore(t)
	defer cleanup()

	srv := NewServer(s, "test-jwt", "", "http://localhost")
	srv.StripeSecretKey = "sk_test_dummy"
	fake := &fakeMeter{}
	srv.postMeterEvent = fake.post

	yesterday := time.Now().UTC().AddDate(0, 0, -1)
	yesterday = time.Date(yesterday.Year(), yesterday.Month(), yesterday.Day(), 0, 0, 0, 0, time.UTC)

	// 15 GB ingested = 15,000,000 KB. Well over the 10 GB free
	// quota, so this customer would see an overage charge.
	const bytes int64 = 15_000_000_000
	seedReporterFixtures(t, srv, s, "reporter-test-co", bytes, yesterday)

	if err := srv.ReportUsageForDay(context.Background(), yesterday); err != nil {
		t.Fatalf("reporter: %v", err)
	}

	if len(fake.events) != 1 {
		t.Fatalf("expected 1 meter event, got %d", len(fake.events))
	}
	got := fake.events[0]
	if derefStr(got.EventName) != meterEventName {
		t.Errorf("event_name = %q, want %q", derefStr(got.EventName), meterEventName)
	}
	if got.Payload["stripe_customer_id"] != "cus_reporter-test-co" {
		t.Errorf("payload[stripe_customer_id] = %q", got.Payload["stripe_customer_id"])
	}
	wantKB := strconv.FormatInt(bytes/1000, 10)
	if got.Payload["value"] != wantKB {
		t.Errorf("payload[value] = %q, want %q", got.Payload["value"], wantKB)
	}
	wantIdentifier := fmt.Sprintf("weft:reporter-test-co:%s", yesterday.Format("2006-01-02"))
	if derefStr(got.Identifier) != wantIdentifier {
		t.Errorf("identifier = %q, want %q", derefStr(got.Identifier), wantIdentifier)
	}

	// Audit row must exist.
	has, err := hasUsageReportFor(s, "reporter-test-co", yesterday)
	if err != nil || !has {
		t.Errorf("audit row missing after success (has=%v, err=%v)", has, err)
	}

	// Second run is a no-op: no new events, audit still one row.
	if err := srv.ReportUsageForDay(context.Background(), yesterday); err != nil {
		t.Fatal(err)
	}
	if len(fake.events) != 1 {
		t.Errorf("second run posted %d events, want 0 (idempotent)", len(fake.events)-1)
	}
}

// TestUsageReporter_StripeFailureLeavesNoAudit confirms the retry
// contract: a Stripe error does NOT write an audit row, so the next
// run re-reports the same day with the same `identifier`. Stripe's
// own dedup on `identifier` is what protects against a successful-
// but-lost-response double-bill.
func TestUsageReporter_StripeFailureLeavesNoAudit(t *testing.T) {
	s, cleanup := newReporterStore(t)
	defer cleanup()

	srv := NewServer(s, "test-jwt", "", "http://localhost")
	srv.StripeSecretKey = "sk_test_dummy"
	fake := &fakeMeter{failNext: true}
	srv.postMeterEvent = fake.post

	yesterday := time.Now().UTC().AddDate(0, 0, -1)
	yesterday = time.Date(yesterday.Year(), yesterday.Month(), yesterday.Day(), 0, 0, 0, 0, time.UTC)
	seedReporterFixtures(t, srv, s, "reporter-fail-co", 5_000_000_000, yesterday)

	if err := srv.ReportUsageForDay(context.Background(), yesterday); err != nil {
		t.Fatal(err)
	}

	has, err := hasUsageReportFor(s, "reporter-fail-co", yesterday)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Errorf("audit row written despite Stripe error — would cause silent drop on retry")
	}

	// Simulate the next day's retry — Stripe recovers, event posts,
	// audit gets written this time.
	if err := srv.ReportUsageForDay(context.Background(), yesterday); err != nil {
		t.Fatal(err)
	}
	if len(fake.events) != 1 {
		t.Errorf("expected 1 retry event after recovery, got %d", len(fake.events))
	}
	has, _ = hasUsageReportFor(s, "reporter-fail-co", yesterday)
	if !has {
		t.Errorf("audit row missing after successful retry")
	}
}

// TestUsageReporter_SkipsWhenNoOveragePrice confirms tiers without
// a configured overage price (flat-only pilot mode, or trial) get
// skipped silently — no events, no audit rows.
func TestUsageReporter_SkipsWhenNoOveragePrice(t *testing.T) {
	s, cleanup := newReporterStore(t)
	defer cleanup()

	srv := NewServer(s, "test-jwt", "", "http://localhost")
	srv.StripeSecretKey = "sk_test_dummy"
	fake := &fakeMeter{}
	srv.postMeterEvent = fake.post

	yesterday := time.Now().UTC().AddDate(0, 0, -1)
	yesterday = time.Date(yesterday.Year(), yesterday.Month(), yesterday.Day(), 0, 0, 0, 0, time.UTC)
	seedReporterFixtures(t, srv, s, "reporter-flat-only", 5_000_000_000, yesterday)

	// Overwrite the tier config to flat-only (no Overage price).
	srv.StripePrices[store.TierSwatch] = TierPrices{Flat: "price_flat_test"}

	if err := srv.ReportUsageForDay(context.Background(), yesterday); err != nil {
		t.Fatal(err)
	}
	if len(fake.events) != 0 {
		t.Errorf("flat-only tier posted %d events, want 0", len(fake.events))
	}
	has, _ := hasUsageReportFor(s, "reporter-flat-only", yesterday)
	if has {
		t.Errorf("audit row written for flat-only tier — should have been skipped")
	}
}

// TestNextReportTime pins the 03:00 UTC schedule.
func TestNextReportTime(t *testing.T) {
	// Before 03:00 → today's 03:00.
	at := time.Date(2026, 4, 23, 1, 30, 0, 0, time.UTC)
	got := nextReportTime(at)
	want := time.Date(2026, 4, 23, 3, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("before 03:00: got %s, want %s", got, want)
	}
	// After 03:00 → tomorrow's 03:00.
	at = time.Date(2026, 4, 23, 14, 0, 0, 0, time.UTC)
	got = nextReportTime(at)
	want = time.Date(2026, 4, 24, 3, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("after 03:00: got %s, want %s", got, want)
	}
}

// hasUsageReportFor inlines the former store.HasUsageReportFor, which was
// relocated into production callers. Reports whether an audit row exists
// for (customer, day).
func hasUsageReportFor(s *store.Store, customerName string, day time.Time) (bool, error) {
	var n int64
	err := s.DB().Raw(`
		SELECT COUNT(*) FROM stripe_usage_reports
		WHERE customer_name = ? AND day = ?::date`,
		customerName, day,
	).Scan(&n).Error
	return n > 0, err
}
