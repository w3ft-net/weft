// Package auth owns JWT issuance and validation for weft. It's shared
// between the api server (which signs tokens during login/verify) and the
// coordinator gRPC server (which validates incoming user-path RPCs via
// an interceptor) so both halves use exactly the same claims shape and
// signature scheme.
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

// Claims are the payload of a weft JWT.
type Claims struct {
	UserID       string `json:"sub"`
	Email        string `json:"email"`
	CustomerName string `json:"customer"`
	Tier         string `json:"tier"`
	IssuedAt     int64  `json:"iat"`
	ExpiresAt    int64  `json:"exp"`
	// EmailVerified is the verification state AT TOKEN ISSUE. It's
	// advisory — any handler that truly gates on verification re-checks
	// the DB, because a user issued an unverified token who later
	// verifies shouldn't need to re-login to unlock.
	EmailVerified bool `json:"email_verified"`
	// VerificationDeadline is the unix time by which the user must
	// verify or be swept. Non-zero only for unverified tokens; sent to
	// the frontend so the banner can show a deadline.
	VerificationDeadline int64 `json:"verification_deadline,omitempty"`
}

// UnverifiedGracePeriod is the window a user has to click their verify
// link before the cleanup sweep deletes the account.
const UnverifiedGracePeriod = 24 * time.Hour

// Generate creates a signed JWT for the user. Tier is for display only
// (dashboard "your plan: Swatch") — enforcement queries the DB.
func Generate(user *store.User, tier string, secret string) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

	verified := user.EmailVerifiedAt != nil
	var deadline int64
	if !verified {
		deadline = user.CreatedAt.Add(UnverifiedGracePeriod).Unix()
	}

	claims := Claims{
		UserID:               user.ID,
		Email:                user.Email,
		CustomerName:         user.CustomerName,
		Tier:                 tier,
		IssuedAt:             time.Now().Unix(),
		ExpiresAt:            time.Now().Add(30 * 24 * time.Hour).Unix(),
		EmailVerified:        verified,
		VerificationDeadline: deadline,
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
