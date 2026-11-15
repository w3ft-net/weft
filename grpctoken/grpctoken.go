// Package grpctoken is the bearer-token gRPC interceptors the internal links
// use: a client that attaches a token to every call, and a server that admits
// only calls carrying one shared secret. They know nothing of users, sessions
// or tenants, so the shard, the coordinator and the CLIs can use them without
// the auth package that does.
package grpctoken

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

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
// (implemented here so this package depends on nothing of ours) — a short-circuit
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
