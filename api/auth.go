package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kevlarcon/weft/store"
)

// JWTClaims are the payload of a weft JWT.
type JWTClaims struct {
	UserID       string `json:"sub"`
	Email        string `json:"email"`
	CustomerName string `json:"customer"`
	Tier         string `json:"tier"`
	IssuedAt     int64  `json:"iat"`
	ExpiresAt    int64  `json:"exp"`
	// EmailVerified is the verification state AT TOKEN ISSUE. It's
	// advisory — middleware that truly gates on verification re-checks
	// the DB (RequireEmailVerified), because a stolen token from before
	// verification should keep working if the user has since verified.
	EmailVerified bool `json:"email_verified"`
	// VerificationDeadline is the unix time by which the user must
	// verify or be swept. Set on unverified tokens; 0 for verified
	// tokens. Sent to the frontend so the banner can show a deadline.
	VerificationDeadline int64 `json:"verification_deadline,omitempty"`
}

// UnverifiedGracePeriod is the window a user has to click their verify
// link before the cleanup sweep deletes the account.
const UnverifiedGracePeriod = 24 * time.Hour

// GenerateJWT creates a signed JWT for the user. Tier is for display only
// (dashboard "your plan: Swatch") — enforcement queries the DB on each request.
// Verified-state is similarly advisory; RequireEmailVerified middleware reads
// the user row on each sensitive request.
func GenerateJWT(user *store.User, tier string, secret string) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

	verified := user.EmailVerifiedAt != nil
	var deadline int64
	if !verified {
		deadline = user.CreatedAt.Add(UnverifiedGracePeriod).Unix()
	}

	claims := JWTClaims{
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

// ValidateJWT verifies and decodes a JWT. Returns claims or error.
func ValidateJWT(tokenStr, secret string) (*JWTClaims, error) {
	parts := strings.Split(tokenStr, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid token format")
	}

	// Verify signature.
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	expectedSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(parts[2]), []byte(expectedSig)) {
		return nil, fmt.Errorf("invalid signature")
	}

	// Decode claims.
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode claims: %w", err)
	}
	var claims JWTClaims
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return nil, fmt.Errorf("unmarshal claims: %w", err)
	}

	// Check expiry.
	if time.Now().Unix() > claims.ExpiresAt {
		return nil, fmt.Errorf("token expired")
	}

	return &claims, nil
}
