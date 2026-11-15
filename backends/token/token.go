// Package token is the contract of the agent-token domain: the tokens that
// authenticate a producer to a namespace, and the Store interface every build
// implements (Postgres for the hosted build, SQLite for the open one; see
// defaults and token/tokenstoretest).
//
// A token is minted for one namespace, and optionally one deployment
// environment, which the coordinator stamps on every row received over it. The
// raw token (wft_<32 hex>) exists only at creation: the store holds its sha256,
// and a short prefix that is safe to show. Whose token it is, is said in the
// build's own coordinates, a shape.Added, as in package source: the open
// build's is the empty value and its store has no org.
//
// A token is minted for a namespace by id. Namespaces are another domain's
// (package source), but not another database's: in every build they live in
// the same one, and a token refers to its namespace with a foreign key, so
// deleting the namespace deletes its tokens and a token can say which namespace
// it is for without asking the other store.
//
// The package imports only the standard library and the shape package, so a
// process that links an implementation stays cgo-free.
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/w3ft-net/weft/shape"
)

// ErrNotFound is the answer for a token the caller named that does not exist.
// Another owner's token is indistinguishable from a missing one.
var ErrNotFound = errors.New("token not found")

// NamespaceRef is a namespace as a listed token shows it.
type NamespaceRef struct {
	ID   uint
	Name string
}

// Token is a minted token, as listed. It never carries the raw secret.
type Token struct {
	ID uint
	// PublicID (tok_<16 hex>) is the only identifier that leaves the store: the
	// API, CLI and UI name a token by it for revoke.
	PublicID string
	// Prefix is the first characters of the raw token, safe to display so tokens
	// can be told apart.
	Prefix    string
	Namespace NamespaceRef
	Label     string
	Env       string
	Revoked   bool
	CreatedAt time.Time

	// The metrics config the agent last reported over this token.
	MetricsEnabled    bool
	MetricsInterval   string
	MetricsCollectors []string
}

// Auth is what a valid token stands for.
type Auth struct {
	// TokenID is the token's internal id.
	TokenID uint
	// Owner is whose token it is.
	Owner     shape.Added
	Namespace string
	// Env, when set, is stamped on every row received over the token.
	Env string
}

// MetricsConfig is what an agent reports about the metrics it collects.
type MetricsConfig struct {
	Enabled    bool
	Interval   string
	Collectors []string
}

// Store is the persistence contract of the agent-token domain.
type Store interface {
	// CreateToken mints a token for the namespace, which must exist, and returns
	// the raw secret, which exists server-side only here, so the caller must hand
	// it over at once. env is optional.
	CreateToken(owner shape.Added, namespaceID uint, label, env string) (raw string, t *Token, err error)
	// ValidateToken is the owner and namespace a presented raw token stands for,
	// nil (and no error) when it is unknown or revoked.
	ValidateToken(raw string) (*Auth, error)
	// RevokeToken revokes the owner's token with that public id. ErrNotFound if
	// the owner has none.
	RevokeToken(owner shape.Added, publicID string) error
	// ListTokens returns the owner's tokens, revoked ones included.
	ListTokens(owner shape.Added) ([]Token, error)
	// CountLiveTokens is how many of the namespace's tokens are not revoked.
	CountLiveTokens(owner shape.Added, namespaceID uint) (int, error)
	// SetMetricsConfig records the metrics configuration an agent reports,
	// looked up by the raw token it presented. An unknown token is a no-op.
	SetMetricsConfig(raw string, cfg MetricsConfig) error
}

// NewRaw is a fresh raw token, wft_ and 32 hex digits.
func NewRaw() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("wft_%x", b), nil
}

// NewPublicID is a fresh token public id, tok_ and 16 hex digits.
func NewPublicID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("tok_%x", b), nil
}

// Hash is the sha256 hex digest a store looks a token up by. It must be stable:
// if it changes, every existing token stops validating.
func Hash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// Prefix is the safe-to-display leading slice of a raw token: 8 characters, "wft_"
// and four hex digits. Enough to tell tokens apart; not enough that leaking one
// narrows a brute-force search on the rest.
func Prefix(raw string) string {
	if len(raw) < 8 {
		return raw
	}
	return raw[:8]
}

// MetricsForecast is the projected metric ingest of a set of tokens.
type MetricsForecast struct {
	AgentsWithMetrics   int
	ProjectedBytesMonth int64
}

// ForecastMetrics projects monthly metric bytes from the metrics configs of the
// live tokens in ts.
func ForecastMetrics(ts []Token) MetricsForecast {
	var f MetricsForecast
	for _, t := range ts {
		if t.Revoked || !t.MetricsEnabled {
			continue
		}
		f.AgentsWithMetrics++
		intervalSec := 30.0 // default
		if t.MetricsInterval != "" {
			if d, err := time.ParseDuration(t.MetricsInterval); err == nil {
				intervalSec = d.Seconds()
			}
		}
		collectors := 5 // default: all
		if len(t.MetricsCollectors) > 0 {
			collectors = len(t.MetricsCollectors)
		}
		// ~300 bytes per metric row, one row per collector per tick
		bytesPerDay := int64(86400.0 / intervalSec * float64(collectors) * 300)
		f.ProjectedBytesMonth += bytesPerDay * 30
	}
	return f
}
