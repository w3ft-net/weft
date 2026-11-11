package auth

import (
	"context"
	"errors"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// SessionBuilder resolves a user ID + active-org selector into a
// Session. The interceptor invokes it after JWT validation, passing
// the X-Active-Org header (or empty if unset) so production builders
// can route through auth.BuildSession; tests pass a fake to assert
// behavior without a DB.
type SessionBuilder func(userID, orgSelector string) (*Session, error)

// activeOrgMetadataKey is the gRPC metadata key for the requested
// active org. gRPC normalizes header names to lowercase; the wire-
// level form is "x-active-org" (matching the HTTP X-Active-Org header
// the api server reads).
const activeOrgMetadataKey = "x-active-org"

// SkipMethods is the set of gRPC full method names the interceptors pass
// through without requiring a JWT. Populated with methods that authenticate
// by some other means (agent tokens, unauthenticated bootstrap).
type SkipMethods map[string]bool

// extractActiveOrg returns the X-Active-Org metadata value if present,
// or "" if absent. Empty selector triggers BuildSession's default
// (last-active or oldest membership).
func extractActiveOrg(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get(activeOrgMetadataKey)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// ExtractJWT reads the authorization metadata from an incoming gRPC
// context, strips the "Bearer " prefix, and validates the signature.
// Returns a structured Unauthenticated error on failure so the
// interceptors can surface it directly.
func ExtractJWT(ctx context.Context, secret string) (*Claims, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get("authorization")
	if len(values) == 0 {
		return nil, status.Errorf(codes.Unauthenticated, "missing authorization metadata")
	}
	token := values[0]
	const prefix = "Bearer "
	if len(token) < len(prefix) || token[:len(prefix)] != prefix {
		return nil, status.Errorf(codes.Unauthenticated, "authorization must be a Bearer token")
	}
	token = token[len(prefix):]
	claims, err := Validate(token, secret)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "%v", err)
	}
	return claims, nil
}

// UnaryServerInterceptor validates a JWT on every unary RPC not listed
// in skip, materializes the caller's Session via build, and injects
// both the claims and the resolved Session into the handler's context.
// Methods in skip pass through untouched so they can do their own auth
// (e.g. agent-token bearer for IngestStream, pre-auth bootstrap for
// Authenticate itself).
func UnaryServerInterceptor(build SessionBuilder, secret string, skip SkipMethods) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if skip[info.FullMethod] {
			return handler(ctx, req)
		}
		claims, err := ExtractJWT(ctx, secret)
		if err != nil {
			return nil, err
		}
		session, err := build(claims.UserID, extractActiveOrg(ctx))
		if err != nil {
			return nil, sessionErrToStatus(err)
		}
		ctx = WithClaims(ctx, claims)
		ctx = WithSession(ctx, session)
		return handler(ctx, req)
	}
}

// StreamServerInterceptor is the streaming counterpart to
// UnaryServerInterceptor.
func StreamServerInterceptor(build SessionBuilder, secret string, skip SkipMethods) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if skip[info.FullMethod] {
			return handler(srv, ss)
		}
		claims, err := ExtractJWT(ss.Context(), secret)
		if err != nil {
			return err
		}
		session, err := build(claims.UserID, extractActiveOrg(ss.Context()))
		if err != nil {
			return sessionErrToStatus(err)
		}
		ctx := WithClaims(ss.Context(), claims)
		ctx = WithSession(ctx, session)
		return handler(srv, &claimsServerStream{ServerStream: ss, ctx: ctx})
	}
}

// sessionErrToStatus translates BuildSession's typed errors into gRPC
// status codes. ErrNoMembership and ErrOrgNotMember are PermissionDenied
// (the JWT is valid, the user just isn't allowed to act in the
// requested scope); anything else is Unauthenticated.
func sessionErrToStatus(err error) error {
	switch {
	case errors.Is(err, ErrOrgNotMember):
		return status.Errorf(codes.PermissionDenied, "not a member of the requested org")
	case errors.Is(err, ErrNoMembership):
		return status.Errorf(codes.PermissionDenied, "no org memberships")
	default:
		return status.Errorf(codes.Unauthenticated, "session: %v", err)
	}
}

// claimsServerStream overrides Context() so downstream handlers see the
// claims-enriched ctx. Needed because grpc.ServerStream exposes the
// context through a method rather than a field.
type claimsServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *claimsServerStream) Context() context.Context { return s.ctx }
