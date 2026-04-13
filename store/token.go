package store

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// AgentToken authenticates a weftd agent and maps it to a customer + namespace.
// Token format: wft_<random> (e.g., wft_abc123def456)
// The token authorizes ingestion for any sub-app within the namespace.
type AgentToken struct {
	gorm.Model
	CustomerID        uint      `gorm:"not null"`
	Customer          Customer  `gorm:"foreignKey:CustomerID"`
	NamespaceID       uint      `gorm:"not null"`
	Namespace         Namespace `gorm:"foreignKey:NamespaceID"`
	Token             string    `gorm:"uniqueIndex;not null"`
	Label             string    // human-readable label (e.g., "prod-coordinator")
	Revoked           bool      `gorm:"not null;default:false"`
	MetricsEnabled    bool      `gorm:"not null;default:false"`
	MetricsInterval   string    // e.g., "30s"
	MetricsCollectors string    // JSON array, e.g., `["cpu","memory","disk"]`
}

// CreateAgentToken generates a new token for a customer and namespace.
func (s *Store) CreateAgentToken(customerID, namespaceID uint, label string) (*AgentToken, error) {
	token, err := generateToken()
	if err != nil {
		return nil, err
	}
	t := AgentToken{
		CustomerID:  customerID,
		NamespaceID: namespaceID,
		Token:       token,
		Label:       label,
	}
	if err := s.db.Create(&t).Error; err != nil {
		return nil, err
	}
	return &t, nil
}

// AgentNamespaceNames returns distinct namespace names from active agent tokens.
func (s *Store) AgentNamespaceNames() ([]string, error) {
	var names []string
	err := s.db.Model(&AgentToken{}).
		Joins("JOIN namespaces ON namespaces.id = agent_tokens.namespace_id").
		Where("agent_tokens.revoked = ?", false).
		Distinct().Pluck("namespaces.name", &names).Error
	return names, err
}

// AgentAppNames is a backward compat alias.
func (s *Store) AgentAppNames() ([]string, error) {
	return s.AgentNamespaceNames()
}

// AgentAuthResult holds the result of agent token validation.
type AgentAuthResult struct {
	Customer  *Customer
	Namespace string // namespace name from the token
}

// ValidateAgentToken checks a token and returns the associated customer + namespace.
// Returns nil if the token is invalid or revoked.
func (s *Store) ValidateAgentToken(token string) (*AgentAuthResult, error) {
	var t AgentToken
	result := s.db.Preload("Customer").Preload("Namespace").Where("token = ? AND revoked = ?", token, false).First(&t)
	if result.Error == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if result.Error != nil {
		return nil, result.Error
	}
	return &AgentAuthResult{
		Customer:  &t.Customer,
		Namespace: t.Namespace.Name,
	}, nil
}

// RevokeAgentToken disables a token.
func (s *Store) RevokeAgentToken(token string) error {
	return s.db.Model(&AgentToken{}).Where("token = ?", token).Update("revoked", true).Error
}

// ListAgentTokens returns all tokens for a customer, with namespace preloaded.
func (s *Store) ListAgentTokens(customerID uint) ([]AgentToken, error) {
	var tokens []AgentToken
	err := s.db.Preload("Namespace").Where("customer_id = ?", customerID).Find(&tokens).Error
	return tokens, err
}

// UpdateAgentMetricsConfig stores the agent's reported metrics configuration.
func (s *Store) UpdateAgentMetricsConfig(token string, enabled bool, interval string, collectors []string) error {
	collectorsJSON := "[]"
	if len(collectors) > 0 {
		b, _ := json.Marshal(collectors)
		collectorsJSON = string(b)
	}
	return s.db.Model(&AgentToken{}).Where("token = ?", token).Updates(map[string]any{
		"metrics_enabled":    enabled,
		"metrics_interval":   interval,
		"metrics_collectors": collectorsJSON,
	}).Error
}

// MetricsForecast holds projected metric ingest for a customer.
type MetricsForecast struct {
	AgentsWithMetrics int
	ProjectedBytesMonth int64
}

// ForecastMetricsUsage computes projected monthly metric bytes from active agent configs.
func (s *Store) ForecastMetricsUsage(customerID uint) (*MetricsForecast, error) {
	var tokens []AgentToken
	err := s.db.Where("customer_id = ? AND revoked = ? AND metrics_enabled = ?",
		customerID, false, true).Find(&tokens).Error
	if err != nil {
		return nil, err
	}

	var totalBytesMonth int64
	for _, t := range tokens {
		intervalSec := 30.0 // default
		if t.MetricsInterval != "" {
			if d, err := time.ParseDuration(t.MetricsInterval); err == nil {
				intervalSec = d.Seconds()
			}
		}
		numCollectors := 5 // default: all
		if t.MetricsCollectors != "" && t.MetricsCollectors != "[]" {
			var c []string
			if json.Unmarshal([]byte(t.MetricsCollectors), &c) == nil && len(c) > 0 {
				numCollectors = len(c)
			}
		}
		// ~300 bytes per metric row, one row per collector per tick
		ticksPerDay := 86400.0 / intervalSec
		bytesPerDay := int64(ticksPerDay * float64(numCollectors) * 300)
		totalBytesMonth += bytesPerDay * 30
	}

	return &MetricsForecast{
		AgentsWithMetrics:   len(tokens),
		ProjectedBytesMonth: totalBytesMonth,
	}, nil
}

// MetricsAppNames returns "<app>.metrics" for each active agent with metrics enabled.
func (s *Store) MetricsAppNames() ([]string, error) {
	var apps []string
	err := s.db.Model(&AgentToken{}).
		Where("revoked = ? AND metrics_enabled = ? AND app != ''", false, true).
		Distinct().Pluck("app", &apps).Error
	if err != nil {
		return nil, err
	}
	for i, a := range apps {
		apps[i] = a + ".metrics"
	}
	return apps, nil
}

// SeedDevAgentToken ensures a deterministic dev token exists. Idempotent.
func (s *Store) SeedDevAgentToken(customerID uint, label, nsName string) {
	const devToken = "wft_dev_0000000000000000"
	var existing AgentToken
	if s.db.Where("token = ?", devToken).First(&existing).Error == nil {
		return // already exists
	}
	ns, _ := s.FindOrCreateNamespace(customerID, nsName)
	if ns == nil {
		return
	}
	s.db.Create(&AgentToken{
		CustomerID:  customerID,
		NamespaceID: ns.ID,
		Token:       devToken,
		Label:       label,
	})
}

func generateToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("wft_%x", b), nil
}
