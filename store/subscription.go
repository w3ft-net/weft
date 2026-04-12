package store

import "time"

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

// CancelSubscription marks a subscription as cancelled by Stripe subscription ID.
func (s *Store) CancelSubscription(stripeSubID string) error {
	return s.db.Model(&Subscription{}).
		Where("stripe_subscription_id = ?", stripeSubID).
		Updates(map[string]any{
			"status":     "cancelled",
			"updated_at": time.Now(),
		}).Error
}
