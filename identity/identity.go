// Package identity is the seam between the coordinator's gRPC service and
// whoever authenticates its callers. The service asks who is calling through
// this package and never learns how they were authenticated: a deployment
// supplies an Identity that yields the interceptors, and the interceptors bind
// the caller to the request context. docs/plans/tenant-scope.md, Phase 4.
package identity

import (
	"context"

	"google.golang.org/grpc"

	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy"
)

// SkipMethods is the set of gRPC full method names the interceptors pass
// through without requiring a caller. It holds the methods that authenticate
// by some other means (agent tokens, unauthenticated bootstrap).
type SkipMethods map[string]bool

// Caller is the authenticated party behind a request: the org its store calls
// are scoped by and the actor the Scoper and Authorizer judge.
type Caller interface {
	// Org is the store handle of the tenant the caller acts in. Zero means the
	// caller belongs to none.
	Org() store.OrgID
	// Actor is the caller as the party the Scoper and Authorizer judge.
	Actor() tenancy.Actor
	// UserID names the user, or is empty for a caller with no user.
	UserID() string
}

type callerKey struct{}

// WithCaller returns ctx carrying the authenticated caller.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerFrom is the caller WithCaller bound to ctx, or nil when no caller was
// authenticated.
func CallerFrom(ctx context.Context) Caller {
	c, _ := ctx.Value(callerKey{}).(Caller)
	return c
}

type userKey struct{}

// WithUserID returns ctx carrying the authenticated user's id, for a caller
// whose token names a user before any session is built from it.
func WithUserID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, userKey{}, id)
}

// UserIDFrom is the user id bound to ctx: the one WithUserID set, else the
// caller's, else "" (the store treats that as the system identity).
func UserIDFrom(ctx context.Context) string {
	if id, _ := ctx.Value(userKey{}).(string); id != "" {
		return id
	}
	if c := CallerFrom(ctx); c != nil {
		return c.UserID()
	}
	return ""
}

// Identity is what a deployment supplies to authenticate callers. It is
// opened once, where the process reads its secrets, so a missing secret is
// reported once and at the same point on every start.
type Identity interface {
	Open(db *store.Store) Server
}

// Server yields the interceptors that authenticate the callers of one gRPC
// server. skip are the methods that server lets through unauthenticated.
type Server interface {
	Interceptors(skip SkipMethods) ([]grpc.UnaryServerInterceptor, []grpc.StreamServerInterceptor)
}

// Tiered is implemented by a Caller whose plan limits apply to what it does
// (hosted: the subscription tier). A Caller that is not Tiered has none.
type Tiered interface {
	Tier() store.Tier
}

// TierOf is the plan tier c is subject to, or "" when it has none.
func TierOf(c Caller) store.Tier {
	if t, ok := c.(Tiered); ok {
		return t.Tier()
	}
	return ""
}
