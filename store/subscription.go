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
	ExpiresAt            *time.Time // nil = no expiry (paid tiers)
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
