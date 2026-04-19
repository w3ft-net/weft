package auth

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/kevlarcon/weft/store"
)

const testSecret = "unit-test-secret"

func signUserJWT(t *testing.T, customer string) string {
	t.Helper()
	now := time.Now()
	tok, err := Generate(&store.User{
		ID: "u1", Email: "u1@example.com",
		CustomerName: customer, EmailVerifiedAt: &now, CreatedAt: now,
	}, "trial", testSecret)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return tok
}

func incomingCtx(pairs ...string) context.Context {
	md := metadata.Pairs(pairs...)
	return metadata.NewIncomingContext(context.Background(), md)
}

// passthroughHandler returns the claims it saw as the response so tests
// can assert what the interceptor injected into the context.
func passthroughHandler(ctx context.Context, _ any) (any, error) {
	return FromContext(ctx), nil
}

func TestUnaryInterceptor_RejectsMissingAuth(t *testing.T) {
	interceptor := UnaryServerInterceptor(testSecret, nil)
	info := &grpc.UnaryServerInfo{FullMethod: "/weft.WeftCoordinator/Query"}

	_, err := interceptor(incomingCtx(), nil, info, passthroughHandler)
	if err == nil {
		t.Fatal("expected Unauthenticated, got nil")
	}
	if s, _ := status.FromError(err); s.Code() != codes.Unauthenticated {
		t.Errorf("expected Unauthenticated, got %s", s.Code())
	}
}

func TestUnaryInterceptor_RejectsBadSignature(t *testing.T) {
	interceptor := UnaryServerInterceptor(testSecret, nil)
	info := &grpc.UnaryServerInfo{FullMethod: "/weft.WeftCoordinator/Query"}

	// Token signed with a different secret should fail validation here.
	bad := signUserJWT(t, "kevlar")
	bad = bad[:len(bad)-4] + "xxxx" // corrupt the signature bytes

	_, err := interceptor(
		incomingCtx("authorization", "Bearer "+bad),
		nil, info, passthroughHandler,
	)
	if err == nil {
		t.Fatal("expected Unauthenticated for bad signature")
	}
	if s, _ := status.FromError(err); s.Code() != codes.Unauthenticated {
		t.Errorf("expected Unauthenticated, got %s", s.Code())
	}
}

func TestUnaryInterceptor_AcceptsValidJWT(t *testing.T) {
	interceptor := UnaryServerInterceptor(testSecret, nil)
	info := &grpc.UnaryServerInfo{FullMethod: "/weft.WeftCoordinator/Query"}

	tok := signUserJWT(t, "kevlar")
	got, err := interceptor(
		incomingCtx("authorization", "Bearer "+tok),
		nil, info, passthroughHandler,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	claims, _ := got.(*Claims)
	if claims == nil {
		t.Fatal("claims not injected into context")
	}
	if claims.CustomerName != "kevlar" {
		t.Errorf("customer = %q, want kevlar", claims.CustomerName)
	}
}

func TestUnaryInterceptor_SkipListBypassesAuth(t *testing.T) {
	skip := SkipMethods{"/weft.WeftCoordinator/IngestStream": true}
	interceptor := UnaryServerInterceptor(testSecret, skip)
	info := &grpc.UnaryServerInfo{FullMethod: "/weft.WeftCoordinator/IngestStream"}

	// No auth header — but this method is skipped, so it should pass.
	got, err := interceptor(context.Background(), nil, info, passthroughHandler)
	if err != nil {
		t.Fatalf("skipped method should not require auth: %v", err)
	}
	if claims, _ := got.(*Claims); claims != nil {
		t.Error("skipped method should not inject claims")
	}
}

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
