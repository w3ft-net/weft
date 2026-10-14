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
	OrgID             OrgID        `gorm:"not null"`
	Organization      Organization `gorm:"foreignKey:OrgID;constraint:OnDelete:CASCADE"`
	NamespaceID       uint         `gorm:"not null"`
	Namespace         Namespace    `gorm:"foreignKey:NamespaceID;constraint:OnDelete:CASCADE"`
	TokenHash         string       `gorm:"column:token_hash;uniqueIndex;not null"`
	Prefix            string       `gorm:"not null"` // e.g. "wft_a1b2" — safe to display
	Label             string       // human-readable label (e.g., "prod-coordinator")
	Env               string       // optional deployment-environment dimension stamped on every row
	Revoked           bool         `gorm:"not null;default:false"`
	MetricsEnabled    bool         `gorm:"not null;default:false"`
	MetricsInterval   string       // e.g., "30s"
	MetricsCollectors string       // JSON array, e.g., `["cpu","memory","disk"]`
	// LastSeenVersion is the weft-client identifier the coordinator
	// read off the most recent gRPC connection from this token. Used
	// to decide when an old wire-shape parser branch can be deleted
	// (no traffic from agents below version X in N days). Empty until
	// the agent reconnects after migration 048.
	LastSeenVersion string     `gorm:"column:last_seen_version;not null;default:''"`
	LastSeenAt      *time.Time `gorm:"column:last_seen_at"`
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
func (s *Store) CreateAgentToken(orgID OrgID, namespaceID uint, label, env string) (string, *AgentToken, error) {
	raw, err := generateToken()
	if err != nil {
		return "", nil, err
	}
	t := AgentToken{
		OrgID:       orgID,
		NamespaceID: namespaceID,
		TokenHash:   hashAgentToken(raw),
		Prefix:      agentTokenPrefix(raw),
		Label:       label,
		Env:         env,
	}
	if err := s.scoped(orgID).Create(&t).Error; err != nil {
		return "", nil, err
	}
	return raw, &t, nil
}

// AgentAuthResult holds the result of agent token validation.
type AgentAuthResult struct {
	TokenID      uint // primary key of the agent_tokens row; used by RecordAgentVersion
	Organization *Organization
	Namespace    string // namespace name from the token
	Env          string // optional env dimension to stamp on every row from this token
}

// ValidateAgentToken checks a presented raw token and returns the
// associated customer + namespace. Hashes the input and looks up by
// token_hash — the DB never sees the plaintext. Returns nil if the
// token is invalid or revoked.
func (s *Store) ValidateAgentToken(token string) (*AgentAuthResult, error) {
	if s.authCache == nil {
		return s.lookupAgentToken(token)
	}
	return s.authCache.get(hashAgentToken(token), func() (*AgentAuthResult, error) {
		return s.lookupAgentToken(token)
	})
}

// EnableAuthCache puts a short-TTL in-process cache in front of
// ValidateAgentToken. Only long-running servers opt in: a revoked token
// stays valid on a process that did not perform the revoke for up to
// authCacheTTL plus jitter (66s).
func (s *Store) EnableAuthCache() {
	s.authCache = newAuthCache(authCacheTTL, authCacheNegTTL, authCacheMaxEntries)
}

const (
	authCacheTTL        = 60 * time.Second
	authCacheNegTTL     = 5 * time.Second // short so a just-minted token isn't rejected for long
	authCacheMaxEntries = 10_000
)

// RevokeAgentToken marks a token revoked and drops it from this process's
// auth cache so the revoke takes effect here immediately.
func (s *Store) RevokeAgentToken(id uint) error {
	if err := s.crossTenant().Model(&AgentToken{}).Where("id = ?", id).Update("revoked", true).Error; err != nil {
		return err
	}
	if s.authCache != nil {
		s.authCache.invalidateTokenID(id)
	}
	return nil
}

func (s *Store) lookupAgentToken(token string) (*AgentAuthResult, error) {
	var t AgentToken
	// The auth path: the customer is the *result* of this lookup, not an
	// input, so it must run unscoped (keyed by the secret token hash).
	result := s.crossTenant().Preload("Organization").Preload("Namespace").
		Where("token_hash = ? AND revoked = ?", hashAgentToken(token), false).
		First(&t)
	if result.Error == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if result.Error != nil {
		return nil, result.Error
	}
	return &AgentAuthResult{
		TokenID:      t.ID,
		Organization: &t.Organization,
		Namespace:    t.Namespace.Name,
		Env:          t.Env,
	}, nil
}

// ListAgentTokens returns all tokens for a customer, with namespace preloaded.
func (s *Store) ListAgentTokens(orgID OrgID) ([]AgentToken, error) {
	var tokens []AgentToken
	err := s.scoped(orgID).Preload("Namespace").Find(&tokens).Error
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
	// Agent self-reports config keyed by its token hash — no customer context.
	return s.crossTenant().Model(&AgentToken{}).Where("token_hash = ?", hashAgentToken(token)).Updates(map[string]any{
		"metrics_enabled":    enabled,
		"metrics_interval":   interval,
		"metrics_collectors": collectorsJSON,
	}).Error
}

// MetricsForecast holds projected metric ingest for a customer.
type MetricsForecast struct {
	AgentsWithMetrics   int
	ProjectedBytesMonth int64
}

// ForecastMetricsUsage computes projected monthly metric bytes from active agent configs.
func (s *Store) ForecastMetricsUsage(orgID OrgID) (*MetricsForecast, error) {
	var tokens []AgentToken
	err := s.scoped(orgID).Where("revoked = ? AND metrics_enabled = ?",
		false, true).Find(&tokens).Error
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

// SeedDevAgentToken ensures a deterministic dev token exists. Idempotent.
// The raw token is still the known string; only its sha256 lives in the
// DB, same as production tokens.
func (s *Store) SeedDevAgentToken(orgID OrgID, label, nsName string) {
	const devToken = "wft_dev_0000000000000000"
	hash := hashAgentToken(devToken)
	var existing AgentToken
	if s.crossTenant().Where("token_hash = ?", hash).First(&existing).Error == nil {
		return // already exists
	}
	ns, _ := s.FindOrCreateNamespace(orgID, nsName)
	if ns == nil {
		return
	}
	s.scoped(orgID).Create(&AgentToken{
		OrgID:       orgID,
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

// AgentVersionSighting summarises one token's recent connection state.
// Returned by AgentsAtOrBelow for the operator-facing /api/admin/agents
// query.
type AgentVersionSighting struct {
	TokenID         uint
	Prefix          string
	OrgID           OrgID
	NamespaceID     uint
	LastSeenVersion string
	LastSeenAt      *time.Time
}

// AgentsAtOrBelow returns every non-revoked agent token whose
// last_seen_version is non-empty and lexicographically <= the given
// version, with last_seen_at within the lookback window. Used to answer
// "is anyone still on weftd 0.14.x" so the operator can safely delete the
// 0.14-shape parser branch on the server.
//
// The lexicographic compare is sufficient for our use because we always
// query in the same major series (e.g. "0.14.99" is always > "0.14.0"
// while staying < "0.15.0"). Callers that need cross-major comparison
// should split into per-major queries.
func (s *Store) AgentsAtOrBelow(version string, lookback time.Duration) ([]AgentVersionSighting, error) {
	cutoff := time.Now().Add(-lookback)
	var rows []AgentToken
	// Operator fleet-wide query across all customers' agents.
	err := s.crossTenant().
		Where("revoked = ?", false).
		Where("last_seen_version != ''").
		Where("last_seen_version <= ?", version).
		Where("last_seen_at >= ?", cutoff).
		Order("last_seen_version, last_seen_at DESC").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]AgentVersionSighting, len(rows))
	for i, r := range rows {
		out[i] = AgentVersionSighting{
			TokenID:         r.ID,
			Prefix:          r.Prefix,
			OrgID:           r.OrgID,
			NamespaceID:     r.NamespaceID,
			LastSeenVersion: r.LastSeenVersion,
			LastSeenAt:      r.LastSeenAt,
		}
	}
	return out, nil
}
