// Package identity is the seam between the coordinator's gRPC service and
// whoever authenticates its callers. The service asks who is calling through
// this package and never learns how they were authenticated: a deployment
// supplies an Identity that yields the interceptors, and the interceptors bind
// the caller to the request context. docs/plans/tenant-scope.md, Phase 4.
package identity

import (
	"context"

	"google.golang.org/grpc"

	"github.com/w3ft-net/weft/tenancy"
)

// SkipMethods is the set of gRPC full method names the interceptors pass
// through without requiring a caller. It holds the methods that authenticate
// by some other means (agent tokens, unauthenticated bootstrap).
type SkipMethods map[string]bool

// Caller is the authenticated party behind a request.
type Caller interface {
	// Actor is the caller as the party the Scoper and Authorizer judge. Its
	// Partition is the tenant the caller acts in: the one place a request's
	// tenant comes from.
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

// Server yields the interceptors that authenticate the callers of one gRPC
// server. skip are the methods that server lets through unauthenticated.
type Server interface {
	Interceptors(skip SkipMethods) ([]grpc.UnaryServerInterceptor, []grpc.StreamServerInterceptor)
}

// Planned is implemented by a Caller whose plan limits apply to what it does
// (hosted: the subscription tier). A Caller that is not Planned has none. A plan
// is an opaque label: only the deployment that names it knows what it allows.
type Planned interface {
	Plan() string
}

// PlanOf is the plan c is subject to, or "" when it has none.
func PlanOf(c Caller) string {
	if p, ok := c.(Planned); ok {
		return p.Plan()
	}
	return ""
}
