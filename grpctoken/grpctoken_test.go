package grpctoken

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func incomingCtx(pairs ...string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(pairs...))
}

func passthroughHandler(context.Context, any) (any, error) { return nil, nil }

func TestClientTokenInterceptor_AttachesBearer(t *testing.T) {
	// Capture the outgoing context the invoker sees.
	var captured context.Context
	invoker := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		captured = ctx
		return nil
	}

	interceptor := ClientTokenUnaryInterceptor("the-jwt")
	if err := interceptor(context.Background(), "/any/Method", nil, nil, nil, invoker); err != nil {
		t.Fatalf("interceptor: %v", err)
	}

	md, _ := metadata.FromOutgoingContext(captured)
	got := md.Get("authorization")
	if len(got) != 1 || got[0] != "Bearer the-jwt" {
		t.Errorf("authorization metadata = %v, want [Bearer the-jwt]", got)
	}
}

func TestClientTokenInterceptor_RespectsPreexistingHeader(t *testing.T) {
	// Simulates the api server forwarding the end user's JWT: it already
	// set the header; the default-token interceptor must not clobber it.
	var captured context.Context
	invoker := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		captured = ctx
		return nil
	}

	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer user-jwt")
	interceptor := ClientTokenUnaryInterceptor("default-cli-token")
	if err := interceptor(ctx, "/any/Method", nil, nil, nil, invoker); err != nil {
		t.Fatalf("interceptor: %v", err)
	}

	md, _ := metadata.FromOutgoingContext(captured)
	got := md.Get("authorization")
	if len(got) != 1 || got[0] != "Bearer user-jwt" {
		t.Errorf("authorization metadata = %v, want [Bearer user-jwt]", got)
	}
}

// ── SharedTokenServerInterceptor (coord ↔ shard) ──────────────

func TestSharedTokenServer_AcceptsMatchingBearer(t *testing.T) {
	interceptor := SharedTokenUnaryServerInterceptor("shared-secret")
	info := &grpc.UnaryServerInfo{FullMethod: "/weft.ShardQuery/Query"}

	_, err := interceptor(
		incomingCtx("authorization", "Bearer shared-secret"),
		nil, info, func(ctx context.Context, req any) (any, error) { return "ok", nil },
	)
	if err != nil {
		t.Fatalf("matching bearer rejected: %v", err)
	}
}

func TestSharedTokenServer_RejectsMissing(t *testing.T) {
	interceptor := SharedTokenUnaryServerInterceptor("shared-secret")
	info := &grpc.UnaryServerInfo{FullMethod: "/weft.ShardQuery/Query"}

	_, err := interceptor(incomingCtx(), nil, info, passthroughHandler)
	if err == nil {
		t.Fatal("expected Unauthenticated, got nil")
	}
	if s, _ := status.FromError(err); s.Code() != codes.Unauthenticated {
		t.Errorf("expected Unauthenticated, got %s", s.Code())
	}
}

func TestSharedTokenServer_RejectsMismatch(t *testing.T) {
	interceptor := SharedTokenUnaryServerInterceptor("shared-secret")
	info := &grpc.UnaryServerInfo{FullMethod: "/weft.ShardQuery/Query"}

	_, err := interceptor(
		incomingCtx("authorization", "Bearer wrong-secret"),
		nil, info, passthroughHandler,
	)
	if err == nil {
		t.Fatal("expected Unauthenticated for mismatch")
	}
	if s, _ := status.FromError(err); s.Code() != codes.Unauthenticated {
		t.Errorf("expected Unauthenticated, got %s", s.Code())
	}
}

func TestSharedTokenServer_EmptyExpectedPanics(t *testing.T) {
	// Defense against misconfigured deploys that would otherwise make
	// the server silently accept everything.
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on empty expected token")
		}
	}()
	_ = SharedTokenUnaryServerInterceptor("")
}
