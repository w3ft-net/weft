package store

import (
	"crypto/rand"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// WatchLimit returns the max number of cron watches for a tier.
func WatchLimit(tier string) int {
	switch tier {
	case "swatch":
		return 5
	case "yard":
		return 10
	case "bolt":
		return 50
	default:
		return 1 // trial: no watches
	}
}

// MinWatchInterval returns the minimum allowed schedule interval for a tier.
func MinWatchInterval(tier string) time.Duration {
	switch tier {
	case "swatch", "trial":
		return 5 * time.Minute
	case "yard", "bolt":
		return 1 * time.Minute
	default:
		return 0 // trial: watches not allowed
	}
}

// User represents a weft user. A user belongs to a customer (org).
// The composite unique index allows a user to exist under multiple customers.
type User struct {
	ID                 string   `gorm:"primaryKey"`
	Email              string   `gorm:"not null;uniqueIndex:idx_user_email_customer"`
	CustomerID         uint     `gorm:"not null;uniqueIndex:idx_user_email_customer"`
	Customer           Customer `gorm:"foreignKey:CustomerID"`
	Name               string
	StripeCustomerID   string
	CanCreateNamespace bool `gorm:"not null;default:false"`
	CreatedAt          time.Time
	UpdatedAt          time.Time

	// Deprecated: use CustomerID. Kept temporarily for migration.
	CustomerName string `gorm:"-"` // excluded from DB, computed on read
}

// MagicLink is a one-time-use login token.
type MagicLink struct {
	ID        uint   `gorm:"primaryKey"`
	Email     string `gorm:"not null"`
	Token     string `gorm:"uniqueIndex;not null"`
	ExpiresAt time.Time
	Used      bool `gorm:"not null;default:false"`
}

// SavedQuery is a user's bookmarked pipeline.
type SavedQuery struct {
	ID        string `gorm:"primaryKey"`
	UserID    string `gorm:"not null;index"`
	Name      string `gorm:"not null"`
	Query     string `gorm:"not null"` // the pipeline text
	CreatedAt time.Time
	UpdatedAt time.Time
}

// UsageRecord tracks ingestion bytes per customer per billing period.
type UsageRecord struct {
	ID            uint   `gorm:"primaryKey"`
	CustomerName  string `gorm:"not null;index"`
	PeriodStart   time.Time
	PeriodEnd     time.Time
	BytesIngested int64 `gorm:"not null;default:0"`
}

// TeamMember tracks membership in a customer team.
type TeamMember struct {
	ID           string  `gorm:"primaryKey"`
	CustomerName string  `gorm:"not null;index"`
	Email        string  `gorm:"not null"`
	UserID       *string `gorm:"index"`
	Role         string  `gorm:"not null;default:member"`  // "owner" or "member"
	Status       string  `gorm:"not null;default:pending"` // "pending", "active", "removed"
	InvitedBy    string
	InvitedAt    time.Time
	AcceptedAt   *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// SeatLimit returns the max number of team members for a tier.
func SeatLimit(tier string) int {
	switch tier {
	case "swatch":
		return 5
	case "yard":
		return 10
	case "bolt":
		return 50
	default:
		return 1 // trial
	}
}

// --- Users ---

func (s *Store) CreateUser(email string, customerID uint) (*User, error) {
	now := time.Now()
	user := User{
		ID:         uuid.New().String(),
		Email:      email,
		CustomerID: customerID,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := s.db.Create(&user).Error; err != nil {
		return nil, err
	}
	return &user, nil
}

func (s *Store) GetUserByEmail(email string) (*User, error) {
	var user User
	if err := s.db.Preload("Customer").Where("email = ?", email).First(&user).Error; err != nil {
		return nil, err
	}
	user.CustomerName = user.Customer.Name
	return &user, nil
}

// GetUserByEmailAndCustomer finds a user for a specific customer.
func (s *Store) GetUserByEmailAndCustomer(email string, customerID uint) (*User, error) {
	var user User
	if err := s.db.Preload("Customer").Where("email = ? AND customer_id = ?", email, customerID).First(&user).Error; err != nil {
		return nil, err
	}
	user.CustomerName = user.Customer.Name
	return &user, nil
}

func (s *Store) GetUserByID(id string) (*User, error) {
	var user User
	if err := s.db.Preload("Customer").First(&user, "id = ?", id).Error; err != nil {
		return nil, err
	}
	user.CustomerName = user.Customer.Name
	return &user, nil
}

func (s *Store) UpdateUserStripeCustomer(userID, stripeCustomerID string) error {
	return s.db.Model(&User{}).Where("id = ?", userID).Updates(map[string]any{
		"stripe_customer_id": stripeCustomerID,
		"updated_at":         time.Now(),
	}).Error
}

// --- Magic Links ---

func (s *Store) CreateMagicLink(email string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := fmt.Sprintf("%x", b)
	link := MagicLink{
		Email:     email,
		Token:     token,
		ExpiresAt: time.Now().Add(15 * time.Minute),
	}
	if err := s.db.Create(&link).Error; err != nil {
		return "", err
	}
	return token, nil
}

// ValidateMagicLink validates and consumes a magic link token.
// Returns the email. User creation/lookup is handled by the API layer.
func (s *Store) ValidateMagicLink(token string) (string, error) {
	var link MagicLink
	if err := s.db.Where("token = ? AND used = ? AND expires_at > ?", token, false, time.Now()).First(&link).Error; err != nil {
		return "", fmt.Errorf("invalid or expired magic link")
	}
	s.db.Model(&link).Update("used", true)
	return link.Email, nil
}

// GetLastMagicLinkToken returns the most recent token for an email (for testing).
func (s *Store) GetLastMagicLinkToken(email string) (string, error) {
	var link MagicLink
	err := s.db.Where("email = ?", email).Order("id DESC").First(&link).Error
	if err != nil {
		return "", err
	}
	return link.Token, nil
}

// --- Saved Queries ---

func (s *Store) CreateSavedQuery(userID, name, query string) (*SavedQuery, error) {
	sq := SavedQuery{
		ID:        uuid.New().String(),
		UserID:    userID,
		Name:      name,
		Query:     query,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := s.db.Create(&sq).Error; err != nil {
		return nil, err
	}
	return &sq, nil
}

func (s *Store) ListSavedQueries(userID string) ([]SavedQuery, error) {
	var queries []SavedQuery
	err := s.db.Where("user_id = ?", userID).Order("created_at DESC").Find(&queries).Error
	return queries, err
}

func (s *Store) DeleteSavedQuery(userID, queryID string) error {
	return s.db.Where("id = ? AND user_id = ?", queryID, userID).Delete(&SavedQuery{}).Error
}

// --- Team Members ---

// ListTeamMembers returns active and pending members (not removed).
func (s *Store) ListTeamMembers(customerName string) ([]TeamMember, error) {
	var members []TeamMember
	err := s.db.Where("customer_name = ? AND status != ?", customerName, "removed").
		Order("role ASC, created_at ASC").Find(&members).Error
	return members, err
}

// CreateTeamMember inserts a new team member record.
func (s *Store) CreateTeamMember(m *TeamMember) error {
	return s.db.Create(m).Error
}

// GetTeamMemberByEmail finds a non-removed member by email.
func (s *Store) GetTeamMemberByEmail(customerName, email string) (*TeamMember, error) {
	var m TeamMember
	err := s.db.Where("customer_name = ? AND email = ? AND status != ?", customerName, email, "removed").
		First(&m).Error
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// GetPendingTeamMemberByEmail finds a pending invite by email (across all customers).
func (s *Store) GetPendingTeamMemberByEmail(email string) (*TeamMember, error) {
	var m TeamMember
	err := s.db.Where("email = ? AND status = ?", email, "pending").First(&m).Error
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// GetTeamMemberByID finds a member by ID.
func (s *Store) GetTeamMemberByID(id string) (*TeamMember, error) {
	var m TeamMember
	if err := s.db.First(&m, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &m, nil
}

// AcceptTeamMember marks a pending member as active.
func (s *Store) AcceptTeamMember(id string, userID string) error {
	now := time.Now()
	return s.db.Model(&TeamMember{}).Where("id = ?", id).Updates(map[string]any{
		"user_id":     userID,
		"status":      "active",
		"accepted_at": &now,
		"updated_at":  now,
	}).Error
}

// RemoveTeamMember sets a member's status to removed.
func (s *Store) RemoveTeamMember(id string) error {
	return s.db.Model(&TeamMember{}).Where("id = ?", id).Updates(map[string]any{
		"status":     "removed",
		"updated_at": time.Now(),
	}).Error
}

// CountTeamMembers returns the count of active + pending members.
func (s *Store) CountTeamMembers(customerName string) (int, error) {
	var count int64
	err := s.db.Model(&TeamMember{}).
		Where("customer_name = ? AND status != ?", customerName, "removed").
		Count(&count).Error
	return int(count), err
}

// UpdateUserCustomer sets the customer association for a user.
func (s *Store) UpdateUserCustomer(userID string, customerID uint) error {
	return s.db.Model(&User{}).Where("id = ?", userID).Updates(map[string]any{
		"customer_id": customerID,
		"updated_at":  time.Now(),
	}).Error
}
