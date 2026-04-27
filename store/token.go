package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// AgentToken authenticates a weftd agent and maps it to a customer + namespace.
// The raw token (format `wft_<32hex>`) is only ever held by the agent and is
// shown to the user exactly once at create time. The row holds sha256(raw)
// as TokenHash; the short Prefix is safe to display so users can tell their
// tokens apart in the UI.
//
// `Env` is bound at mint time and stamped on every row received from this
// token by the coordinator's IngestStream handler. See
// docs/plans/ingest-dimensions.md.
type AgentToken struct {
	gorm.Model
	CustomerID        uint      `gorm:"not null"`
	Customer          Customer  `gorm:"foreignKey:CustomerID;constraint:OnDelete:CASCADE"`
	NamespaceID       uint      `gorm:"not null"`
	Namespace         Namespace `gorm:"foreignKey:NamespaceID;constraint:OnDelete:CASCADE"`
	TokenHash         string    `gorm:"column:token_hash;uniqueIndex;not null"`
	Prefix            string    `gorm:"not null"` // e.g. "wft_a1b2" — safe to display
	Label             string    // human-readable label (e.g., "prod-coordinator")
	Env               string    // optional deployment-environment dimension stamped on every row
	Revoked           bool      `gorm:"not null;default:false"`
	MetricsEnabled    bool      `gorm:"not null;default:false"`
	MetricsInterval   string    // e.g., "30s"
	MetricsCollectors string    // JSON array, e.g., `["cpu","memory","disk"]`
}

// hashAgentToken returns the sha256 hex digest used for lookup in
// agent_tokens.token_hash. Must be stable: if this changes, every
// existing token stops validating.
func hashAgentToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// agentTokenPrefix returns the safe-to-display leading slice of the
// raw token. 8 chars = "wft_" plus 4 hex digits (~16 bits). Plenty of
// entropy to distinguish tokens in the UI; not enough that leaking a
// prefix meaningfully narrows a brute-force search on the remaining
// 112 bits.
func agentTokenPrefix(raw string) string {
	if len(raw) < 8 {
		return raw
	}
	return raw[:8]
}

// CreateAgentToken generates a new token for a customer and namespace.
// Returns the raw wft_<32hex> string as its first value — this is the
// only moment it exists server-side, so the caller must hand it to the
// user immediately and then discard. The returned *AgentToken contains
// only the hash and prefix.
//
// `env` is optional; pass "" for tokens that don't carry an env
// dimension. When set, the coordinator's IngestStream handler stamps
// it on every row from this token (the agent can't override).
func (s *Store) CreateAgentToken(customerID, namespaceID uint, label, env string) (string, *AgentToken, error) {
	raw, err := generateToken()
	if err != nil {
		return "", nil, err
	}
	t := AgentToken{
		CustomerID:  customerID,
		NamespaceID: namespaceID,
		TokenHash:   hashAgentToken(raw),
		Prefix:      agentTokenPrefix(raw),
		Label:       label,
		Env:         env,
	}
	if err := s.db.Create(&t).Error; err != nil {
		return "", nil, err
	}
	return raw, &t, nil
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

// AgentAuthResult holds the result of agent token validation.
type AgentAuthResult struct {
	Customer  *Customer
	Namespace string // namespace name from the token
	Env       string // optional env dimension to stamp on every row from this token
}

// ValidateAgentToken checks a presented raw token and returns the
// associated customer + namespace. Hashes the input and looks up by
// token_hash — the DB never sees the plaintext. Returns nil if the
// token is invalid or revoked.
func (s *Store) ValidateAgentToken(token string) (*AgentAuthResult, error) {
	var t AgentToken
	result := s.db.Preload("Customer").Preload("Namespace").
		Where("token_hash = ? AND revoked = ?", hashAgentToken(token), false).
		First(&t)
	if result.Error == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if result.Error != nil {
		return nil, result.Error
	}
	return &AgentAuthResult{
		Customer:  &t.Customer,
		Namespace: t.Namespace.Name,
		Env:       t.Env,
	}, nil
}

// RevokeAgentToken disables a token by primary key. The UI only has
// the ID + prefix after creation, so revocation is ID-based.
func (s *Store) RevokeAgentToken(id uint) error {
	return s.db.Model(&AgentToken{}).Where("id = ?", id).Update("revoked", true).Error
}

// ListAgentTokens returns all tokens for a customer, with namespace preloaded.
func (s *Store) ListAgentTokens(customerID uint) ([]AgentToken, error) {
	var tokens []AgentToken
	err := s.db.Preload("Namespace").Where("customer_id = ?", customerID).Find(&tokens).Error
	return tokens, err
}

// UpdateAgentMetricsConfig stores the agent's reported metrics configuration.
// Looks up the row by the hash of the presented raw token.
func (s *Store) UpdateAgentMetricsConfig(token string, enabled bool, interval string, collectors []string) error {
	collectorsJSON := "[]"
	if len(collectors) > 0 {
		b, _ := json.Marshal(collectors)
		collectorsJSON = string(b)
	}
	return s.db.Model(&AgentToken{}).Where("token_hash = ?", hashAgentToken(token)).Updates(map[string]any{
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
// The raw token is still the known string; only its sha256 lives in the
// DB, same as production tokens.
func (s *Store) SeedDevAgentToken(customerID uint, label, nsName string) {
	const devToken = "wft_dev_0000000000000000"
	hash := hashAgentToken(devToken)
	var existing AgentToken
	if s.db.Where("token_hash = ?", hash).First(&existing).Error == nil {
		return // already exists
	}
	ns, _ := s.FindOrCreateNamespace(customerID, nsName)
	if ns == nil {
		return
	}
	s.db.Create(&AgentToken{
		CustomerID:  customerID,
		NamespaceID: ns.ID,
		TokenHash:   hash,
		Prefix:      agentTokenPrefix(devToken),
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
