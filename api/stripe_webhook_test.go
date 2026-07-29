package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	stripe "github.com/stripe/stripe-go/v84"
	"github.com/stripe/stripe-go/v84/webhook"

	"github.com/graybeardcorp/saaskit/stripekit"
	"github.com/graybeardcorp/weft/store"
)

// signStripePayload builds a valid Stripe-Signature header for body using
// secret, exactly the way Stripe signs webhooks: "t=<unix>,v1=<hex(hmac)>".
func signStripePayload(body []byte, secret string, ts time.Time) string {
	sig := webhook.ComputeSignature(ts, body, secret)
	return fmt.Sprintf("t=%d,v1=%s", ts.Unix(), hex.EncodeToString(sig))
}

// postWebhook drives the handler directly (bypassing the rate limiter) and
// returns the status code.
func postWebhook(t *testing.T, srv *Server, body []byte, sig string) int {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/billing/webhook", strings.NewReader(string(body)))
	req.Header.Set("Stripe-Signature", sig)
	w := httptest.NewRecorder()
	srv.handleStripeWebhook(w, req)
	return w.Code
}

// subDeletedEvent builds a customer.subscription.deleted event carrying the
// API version Stripe actually delivers today and a single subscription item at
// priceID — the shape webhook.ConstructEvent + the handler parse.
func subDeletedEvent(subID, priceID string, extraMetadata string) []byte {
	ev := map[string]any{
		"id":          "evt_test",
		"object":      "event",
		"api_version": "2026-02-25.clover",
		"type":        "customer.subscription.deleted",
		"data": map[string]any{
			"object": map[string]any{
				"id":     subID,
				"object": "subscription",
				"items": map[string]any{
					"object": "list",
					"data": []any{
						map[string]any{"price": map[string]any{"id": priceID}},
					},
				},
				"metadata": map[string]any{"pad": extraMetadata},
			},
		},
	}
	b, _ := json.Marshal(ev)
	return b
}

// seedActiveSub inserts an active subscription row tied to a fresh customer and
// returns its Stripe subscription id.
func seedActiveSub(t *testing.T, s *store.Store, name, stripeSubID string) {
	t.Helper()
	cust, err := s.FindOrCreateOrg(name)
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	if err := s.CreateSubscription(&store.Subscription{
		ID:                   "sub-" + name,
		OrgID:                cust.ID,
		Tier:                 "swatch",
		Status:               "active",
		StartsAt:             time.Now().AddDate(0, -1, 0),
		StripeSubscriptionID: stripeSubID,
	}); err != nil {
		t.Fatalf("create subscription: %v", err)
	}
}

func subStatus(t *testing.T, s *store.Store, stripeSubID string) string {
	t.Helper()
	var got store.Subscription
	if err := s.DB().WithContext(store.WithoutTenantScope(context.Background())).
		Where("stripe_subscription_id = ?", stripeSubID).First(&got).Error; err != nil {
		t.Fatalf("read subscription %s: %v", stripeSubID, err)
	}
	return got.Status
}

// TestStripeWebhookAcceptsCloverAndCancelsOwnSub reproduces the production
// incident: Stripe delivers 2026-02-25.clover events, the old stripe-go was
// pinned to 2025-08-27.basil, so webhook.ConstructEvent rejected every event
// with a 400 the handler logged as "signature verification failed". After the
// v84 (clover) upgrade a correctly-signed weft event must be accepted AND
// processed (the cancelled subscription row flips to "cancelled").
func TestStripeWebhookAcceptsCloverAndCancelsOwnSub(t *testing.T) {
	srv, s := testServer(t)
	srv.StripeWebhookSecret = "whsec_test_clover"
	srv.StripePrices[store.TierSwatch] = TierPrices{Flat: "price_weft_flat", Overage: "price_weft_overage"}

	seedActiveSub(t, s, "acme", "sub_weft_1")

	body := subDeletedEvent("sub_weft_1", "price_weft_flat", "")
	if code := postWebhook(t, srv, body, signStripePayload(body, srv.StripeWebhookSecret, time.Now())); code != http.StatusOK {
		t.Fatalf("clover weft event: got %d, want 200", code)
	}
	if got := subStatus(t, s, "sub_weft_1"); got != "cancelled" {
		t.Fatalf("weft subscription should be cancelled, got %q", got)
	}

	// Negative control: a bad signature must still be rejected 400 — the fix
	// must not weaken signature verification.
	if code := postWebhook(t, srv, body, fmt.Sprintf("t=%d,v1=deadbeef", time.Now().Unix())); code != http.StatusBadRequest {
		t.Fatalf("bad signature: got %d, want 400", code)
	}
}

// TestStripeWebhookIgnoresForeignProduct is the cross-product guard: weft
// shares a Stripe account with vimgym, so a vimgym subscription.deleted fans
// out to weft's endpoint. If a vimgym Stripe-subscription id ever collided
// with a weft row, the unfiltered handler would cancel a live weft
// subscription. Before the price filter this FAILS (the row gets cancelled).
func TestStripeWebhookIgnoresForeignProduct(t *testing.T) {
	srv, s := testServer(t)
	srv.StripeWebhookSecret = "whsec_test_clover"
	srv.StripePrices[store.TierSwatch] = TierPrices{Flat: "price_weft_flat", Overage: "price_weft_overage"}

	// A weft row whose Stripe id happens to match the incoming (foreign) event.
	seedActiveSub(t, s, "collide", "sub_collision")

	body := subDeletedEvent("sub_collision", "price_vimgym_monthly", "")
	if code := postWebhook(t, srv, body, signStripePayload(body, srv.StripeWebhookSecret, time.Now())); code != http.StatusOK {
		t.Fatalf("foreign event: got %d, want 200 (ignored, not errored)", code)
	}
	if got := subStatus(t, s, "sub_collision"); got != "active" {
		t.Fatalf("a vimgym-priced event must NOT cancel a weft subscription, got %q", got)
	}
}

// TestStripeWebhookAcceptsLargeBody proves the body cap no longer truncates a
// genuinely large event into a phantom signature failure. The body is padded
// past the old 64KB limit; the signature is computed over the full bytes, so a
// truncating read would fail verification and 400.
func TestStripeWebhookAcceptsLargeBody(t *testing.T) {
	srv, s := testServer(t)
	srv.StripeWebhookSecret = "whsec_test_clover"
	srv.StripePrices[store.TierSwatch] = TierPrices{Flat: "price_weft_flat"}

	seedActiveSub(t, s, "bigco", "sub_big")

	pad := strings.Repeat("x", 100*1024) // 100KB > old 64KB cap
	body := subDeletedEvent("sub_big", "price_weft_flat", pad)
	if len(body) <= 65536 {
		t.Fatalf("test bug: body %d not larger than old cap", len(body))
	}
	if code := postWebhook(t, srv, body, signStripePayload(body, srv.StripeWebhookSecret, time.Now())); code != http.StatusOK {
		t.Fatalf("large valid event: got %d, want 200 (body must not be truncated)", code)
	}
}

func TestClassifyWebhookError(t *testing.T) {
	versionErr := fmt.Errorf("Received event with API version 2026-02-25.clover, " +
		"but stripe-go v82.5.1 expects API version 2025-08-27.basil. We recommend...")
	if got := stripekit.ClassifyWebhookError(versionErr); !strings.Contains(got, "API version") {
		t.Errorf("version mismatch should be classified as such, got %q", got)
	}
	if got := stripekit.ClassifyWebhookError(versionErr); strings.Contains(got, "signature") {
		t.Errorf("version mismatch must NOT read as a signature failure, got %q", got)
	}
	if got := stripekit.ClassifyWebhookError(fmt.Errorf("webhook had no valid signature")); !strings.Contains(got, "signature") {
		t.Errorf("signature failure should mention signature, got %q", got)
	}
}

func TestOurPriceFilters(t *testing.T) {
	srv := &Server{StripePrices: map[store.Tier]TierPrices{
		store.TierSwatch: {Flat: "price_weft_flat", Overage: "price_weft_overage"},
	}}

	mkSub := func(price string) *stripe.Subscription {
		return &stripe.Subscription{Items: &stripe.SubscriptionItemList{
			Data: []*stripe.SubscriptionItem{{Price: &stripe.Price{ID: price}}},
		}}
	}
	if !srv.subscriptionHasOurPrice(mkSub("price_weft_overage")) {
		t.Error("weft-priced subscription should match")
	}
	if srv.subscriptionHasOurPrice(mkSub("price_vimgym_monthly")) {
		t.Error("vimgym-priced subscription should not match")
	}
	if srv.subscriptionHasOurPrice(&stripe.Subscription{}) {
		t.Error("subscription with no items should not match")
	}

	mkInv := func(price string) *stripe.Invoice {
		return &stripe.Invoice{Lines: &stripe.InvoiceLineItemList{Data: []*stripe.InvoiceLineItem{
			{Pricing: &stripe.InvoiceLineItemPricing{PriceDetails: &stripe.InvoiceLineItemPricingPriceDetails{Price: price}}},
		}}}
	}
	if !srv.invoiceHasOurPrice(mkInv("price_weft_flat")) {
		t.Error("weft-priced invoice should match")
	}
	if srv.invoiceHasOurPrice(mkInv("price_vimgym_monthly")) {
		t.Error("vimgym-priced invoice should not match")
	}
}
