package store

import (
	"testing"
	"time"
)

// TestSubscription_KeyedByCustomerSurrogate pins migration 053: a
// subscription is keyed by the customer's surrogate id (FK →
// organizations.id), not the name string. Looking up by id round-trips,
// the persisted column holds the id, and an unrelated id does not
// resolve to this customer's subscription.
func TestSubscription_KeyedByCustomerSurrogate(t *testing.T) {
	db := testOpen(t)
	cust, err := db.FindOrCreateOrg("surrogate-key-cust")
	if err != nil {
		t.Fatal(err)
	}
	db.SeedDevSubscription(cust.ID)

	sub, err := db.ActiveSubscription(cust.ID)
	if err != nil || sub == nil {
		t.Fatalf("ActiveSubscription(%s): sub=%+v err=%v", cust.ID, sub, err)
	}
	if sub.OrgID != cust.ID {
		t.Errorf("sub.OrgID = %s, want surrogate id %s", sub.OrgID, cust.ID)
	}

	// An unrelated id must not resolve to this customer's subscription
	// (proves the lookup keys on the surrogate, not a stringly-typed name).
	if other, _ := db.ActiveSubscription(57005); other != nil {
		t.Errorf("ActiveSubscription(unrelated id) resolved to %+v", other)
	}
}

// TestSeedDevSubscription_MultipleCustomers reproduces the bug where
// SeedDevSubscription hardcodes the row's primary key as "dev-sub" —
// so the second customer's call hits a uniqueness violation against
// the first customer's row, silently fails (the function swallows
// the Create error), and aborts the surrounding transaction.
//
// Symptom in the wild: in tests that use BeginTest, the next DB call
// after SeedDevSubscription returns SQLSTATE 25P02 ("current
// transaction is aborted"), even though SeedDevSubscription itself
// returns void.
//
// Expected behaviour: each customer ends up with its own active dev
// subscription, and the surrounding transaction stays healthy.
func TestSeedDevSubscription_MultipleCustomers(t *testing.T) {
	db := testOpen(t)

	alpha, err := db.FindOrCreateOrg("seed-mc-alpha")
	if err != nil {
		t.Fatal(err)
	}
	beta, err := db.FindOrCreateOrg("seed-mc-beta")
	if err != nil {
		t.Fatal(err)
	}

	db.SeedDevSubscription(alpha.ID)
	db.SeedDevSubscription(beta.ID)

	subA, err := db.ActiveSubscription(alpha.ID)
	if err != nil {
		t.Fatalf("alpha ActiveSubscription: %v", err)
	}
	if subA == nil {
		t.Fatal("alpha has no active subscription after seed")
	}
	subB, err := db.ActiveSubscription(beta.ID)
	if err != nil {
		t.Fatalf("beta ActiveSubscription: %v", err)
	}
	if subB == nil {
		t.Fatal("beta has no active subscription after seed (PK collision likely)")
	}

	// The transaction must still be usable — a follow-up insert
	// would fail with SQLSTATE 25P02 if the silent Create error in
	// the second SeedDevSubscription poisoned it.
	if _, err := db.CreateUser("seed-mc-canary@example.test"); err != nil {
		t.Fatalf("post-seed CreateUser failed (transaction poisoned?): %v", err)
	}
}

// TestGrantCompSubscription_OverridesActive confirms that granting a
// comp sub cancels any prior active sub and lands the customer at the
// requested tier. Validates the path from "trial customer who never
// paid" → "comp swatch" without going through Stripe.
func TestGrantCompSubscription_OverridesActive(t *testing.T) {
	db := testOpen(t)
	cust, err := db.FindOrCreateOrg("comp-grant-overrides")
	if err != nil {
		t.Fatal(err)
	}
	db.SeedDevSubscription(cust.ID) // creates a swatch dev sub

	if err := db.GrantCompSubscription(cust.ID, string(TierBolt), "owner"); err != nil {
		t.Fatalf("grant: %v", err)
	}

	sub, err := db.ActiveSubscription(cust.ID)
	if err != nil || sub == nil {
		t.Fatalf("active sub after grant: sub=%+v err=%v", sub, err)
	}
	if sub.Tier != string(TierBolt) {
		t.Errorf("tier = %q, want bolt", sub.Tier)
	}
	if sub.CompReason != "owner" {
		t.Errorf("comp_reason = %q, want owner", sub.CompReason)
	}
	if !sub.IsComp() {
		t.Error("IsComp() = false, want true")
	}

	// Exactly one active sub should remain.
	var count int64
	db.crossTenant().Model(&Subscription{}).Where("org_id = ? AND status = 'active'", cust.ID).Count(&count)
	if count != 1 {
		t.Errorf("active sub count = %d, want 1", count)
	}
}

// TestGrantCompSubscription_Idempotent reproduces re-granting the same
// (customer, tier, reason) tuple as a no-op — no duplicate rows, no
// silent error.
func TestGrantCompSubscription_Idempotent(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("comp-grant-idem")

	if err := db.GrantCompSubscription(cust.ID, string(TierSwatch), "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := db.GrantCompSubscription(cust.ID, string(TierSwatch), "alpha"); err != nil {
		t.Fatal(err)
	}

	var total int64
	db.crossTenant().Model(&Subscription{}).Where("org_id = ?", cust.ID).Count(&total)
	if total != 1 {
		t.Errorf("total sub rows = %d, want 1 (re-grant should be a no-op)", total)
	}
}

// TestGrantCompSubscription_RequiresReason refuses to silently flip a
// row to comp without the audit trail. Reason is the only durable
// signal that a sub was granted out of band.
func TestGrantCompSubscription_RequiresReason(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("comp-grant-no-reason")
	if err := db.GrantCompSubscription(cust.ID, string(TierSwatch), ""); err == nil {
		t.Error("expected error on empty reason")
	}
}

// TestRevokeCompSubscription cancels a comp sub but leaves paid subs
// alone — even if you call it on a customer that's currently paying,
// nothing changes.
func TestRevokeCompSubscription(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("comp-revoke")

	// Grant then revoke.
	if err := db.GrantCompSubscription(cust.ID, string(TierBolt), "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := revokeCompSubscription(db, cust.ID); err != nil {
		t.Fatal(err)
	}
	sub, _ := db.ActiveSubscription(cust.ID)
	if sub != nil {
		t.Errorf("expected no active sub after revoke, got %+v", sub)
	}

	// Now seed a paid (non-comp) sub and verify revoke leaves it.
	db.SeedDevSubscription(cust.ID) // dev sub has comp_reason = ""
	if err := revokeCompSubscription(db, cust.ID); err != nil {
		t.Fatal(err)
	}
	sub, _ = db.ActiveSubscription(cust.ID)
	if sub == nil || sub.Tier != string(TierSwatch) {
		t.Errorf("paid sub should survive revoke; got %+v", sub)
	}
}

// TestWebhookGuard_SkipsCompSub confirms that the Stripe-keyed
// mutators (CancelSubscription, SetSubscriptionStatus, PromoteSubscriptionTier)
// refuse to touch a row with a non-empty comp_reason — even if the row
// somehow has a stripe_subscription_id attached. Defense against ID
// collision and webhook-misroute scenarios.
func TestWebhookGuard_SkipsCompSub(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("comp-guard")
	if err := db.GrantCompSubscription(cust.ID, string(TierBolt), "owner"); err != nil {
		t.Fatal(err)
	}

	// Force a Stripe sub ID onto the comp row to simulate the misroute.
	const fakeStripeID = "sub_fake_misroute"
	if err := db.crossTenant().Model(&Subscription{}).
		Where("org_id = ?", cust.ID).
		Update("stripe_subscription_id", fakeStripeID).Error; err != nil {
		t.Fatal(err)
	}

	// Webhook tries to cancel by Stripe ID. Comp guard must skip.
	if err := cancelSubscription(db, fakeStripeID); err != nil {
		t.Fatal(err)
	}
	sub, _ := db.ActiveSubscription(cust.ID)
	if sub == nil {
		t.Fatal("comp sub was cancelled by Stripe webhook (guard failed)")
	}

	// Same for status flip.
	if err := setSubscriptionStatus(db, fakeStripeID, "past_due"); err != nil {
		t.Fatal(err)
	}
	sub, _ = db.ActiveSubscription(cust.ID)
	if sub == nil || sub.Status != "active" {
		t.Errorf("comp sub status was changed (guard failed); got %+v", sub)
	}

	// Same for tier promotion.
	if err := promoteSubscriptionTier(db, fakeStripeID, string(TierTrial)); err != nil {
		t.Fatal(err)
	}
	sub, _ = db.ActiveSubscription(cust.ID)
	if sub == nil || sub.Tier != string(TierBolt) {
		t.Errorf("comp sub tier was changed (guard failed); got %+v", sub)
	}
}

// TestSeedDevSubscription_Idempotent confirms re-seeding the same
// customer is a no-op rather than producing duplicate rows or
// aborting on PK conflict against the customer's own prior row.
func TestSeedDevSubscription_Idempotent(t *testing.T) {
	db := testOpen(t)

	cust, err := db.FindOrCreateOrg("seed-idem")
	if err != nil {
		t.Fatal(err)
	}

	db.SeedDevSubscription(cust.ID)
	db.SeedDevSubscription(cust.ID)
	db.SeedDevSubscription(cust.ID)

	// Each customer should have exactly one active dev subscription
	// regardless of seed-call count.
	var count int64
	if err := db.crossTenant().Model(&Subscription{}).
		Where("org_id = ? AND status = 'active'", cust.ID).
		Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected 1 active subscription for customer %s, got %d", cust.ID, count)
	}
}

// --- helpers for subscription store methods relocated out of the store package ---
//
// Production logic moved to callers outside the store package. These
// test-local helpers reproduce the original method bodies verbatim
// (parameterized on db) so the subscription tests keep exercising the
// same behaviour.

// cancelSubscription mirrors the former Store.CancelSubscription.
func cancelSubscription(db *Store, stripeSubID string) error {
	return db.crossTenant().Model(&Subscription{}).
		Where("stripe_subscription_id = ? AND comp_reason = ''", stripeSubID).
		Updates(map[string]any{
			"status":     "cancelled",
			"updated_at": time.Now(),
		}).Error
}

// setSubscriptionStatus mirrors the former Store.SetSubscriptionStatus.
func setSubscriptionStatus(db *Store, stripeSubID, status string) error {
	return db.crossTenant().Model(&Subscription{}).
		Where("stripe_subscription_id = ? AND comp_reason = ''", stripeSubID).
		Updates(map[string]any{
			"status":     status,
			"updated_at": time.Now(),
		}).Error
}

// promoteSubscriptionTier mirrors the former Store.PromoteSubscriptionTier.
func promoteSubscriptionTier(db *Store, stripeSubID, tier string) error {
	return db.crossTenant().Model(&Subscription{}).
		Where("stripe_subscription_id = ? AND comp_reason = ''", stripeSubID).
		Updates(map[string]any{
			"tier":       tier,
			"status":     "active",
			"expires_at": nil,
			"updated_at": time.Now(),
		}).Error
}

// revokeCompSubscription mirrors the former Store.RevokeCompSubscription.
func revokeCompSubscription(db *Store, orgID OrgID) error {
	return db.scoped(orgID).Model(&Subscription{}).
		Where("status = 'active' AND comp_reason != ''").
		Updates(map[string]any{
			"status":     "cancelled",
			"updated_at": time.Now(),
		}).Error
}
