package api

import (
	"crypto/rand"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// User represents a weft customer account.
type User struct {
	ID                   string `gorm:"primaryKey"`
	Email                string `gorm:"uniqueIndex;not null"`
	Name                 string
	CustomerName         string // maps to configstore Customer.Name
	Tier                 string `gorm:"not null;default:'trial'"` // trial, starter, growth, scale
	TrialEndsAt          *time.Time
	StripeCustomerID     string
	StripeSubscriptionID string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// MagicLink is a one-time-use login token.
type MagicLink struct {
	ID        uint `gorm:"primaryKey"`
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

// Store manages the API server's SQLite database.
type Store struct {
	db *gorm.DB
}

// NewStore opens or creates the API database.
func NewStore(path string) (*Store, error) {
	db, err := gorm.Open(sqlite.Open(path+"?_journal_mode=WAL"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(&User{}, &MagicLink{}, &SavedQuery{}, &UsageRecord{}); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	db, err := s.db.DB()
	if err != nil {
		return err
	}
	return db.Close()
}

// --- Users ---

func (s *Store) CreateUser(email, customerName string) (*User, error) {
	now := time.Now()
	trialEnd := now.Add(14 * 24 * time.Hour)
	user := User{
		ID:           uuid.New().String(),
		Email:        email,
		CustomerName: customerName,
		Tier:         "trial",
		TrialEndsAt:  &trialEnd,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.db.Create(&user).Error; err != nil {
		return nil, err
	}
	return &user, nil
}

func (s *Store) GetUserByEmail(email string) (*User, error) {
	var user User
	if err := s.db.Where("email = ?", email).First(&user).Error; err != nil {
		return nil, err
	}
	return &user, nil
}

func (s *Store) GetUserByID(id string) (*User, error) {
	var user User
	if err := s.db.First(&user, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &user, nil
}

func (s *Store) UpdateUserStripe(userID, stripeCustomerID, stripeSubID, tier string) error {
	return s.db.Model(&User{}).Where("id = ?", userID).Updates(map[string]any{
		"stripe_customer_id":     stripeCustomerID,
		"stripe_subscription_id": stripeSubID,
		"tier":                   tier,
		"updated_at":             time.Now(),
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

func (s *Store) ValidateMagicLink(token string) (*User, error) {
	var link MagicLink
	if err := s.db.Where("token = ? AND used = ? AND expires_at > ?", token, false, time.Now()).First(&link).Error; err != nil {
		return nil, fmt.Errorf("invalid or expired magic link")
	}
	s.db.Model(&link).Update("used", true)

	// Find or create user.
	user, err := s.GetUserByEmail(link.Email)
	if err != nil {
		// New user.
		user, err = s.CreateUser(link.Email, "")
		if err != nil {
			return nil, err
		}
	}
	return user, nil
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
