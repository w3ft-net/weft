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
	"time"

	"github.com/graybeardcorp/saaskit/jwtauth"
	"github.com/graybeardcorp/weft/store"
)

// Claims are the payload of a weft JWT — strictly the user ID and timing
// fields. Anything else lives on Session, looked up per request, so state
// changes (tier, email verification, owner promotion, customer rename)
// propagate without forcing a re-login. Aliased to the shared jwtauth claims;
// weft never sets the optional email field, so the on-wire payload stays
// {sub,iat,exp} — byte-compatible with tokens minted before this migration.
type Claims = jwtauth.Claims

// UnverifiedGracePeriod is the window a user has to click their verify
// link before the cleanup sweep deletes the account.
const UnverifiedGracePeriod = 24 * time.Hour

// Generate creates a signed JWT for the user. JWTs are valid for 30
// days and carry only the user ID; all other identity attributes are
// resolved per-request from the DB.
func Generate(user *store.User, secret string) (string, error) {
	return jwtauth.Generate(user.ID, "", []byte(secret), 30*24*time.Hour)
}

// Validate verifies a JWT signature and decodes the claims. Returns an
// error on bad shape, bad signature, or expired token.
func Validate(tokenStr, secret string) (*Claims, error) {
	return jwtauth.Validate(tokenStr, []byte(secret))
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
