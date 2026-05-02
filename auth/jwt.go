// Package auth owns JWT issuance and validation for weft. It's shared
// between the api server (which signs tokens during login/verify) and
// the coordinator gRPC server (which validates incoming user-path RPCs
// via an interceptor) so both halves use exactly the same claims shape
// and signature scheme.
//
// The JWT carries only the user ID. Everything else a handler needs
// (home customer, tier, email, owner role, verification state) is
// materialized per request from the DB via auth.BuildSession. See
// auth/session.go for the resolved-identity story; the JWT is just
// proof of who is calling.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kevlarcon/weft/store"
)

// Claims are the payload of a weft JWT — strictly the user ID and
// timing fields. Anything else lives on Session, looked up per request,
// so state changes (tier, email verification, owner promotion, customer
// rename) propagate without forcing a re-login.
type Claims struct {
	UserID    string `json:"sub"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

// UnverifiedGracePeriod is the window a user has to click their verify
// link before the cleanup sweep deletes the account.
const UnverifiedGracePeriod = 24 * time.Hour

// Generate creates a signed JWT for the user. JWTs are valid for 30
// days and carry only the user ID; all other identity attributes are
// resolved per-request from the DB.
func Generate(user *store.User, secret string) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

	claims := Claims{
		UserID:    user.ID,
		IssuedAt:  time.Now().Unix(),
		ExpiresAt: time.Now().Add(30 * 24 * time.Hour).Unix(),
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(claimsJSON)

	unsigned := header + "." + payload
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(unsigned))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	return unsigned + "." + sig, nil
}

// Validate verifies a JWT signature and decodes the claims. Returns an
// error on bad shape, bad signature, or expired token.
func Validate(tokenStr, secret string) (*Claims, error) {
	parts := strings.Split(tokenStr, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid token format")
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	expectedSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(parts[2]), []byte(expectedSig)) {
		return nil, fmt.Errorf("invalid signature")
	}

	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode claims: %w", err)
	}
	var claims Claims
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return nil, fmt.Errorf("unmarshal claims: %w", err)
	}

	if time.Now().Unix() > claims.ExpiresAt {
		return nil, fmt.Errorf("token expired")
	}

	return &claims, nil
}

// contextKey is a private type so only this package can produce the
// sentinel — prevents accidental collision with other context values.
type contextKey struct{}

var claimsKey = contextKey{}

// WithClaims returns a context carrying the validated claims. Used by
// both the REST auth middleware (api) and the gRPC auth interceptor
// (auth/grpc.go) so downstream handlers retrieve via FromContext
// regardless of protocol.
func WithClaims(ctx context.Context, c *Claims) context.Context {
	return context.WithValue(ctx, claimsKey, c)
}

// FromContext pulls claims previously injected by WithClaims. Returns
// nil if no auth was performed.
func FromContext(ctx context.Context) *Claims {
	c, _ := ctx.Value(claimsKey).(*Claims)
	return c
}
