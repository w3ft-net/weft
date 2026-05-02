package auth

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// SessionBuilder resolves a user ID to a Session. The interceptor
// invokes it after JWT validation. The concrete production builder
// closes over a store handle (see auth.BuildSession); tests pass a
// fake to assert behavior without a DB.
type SessionBuilder func(userID string) (*Session, error)

// SkipMethods is the set of gRPC full method names the interceptors pass
// through without requiring a JWT. Populated with methods that authenticate
// by some other means (agent tokens, unauthenticated bootstrap).
type SkipMethods map[string]bool

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
		session, err := build(claims.UserID)
		if err != nil {
			return nil, status.Errorf(codes.Unauthenticated, "session: %v", err)
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
		session, err := build(claims.UserID)
		if err != nil {
			return status.Errorf(codes.Unauthenticated, "session: %v", err)
		}
		ctx := WithClaims(ss.Context(), claims)
		ctx = WithSession(ctx, session)
		return handler(srv, &claimsServerStream{ServerStream: ss, ctx: ctx})
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

// ClientTokenUnaryInterceptor attaches a bearer token to every outbound
// unary RPC. Used by the api server (JWT-forwarding) and the weft CLI
// (configured WEFT_TOKEN). Returns a no-op interceptor when token is "".
func ClientTokenUnaryInterceptor(token string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx = attachToken(ctx, token)
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// ClientTokenStreamInterceptor is the streaming counterpart to
// ClientTokenUnaryInterceptor.
func ClientTokenStreamInterceptor(token string) grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		ctx = attachToken(ctx, token)
		return streamer(ctx, desc, cc, method, opts...)
	}
}

func attachToken(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	// Don't overwrite an authorization header the caller already set
	// (e.g. api server forwards the end-user's JWT per-request, bypassing
	// this interceptor's default token).
	md, _ := metadata.FromOutgoingContext(ctx)
	if len(md.Get("authorization")) > 0 {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

// SharedTokenUnaryServerInterceptor gates every unary RPC on a single
// shared bearer token — used on server-to-server links where both ends
// are trusted internal services (coordinator ↔ shard). The token is a
// static secret distributed to both endpoints at deploy time (via SSM /
// Secrets Manager) rather than per-request JWT.
//
// Constant-time compare via store.ConstantTimeTokenEqual semantics
// (implemented here so auth doesn't depend on store) — a short-circuit
// length check would leak length via timing. Empty expected token panics
// at construction so a misconfigured deploy never silently accepts
// everything.
func SharedTokenUnaryServerInterceptor(expected string) grpc.UnaryServerInterceptor {
	if expected == "" {
		panic("SharedTokenUnaryServerInterceptor: expected token cannot be empty")
	}
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := checkSharedToken(ctx, expected); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// SharedTokenStreamServerInterceptor is the streaming counterpart to
// SharedTokenUnaryServerInterceptor.
func SharedTokenStreamServerInterceptor(expected string) grpc.StreamServerInterceptor {
	if expected == "" {
		panic("SharedTokenStreamServerInterceptor: expected token cannot be empty")
	}
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := checkSharedToken(ss.Context(), expected); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

func checkSharedToken(ctx context.Context, expected string) error {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get("authorization")
	if len(values) == 0 {
		return status.Errorf(codes.Unauthenticated, "missing authorization metadata")
	}
	const prefix = "Bearer "
	hdr := values[0]
	if len(hdr) < len(prefix) || hdr[:len(prefix)] != prefix {
		return status.Errorf(codes.Unauthenticated, "authorization must be a Bearer token")
	}
	presented := hdr[len(prefix):]
	if !constantTimeEqual(expected, presented) {
		return status.Errorf(codes.Unauthenticated, "shared token mismatch")
	}
	return nil
}

// constantTimeEqual compares two strings in constant time. Duplicated
// from store.ConstantTimeTokenEqual to avoid an auth → store dependency.
func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		// subtle.ConstantTimeCompare already returns 0 for mismatched
		// lengths; the explicit check here is only to skip the compare
		// allocation path for the obvious case.
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}
