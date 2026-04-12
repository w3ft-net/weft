package store

import (
	"crypto/rand"
	"fmt"

	"gorm.io/gorm"
)

// AgentToken authenticates a weftd agent and maps it to a customer.
// Token format: wft_<random> (e.g., wft_abc123def456)
type AgentToken struct {
	gorm.Model
	CustomerID uint     `gorm:"not null"`
	Customer   Customer `gorm:"foreignKey:CustomerID"`
	Token      string   `gorm:"uniqueIndex;not null"`
	Label      string   // human-readable label (e.g., "vimgym-prod")
	App        string   // app name this token authorizes (e.g., "vimgym")
	Revoked    bool     `gorm:"not null;default:false"`
}

// CreateAgentToken generates a new token for a customer and app.
func (s *Store) CreateAgentToken(customerID uint, label, app string) (*AgentToken, error) {
	token, err := generateToken()
	if err != nil {
		return nil, err
	}
	t := AgentToken{
		CustomerID: customerID,
		Token:      token,
		Label:      label,
		App:        app,
	}
	if err := s.db.Create(&t).Error; err != nil {
		return nil, err
	}
	return &t, nil
}

// AgentAppNames returns distinct app names from active (non-revoked) agent tokens.
func (s *Store) AgentAppNames() ([]string, error) {
	var names []string
	err := s.db.Model(&AgentToken{}).
		Where("revoked = ? AND app != ''", false).
		Distinct().Pluck("app", &names).Error
	return names, err
}

// ValidateAgentToken checks a token and returns the associated customer name.
// Returns nil if the token is invalid or revoked.
func (s *Store) ValidateAgentToken(token string) (*Customer, error) {
	var t AgentToken
	result := s.db.Preload("Customer").Where("token = ? AND revoked = ?", token, false).First(&t)
	if result.Error == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if result.Error != nil {
		return nil, result.Error
	}
	return &t.Customer, nil
}

// RevokeAgentToken disables a token.
func (s *Store) RevokeAgentToken(token string) error {
	return s.db.Model(&AgentToken{}).Where("token = ?", token).Update("revoked", true).Error
}

// ListAgentTokens returns all tokens for a customer.
func (s *Store) ListAgentTokens(customerID uint) ([]AgentToken, error) {
	var tokens []AgentToken
	err := s.db.Where("customer_id = ?", customerID).Find(&tokens).Error
	return tokens, err
}

func generateToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("wft_%x", b), nil
}
