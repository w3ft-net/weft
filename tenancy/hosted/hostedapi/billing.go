package hostedapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"

	"github.com/graybeardcorp/saaskit/stripekit"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy"
	"github.com/graybeardcorp/weft/tenancy/hosted"
	"github.com/stripe/stripe-go/v85"
	"github.com/stripe/stripe-go/v85/customer"
	"github.com/stripe/stripe-go/v85/subscription"
	weftsdk "github.com/w3ft-net/weft-go"
)

// --- Billing handlers ---

// ensureStripeCustomer returns the Stripe customer ID for a user, creating one
// on Stripe if the user doesn't have one yet.
func (s *product) ensureStripeCustomer(ctx context.Context, u *store.User, customerName string) (string, error) {
	if u.StripeCustomerID != "" {
		return u.StripeCustomerID, nil
	}
	cust, err := customer.New(&stripe.CustomerParams{
		Params: stripe.Params{Context: ctx},
		Email:  stripe.String(u.Email),
		Metadata: map[string]string{
			"user_id":  u.ID,
			"customer": customerName,
		},
	})
	if err != nil {
		return "", err
	}
	if err := s.Store().DB().Model(&store.User{}).Where("id = ?", u.ID).Updates(map[string]any{
		"stripe_customer_id": cust.ID,
		"updated_at":         time.Now(),
	}).Error; err != nil {
		return "", err
	}
	return cust.ID, nil
}

// handleSetupIntent creates (or reuses) a Stripe Subscription in trialing
// state with payment_behavior=default_incomplete and returns the
// pending_setup_intent's client_secret. The frontend mounts Elements against
// that client_secret and calls stripe.confirmSetup() to collect card details.
func (s *product) handleSetupIntent(w http.ResponseWriter, r *http.Request) {
	session := sessionOf(r)
	if s.Authorize(r.Context(), session.Caller(), hosted.ActionManageBilling, tenancy.Resource{}) != nil {
		http.Error(w, "only the owner can change the plan", http.StatusForbidden)
		return
	}

	var req struct {
		Tier string `json:"tier"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "tier required", http.StatusBadRequest)
		return
	}
	prices, ok := s.StripePrices[store.Tier(req.Tier)]
	if !ok {
		http.Error(w, "invalid tier", http.StatusBadRequest)
		return
	}

	user, err := s.Store().GetUserByID(session.UserID)
	if err != nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}

	stripe.Key = s.StripeSecretKey

	custID, err := s.ensureStripeCustomer(r.Context(), user, session.ActiveCustomerName)
	if err != nil {
		weftsdk.Errorf(r.Context(), "stripe customer create failed: %v", err)
		http.Error(w, "billing setup failed", http.StatusInternalServerError)
		return
	}

	// The row this checkout belongs to: the pending one a signup leaves, or
	// the active legacy trial of an account that predates card-first signup.
	trial, _ := s.Store().PendingPaymentSubscription(session.Org())
	if trial == nil {
		trial, _ = s.Store().ActiveSubscription(session.Org())
	}

	// Reuse an existing trialing Stripe subscription if the trial row already
	// has one attached (handles page refresh / retries idempotently).
	if trial != nil && trial.StripeSubscriptionID != "" {
		getParams := &stripe.SubscriptionParams{Params: stripe.Params{Context: r.Context()}}
		getParams.AddExpand("pending_setup_intent")
		sub, err := subscription.Get(trial.StripeSubscriptionID, getParams)
		if err == nil && sub.PendingSetupIntent != nil && sub.PendingSetupIntent.ClientSecret != "" {
			json.NewEncoder(w).Encode(map[string]string{
				"client_secret":   sub.PendingSetupIntent.ClientSecret,
				"publishable_key": s.StripePublishableKey,
			})
			return
		}
	}

	// Flat-rate item always; metered overage item only when the tier
	// has an overage price configured (absence = flat-only, useful
	// during a per-tier pilot of metered billing).
	items := []*stripe.SubscriptionItemsParams{
		{Price: stripe.String(prices.Flat), Quantity: stripe.Int64(1)},
	}
	if prices.Overage != "" {
		items = append(items, &stripe.SubscriptionItemsParams{
			Price: stripe.String(prices.Overage),
		})
	}
	params := &stripe.SubscriptionParams{
		Params:          stripe.Params{Context: r.Context()},
		Customer:        stripe.String(custID),
		Items:           items,
		TrialPeriodDays: stripe.Int64(14),
		PaymentBehavior: stripe.String("default_incomplete"),
		PaymentSettings: &stripe.SubscriptionPaymentSettingsParams{
			SaveDefaultPaymentMethod: stripe.String(string(stripe.SubscriptionPaymentSettingsSaveDefaultPaymentMethodOnSubscription)),
		},
		TrialSettings: &stripe.SubscriptionTrialSettingsParams{
			EndBehavior: &stripe.SubscriptionTrialSettingsEndBehaviorParams{
				MissingPaymentMethod: stripe.String(string(stripe.SubscriptionTrialSettingsEndBehaviorMissingPaymentMethodCancel)),
			},
		},
		Metadata: map[string]string{
			"user_id":  user.ID,
			"customer": session.ActiveCustomerName,
			"tier":     req.Tier,
		},
	}
	params.AddExpand("pending_setup_intent")

	sub, err := subscription.New(params)
	if err != nil {
		weftsdk.Errorf(r.Context(), "stripe subscription create failed: %v", err)
		http.Error(w, "billing setup failed", http.StatusInternalServerError)
		return
	}
	if sub.PendingSetupIntent == nil || sub.PendingSetupIntent.ClientSecret == "" {
		weftsdk.Infof(r.Context(), "stripe subscription missing pending_setup_intent: id=%s", sub.ID)
		http.Error(w, "billing setup failed", http.StatusInternalServerError)
		return
	}

	// Attach the Stripe subscription ID to our local trial row so refresh
	// reuses the same subscription and the webhook can promote it on confirm.
	if trial != nil {
		// Unscoped, but trial came from the org-scoped reads above,
		// so this id is already known to belong to the caller's org.
		_ = s.Store().DB().WithContext(store.WithoutTenantScope(r.Context())).Model(&store.Subscription{}).
			Where("id = ?", trial.ID).
			Updates(map[string]any{
				"stripe_subscription_id": sub.ID,
				"updated_at":             time.Now(),
			}).Error
	}

	json.NewEncoder(w).Encode(map[string]string{
		"client_secret":   sub.PendingSetupIntent.ClientSecret,
		"publishable_key": s.StripePublishableKey,
	})
}

// handleCheckout handles tier changes for an existing paid subscription.
// It updates the Stripe subscription with the new price and returns either
// {status: "active"} if no confirmation is needed, or a client_secret for the
// latest invoice's PaymentIntent so the frontend can confirm inline.
func (s *product) handleCheckout(w http.ResponseWriter, r *http.Request) {
	session := sessionOf(r)
	if s.Authorize(r.Context(), session.Caller(), hosted.ActionManageBilling, tenancy.Resource{}) != nil {
		http.Error(w, "only the owner can change the plan", http.StatusForbidden)
		return
	}

	var req struct {
		Tier string `json:"tier"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "tier required", http.StatusBadRequest)
		return
	}
	newPrices, ok := s.StripePrices[store.Tier(req.Tier)]
	if !ok {
		http.Error(w, "invalid tier", http.StatusBadRequest)
		return
	}

	stripe.Key = s.StripeSecretKey

	currentSub, _ := s.Store().ActiveSubscription(session.Org())
	if currentSub == nil || currentSub.StripeSubscriptionID == "" {
		http.Error(w, "no active stripe subscription; complete signup first", http.StatusBadRequest)
		return
	}

	// Fetch the existing subscription so we can update items in place.
	// We identify each item by its price's recurring.usage_type:
	// "licensed" for the flat tier item, "metered" for the overage
	// item (if present). Matching by position would be fragile when
	// Stripe reorders or a new tier adds the overage item for the
	// first time.
	existing, err := subscription.Get(currentSub.StripeSubscriptionID, &stripe.SubscriptionParams{Params: stripe.Params{Context: r.Context()}})
	if err != nil {
		weftsdk.Errorf(r.Context(), "stripe subscription lookup failed: %v", err)
		http.Error(w, "billing update failed", http.StatusInternalServerError)
		return
	}
	if len(existing.Items.Data) == 0 {
		http.Error(w, "subscription has no items", http.StatusInternalServerError)
		return
	}
	var flatItemID, overageItemID string
	for _, it := range existing.Items.Data {
		if it.Price == nil || it.Price.Recurring == nil {
			continue
		}
		switch it.Price.Recurring.UsageType {
		case stripe.PriceRecurringUsageTypeMetered:
			overageItemID = it.ID
		default:
			flatItemID = it.ID
		}
	}
	if flatItemID == "" {
		// Shouldn't happen — every subscription has a flat item — but
		// fall back to the first item rather than silently misroute.
		flatItemID = existing.Items.Data[0].ID
	}

	// Update the flat item in place. Add or update the overage item
	// according to the new tier's config — absence means flat-only.
	items := []*stripe.SubscriptionItemsParams{
		{ID: stripe.String(flatItemID), Price: stripe.String(newPrices.Flat)},
	}
	if newPrices.Overage != "" {
		oi := &stripe.SubscriptionItemsParams{Price: stripe.String(newPrices.Overage)}
		if overageItemID != "" {
			oi.ID = stripe.String(overageItemID)
		}
		items = append(items, oi)
	} else if overageItemID != "" {
		// New tier has no overage item but old one does — delete it.
		items = append(items, &stripe.SubscriptionItemsParams{
			ID:      stripe.String(overageItemID),
			Deleted: stripe.Bool(true),
		})
	}

	updateParams := &stripe.SubscriptionParams{
		Params:            stripe.Params{Context: r.Context()},
		Items:             items,
		ProrationBehavior: stripe.String("create_prorations"),
		PaymentBehavior:   stripe.String("default_incomplete"),
		Metadata: map[string]string{
			"tier":     req.Tier,
			"user_id":  session.UserID,
			"customer": session.ActiveCustomerName,
		},
	}
	updateParams.AddExpand("latest_invoice.confirmation_secret")

	updated, err := subscription.Update(currentSub.StripeSubscriptionID, updateParams)
	if err != nil {
		weftsdk.Errorf(r.Context(), "stripe subscription update failed: %v", err)
		http.Error(w, "billing update failed", http.StatusInternalServerError)
		return
	}

	// If the invoice needs a confirmation (3DS or first charge), return its
	// client_secret so the frontend can call stripe.confirmPayment().
	if updated.LatestInvoice != nil && updated.LatestInvoice.ConfirmationSecret != nil {
		cs := updated.LatestInvoice.ConfirmationSecret.ClientSecret
		if cs != "" {
			json.NewEncoder(w).Encode(map[string]any{
				"requires_action": true,
				"client_secret":   cs,
				"publishable_key": s.StripePublishableKey,
			})
			return
		}
	}

	json.NewEncoder(w).Encode(map[string]any{"status": "active"})
}

// ourPriceIDs returns the set of Stripe price IDs that belong to weft, drawn
// from the configured per-tier flat + overage prices. The webhook endpoint
// shares a Stripe account with sibling products (e.g. vimgym), so every
// product's events fan out to it; an event is ours only if it references one
// of these prices.
func (s *product) ourPriceIDs() map[string]bool {
	ids := make(map[string]bool, len(s.StripePrices)*2)
	for _, p := range s.StripePrices {
		if p.Flat != "" {
			ids[p.Flat] = true
		}
		if p.Overage != "" {
			ids[p.Overage] = true
		}
	}
	return ids
}

// subscriptionHasOurPrice reports whether any item on the subscription
// references a weft price. Subscription items are included in the payload.
func (s *product) subscriptionHasOurPrice(sub *stripe.Subscription) bool {
	return stripekit.SubscriptionHasOurPrice(sub, s.ourPriceIDs())
}

// invoiceHasOurPrice reports whether any line item on the invoice references a
// weft price. Invoice line items are included in the webhook payload.
func (s *product) invoiceHasOurPrice(inv *stripe.Invoice) bool {
	return stripekit.InvoiceHasOurPrice(inv, s.ourPriceIDs())
}

func (s *product) handleStripeWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, stripekit.MaxWebhookBytes))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	event, err := stripekit.VerifyWebhook(body, r.Header.Get("Stripe-Signature"), s.StripeWebhookSecret)
	if err != nil {
		// VerifyWebhook fails for two unrelated reasons — a genuine bad
		// signature, or an authentic event whose API-version release train
		// doesn't match the pinned stripe-go. Name which one, so the log
		// points at the real fix instead of a phantom signing-secret problem.
		weftsdk.Infof(r.Context(), "Stripe webhook rejected: %s: %v", stripekit.ClassifyWebhookError(err), err)
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}

	switch event.Type {
	case "customer.subscription.updated":
		// Covers trial→active and tier changes. Promote the local row to the
		// tier recorded in metadata, clearing any trial expiry.
		var sub stripe.Subscription
		if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
			weftsdk.Errorf(r.Context(), "Stripe webhook unmarshal error: %v", err)
			break
		}
		// Shared Stripe account: ignore sibling products' subscriptions.
		if !s.subscriptionHasOurPrice(&sub) {
			break
		}
		tier := sub.Metadata["tier"]
		// Only promote once the subscription is actually paying (not trialing
		// without a PM, and not past_due). A trialing subscription exists, and
		// gets an update, before any card is entered, so trialing counts only
		// once the card is saved as its default payment method.
		paying := sub.Status == stripe.SubscriptionStatusActive ||
			(sub.Status == stripe.SubscriptionStatusTrialing && sub.DefaultPaymentMethod != nil)
		if tier != "" && paying {
			// Unscoped: no session on a webhook. The signature authenticates the call
			// and the Stripe subscription id selects the row.
			if err := s.Store().DB().WithContext(store.WithoutTenantScope(context.WithoutCancel(r.Context()))).Model(&store.Subscription{}).
				Where("stripe_subscription_id = ? AND comp_reason = ''", sub.ID).
				Updates(map[string]any{
					"tier":       tier,
					"status":     "active",
					"expires_at": nil,
					"updated_at": time.Now(),
				}).Error; err != nil {
				weftsdk.Errorf(r.Context(), "failed to promote subscription %s: %v", sub.ID, err)
			} else {
				weftsdk.Infof(r.Context(), "subscription promoted: stripe_sub=%s tier=%s status=%s", sub.ID, tier, sub.Status)
			}
		}

	case "invoice.payment_failed":
		// Mark the local subscription past_due so the UI can prompt for a new card.
		var inv stripe.Invoice
		if err := json.Unmarshal(event.Data.Raw, &inv); err != nil {
			weftsdk.Errorf(r.Context(), "Stripe webhook unmarshal error: %v", err)
			break
		}
		// Shared Stripe account: ignore sibling products' invoices.
		if !s.invoiceHasOurPrice(&inv) {
			break
		}
		if inv.Parent == nil || inv.Parent.SubscriptionDetails == nil {
			break
		}
		if inv.Parent.SubscriptionDetails.Subscription != nil {
			subID := inv.Parent.SubscriptionDetails.Subscription.ID
			// Unscoped: no session on a webhook. The signature authenticates the call
			// and the Stripe subscription id selects the row.
			_ = s.Store().DB().WithContext(store.WithoutTenantScope(context.WithoutCancel(r.Context()))).Model(&store.Subscription{}).
				Where("stripe_subscription_id = ? AND comp_reason = ''", subID).
				Updates(map[string]any{
					"status":     "past_due",
					"updated_at": time.Now(),
				}).Error
			weftsdk.Errorf(r.Context(), "subscription payment failed: stripe_sub=%s", subID)
		}

	case "customer.subscription.deleted":
		var sub stripe.Subscription
		if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
			weftsdk.Errorf(r.Context(), "Stripe webhook unmarshal error: %v", err)
			break
		}
		// Shared Stripe account: ignore sibling products' subscriptions.
		if !s.subscriptionHasOurPrice(&sub) {
			break
		}
		// Unscoped: no session on a webhook. The signature authenticates the call
		// and the Stripe subscription id selects the row.
		if err := s.Store().DB().WithContext(store.WithoutTenantScope(context.WithoutCancel(r.Context()))).Model(&store.Subscription{}).
			Where("stripe_subscription_id = ? AND comp_reason = ''", sub.ID).
			Updates(map[string]any{
				"status":     "cancelled",
				"updated_at": time.Now(),
			}).Error; err != nil {
			weftsdk.Errorf(r.Context(), "failed to cancel subscription %s: %v", sub.ID, err)
		} else {
			weftsdk.Infof(r.Context(), "subscription cancelled: %s", sub.ID)
		}
	}

	w.WriteHeader(http.StatusOK)
}

// projectedOverageCents returns what an extra $N the customer's next
// invoice would carry if the current period's ingestion stopped now.
// Cents, integer-truncated — the dashboard uses this as a heads-up,
// not an invoice line item (Stripe does the precise math at billing
// time). Zero when under the tier allowance or on tiers without an
// overage rate (trial).
func projectedOverageCents(bytes int64, tier store.Tier) int {
	over := bytes - store.VolumeAllowance(tier)
	if over <= 0 {
		return 0
	}
	rate := store.OverageRatePerGB(tier) // cents per decimal GB
	if rate == 0 {
		return 0
	}
	return int((over * int64(rate)) / 1_000_000_000)
}

func (s *product) handleUsage(w http.ResponseWriter, r *http.Request) {
	session := sessionOf(r)
	if sub, _ := s.Store().ActiveSubscription(session.Org()); sub == nil {
		http.Error(w, "no active subscription", http.StatusPaymentRequired)
		return
	}

	resp, err := s.CoordClient().GetUsage(s.CoordCtx(r), &pb.GetUsageRequest{})
	if err != nil {
		weftsdk.Errorf(r.Context(), "GetUsage gRPC error: %v", err)
		http.Error(w, "coordinator error", http.StatusBadGateway)
		return
	}

	// Tier from the resolved Session (which BuildSession looks up
	// via TierForOrg); fall through to the active subscription
	// in case display tier evolves separately.
	tier := session.Tier
	if sub, _ := s.Store().ActiveSubscription(session.Org()); sub != nil {
		tier = store.Tier(sub.Tier)
	}

	result := map[string]any{
		"bytes_ingested":          resp.BytesIngested,
		"tier_limit_bytes":        store.VolumeAllowance(tier),
		"period_start":            resp.PeriodStart,
		"period_end":              resp.PeriodEnd,
		"tier":                    tier,
		"projected_overage_cents": projectedOverageCents(resp.BytesIngested, tier),
	}

	// Add metrics forecast if available.
	if cust, err := s.Store().GetOrgByName(session.ActiveCustomerName); err == nil {
		if forecast, err := s.Store().ForecastMetricsUsage(cust.ID); err == nil && forecast.AgentsWithMetrics > 0 {
			result["metrics_forecast"] = map[string]any{
				"agents_with_metrics":   forecast.AgentsWithMetrics,
				"projected_bytes_month": forecast.ProjectedBytesMonth,
			}
		}
	}

	json.NewEncoder(w).Encode(result)
}
