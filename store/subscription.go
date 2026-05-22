package store

import (
	"fmt"
	"log"
	"time"

	"gorm.io/gorm"
)

// Subscription tracks a customer's billing state. A customer can have
// multiple subscriptions over time (trial -> paid -> cancelled -> new paid).
// Only the most recent active one matters for access control.
type Subscription struct {
	ID                   string     `gorm:"primaryKey"`
	CustomerID           uint       `gorm:"not null;index"` // FK → customers.id
	Tier                 string     `gorm:"not null"`       // trial, swatch, yard, bolt
	Status               string     `gorm:"not null"`       // active, cancelled
	StartsAt             time.Time  `gorm:"not null"`
	ExpiresAt            *time.Time                         // nil = no expiry (paid tiers)
	StripeSubscriptionID string
	// CompReason is the audit string when this subscription is comped
	// rather than paid (owner / design-partner / etc.). Empty for paid
	// subs. The Stripe webhook mutators refuse to touch rows where this
	// is non-empty, so a misfire can never accidentally cancel a comp.
	CompReason string `gorm:"not null;default:''"`
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// IsComp returns true if this subscription was granted out-of-band
// rather than created through Stripe. Callers that want to distinguish
// "shouldn't appear on a billing dashboard" use this; tier-limit
// callers don't care — comp swatch behaves identically to paid swatch.
func (sub *Subscription) IsComp() bool { return sub.CompReason != "" }

// IsActive returns true if this subscription grants access.
// Cancelled subscriptions are inactive. Active subscriptions with an
// ExpiresAt in the past are also inactive (expired trial).
func (sub *Subscription) IsActive() bool {
	if sub.Status == "cancelled" {
		return false
	}
	if sub.ExpiresAt != nil && time.Now().After(*sub.ExpiresAt) {
		return false
	}
	return sub.Status == "active"
}

// --- Subscriptions ---

// ActiveSubscription returns the most recent active subscription for a
// customer, or nil if none exists or the subscription has expired.
func (s *Store) ActiveSubscription(customerID uint) (*Subscription, error) {
	var sub Subscription
	err := s.scoped(customerID).Where("status = 'active'").
		Order("created_at DESC").First(&sub).Error
	if err != nil {
		return nil, err
	}
	if !sub.IsActive() {
		return nil, nil // expired
	}
	return &sub, nil
}

// CreateSubscription inserts a new subscription record.
func (s *Store) CreateSubscription(sub *Subscription) error {
	return s.scoped(sub.CustomerID).Create(sub).Error
}

// SeedDevSubscription ensures an active dev subscription exists. Idempotent.
// The row id is namespaced per customer so seeding multiple customers in
// the same DB doesn't collide on the subscriptions primary key.
func (s *Store) SeedDevSubscription(customerID uint) {
	sub, _ := s.ActiveSubscription(customerID)
	if sub != nil {
		return
	}
	now := time.Now()
	if err := s.scoped(customerID).Create(&Subscription{
		ID:         fmt.Sprintf("dev-sub-%d", customerID),
		CustomerID: customerID,
		Tier:       "swatch",
		Status:     "active",
		StartsAt:   now,
		CreatedAt:  now,
		UpdatedAt:  now,
	}).Error; err != nil {
		log.Printf("store: SeedDevSubscription(%d): %v", customerID, err)
	}
}

// CancelSubscription marks a subscription as cancelled by Stripe
// subscription ID. Comp subs (comp_reason != '') are skipped — they
// have no Stripe sub ID anyway, but the guard makes the intent
// explicit and protects against ID collisions.
func (s *Store) CancelSubscription(stripeSubID string) error {
	return s.crossTenant().Model(&Subscription{}).
		Where("stripe_subscription_id = ? AND comp_reason = ''", stripeSubID).
		Updates(map[string]any{
			"status":     "cancelled",
			"updated_at": time.Now(),
		}).Error
}

// AttachStripeSubscription sets the Stripe subscription ID on an existing
// local subscription row by its local ID.
func (s *Store) AttachStripeSubscription(subID, stripeSubID string) error {
	return s.crossTenant().Model(&Subscription{}).
		Where("id = ?", subID).
		Updates(map[string]any{
			"stripe_subscription_id": stripeSubID,
			"updated_at":             time.Now(),
		}).Error
}

// GetSubscriptionByStripeID returns the local subscription row matching a
// Stripe subscription ID, or nil if none.
func (s *Store) GetSubscriptionByStripeID(stripeSubID string) (*Subscription, error) {
	var sub Subscription
	if err := s.crossTenant().Where("stripe_subscription_id = ?", stripeSubID).First(&sub).Error; err != nil {
		return nil, err
	}
	return &sub, nil
}

// SetSubscriptionStatus updates the status field on a local subscription
// row identified by its Stripe subscription ID. Comp subs are skipped.
func (s *Store) SetSubscriptionStatus(stripeSubID, status string) error {
	return s.crossTenant().Model(&Subscription{}).
		Where("stripe_subscription_id = ? AND comp_reason = ''", stripeSubID).
		Updates(map[string]any{
			"status":     status,
			"updated_at": time.Now(),
		}).Error
}

// PromoteSubscriptionTier upgrades a subscription's tier and clears its
// trial expiry once payment is confirmed. Comp subs are skipped.
func (s *Store) PromoteSubscriptionTier(stripeSubID, tier string) error {
	return s.crossTenant().Model(&Subscription{}).
		Where("stripe_subscription_id = ? AND comp_reason = ''", stripeSubID).
		Updates(map[string]any{
			"tier":       tier,
			"status":     "active",
			"expires_at": nil,
			"updated_at": time.Now(),
		}).Error
}

// GrantCompSubscription cancels any active subscription for the customer
// and inserts a fresh comp subscription at the given tier. Idempotent:
// re-granting the same (customer, tier, reason) is a no-op when the
// active sub already matches; otherwise it cancels-and-replaces.
//
// Customer is the surrogate id (FK → customers.id). Reason must be
// non-empty so it can be distinguished from a paid sub.
func (s *Store) GrantCompSubscription(customerID uint, tier, reason string) error {
	if reason == "" {
		return fmt.Errorf("comp grant requires a non-empty reason")
	}

	now := time.Now()
	return s.scoped(customerID).Transaction(func(tx *gorm.DB) error {
		// Idempotent: if the most recent active sub already matches,
		// skip the cancel+insert.
		var current Subscription
		err := tx.Where("status = 'active'").
			Order("created_at DESC").First(&current).Error
		if err == nil && current.Tier == tier && current.CompReason == reason {
			return nil
		}

		// Cancel any active subscription for this customer.
		if err := tx.Model(&Subscription{}).
			Where("status = 'active'").
			Updates(map[string]any{
				"status":     "cancelled",
				"updated_at": now,
			}).Error; err != nil {
			return err
		}

		return tx.Create(&Subscription{
			ID:         fmt.Sprintf("comp-%d-%s", customerID, now.Format("20060102150405")),
			CustomerID: customerID,
			Tier:       tier,
			Status:     "active",
			StartsAt:   now,
			CompReason: reason,
			CreatedAt:  now,
			UpdatedAt:  now,
		}).Error
	})
}

// RevokeCompSubscription cancels the active comp subscription for the
// customer. Refuses to touch a paid sub. Returns nil if there is no
// active comp sub (idempotent).
func (s *Store) RevokeCompSubscription(customerID uint) error {
	return s.scoped(customerID).Model(&Subscription{}).
		Where("status = 'active' AND comp_reason != ''").
		Updates(map[string]any{
			"status":     "cancelled",
			"updated_at": time.Now(),
		}).Error
}
