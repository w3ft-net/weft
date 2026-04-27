package store

import (
	"log"
	"time"
)

// Subscription tracks a customer's billing state. A customer can have
// multiple subscriptions over time (trial -> paid -> cancelled -> new paid).
// Only the most recent active one matters for access control.
type Subscription struct {
	ID                   string     `gorm:"primaryKey"`
	CustomerID           string     `gorm:"not null;index"` // customer name
	Tier                 string     `gorm:"not null"`       // trial, swatch, yard, bolt
	Status               string     `gorm:"not null"`       // active, cancelled
	StartsAt             time.Time  `gorm:"not null"`
	ExpiresAt            *time.Time                         // nil = no expiry (paid tiers)
	StripeSubscriptionID string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

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
func (s *Store) ActiveSubscription(customerName string) (*Subscription, error) {
	var sub Subscription
	err := s.db.Where("customer_id = ? AND status = 'active'", customerName).
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
	return s.db.Create(sub).Error
}

// SeedDevSubscription ensures an active dev subscription exists. Idempotent.
// The row id is namespaced per customer so seeding multiple customers in
// the same DB doesn't collide on the subscriptions primary key.
func (s *Store) SeedDevSubscription(customerName string) {
	sub, _ := s.ActiveSubscription(customerName)
	if sub != nil {
		return
	}
	now := time.Now()
	if err := s.db.Create(&Subscription{
		ID:         "dev-sub-" + customerName,
		CustomerID: customerName,
		Tier:       "swatch",
		Status:     "active",
		StartsAt:   now,
		CreatedAt:  now,
		UpdatedAt:  now,
	}).Error; err != nil {
		log.Printf("store: SeedDevSubscription(%s): %v", customerName, err)
	}
}

// CancelSubscription marks a subscription as cancelled by Stripe subscription ID.
func (s *Store) CancelSubscription(stripeSubID string) error {
	return s.db.Model(&Subscription{}).
		Where("stripe_subscription_id = ?", stripeSubID).
		Updates(map[string]any{
			"status":     "cancelled",
			"updated_at": time.Now(),
		}).Error
}

// AttachStripeSubscription sets the Stripe subscription ID on an existing
// local subscription row by its local ID.
func (s *Store) AttachStripeSubscription(subID, stripeSubID string) error {
	return s.db.Model(&Subscription{}).
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
	if err := s.db.Where("stripe_subscription_id = ?", stripeSubID).First(&sub).Error; err != nil {
		return nil, err
	}
	return &sub, nil
}

// SetSubscriptionStatus updates the status field on a local subscription row
// identified by its Stripe subscription ID.
func (s *Store) SetSubscriptionStatus(stripeSubID, status string) error {
	return s.db.Model(&Subscription{}).
		Where("stripe_subscription_id = ?", stripeSubID).
		Updates(map[string]any{
			"status":     status,
			"updated_at": time.Now(),
		}).Error
}

// PromoteSubscriptionTier upgrades a subscription's tier and clears its
// trial expiry once payment is confirmed.
func (s *Store) PromoteSubscriptionTier(stripeSubID, tier string) error {
	return s.db.Model(&Subscription{}).
		Where("stripe_subscription_id = ?", stripeSubID).
		Updates(map[string]any{
			"tier":       tier,
			"status":     "active",
			"expires_at": nil,
			"updated_at": time.Now(),
		}).Error
}
