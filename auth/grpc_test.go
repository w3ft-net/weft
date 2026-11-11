package auth

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/graybeardcorp/weft/store"
)

// testOrgID is a fixed org UUID for fixtures that never touch the database.
const testOrgID = 161

const testSecret = "unit-test-secret"

func signUserJWT(t *testing.T, _ string) string {
	t.Helper()
	now := time.Now()
	tok, err := Generate(&store.User{
		ID: "u1", Email: "u1@example.com",
		EmailVerifiedAt: &now, CreatedAt: now,
	}, testSecret)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return tok
}

// fakeBuilder returns a Session whose active org matches the requested
// name. Used by interceptor tests so they don't need a real DB. The
// builder ignores the orgSelector arg — tests assert against a fixed
// active customer regardless of any X-Active-Org metadata.
func fakeBuilder(customer string) SessionBuilder {
	return func(userID, _ string) (*Session, error) {
		return &Session{
			UserID:             userID,
			Email:              userID + "@example.com",
			ActiveCustomerName: customer,
			ActiveOrgID:        testOrgID,
		}, nil
	}
}

func incomingCtx(pairs ...string) context.Context {
	md := metadata.Pairs(pairs...)
	return metadata.NewIncomingContext(context.Background(), md)
}

// passthroughHandler returns the session it saw as the response so tests
// can assert what the interceptor injected into the context.
func passthroughHandler(ctx context.Context, _ any) (any, error) {
	return SessionFromContext(ctx), nil
}

func TestUnaryInterceptor_RejectsMissingAuth(t *testing.T) {
	interceptor := UnaryServerInterceptor(fakeBuilder("kevlar"), testSecret, nil)
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
	interceptor := UnaryServerInterceptor(fakeBuilder("kevlar"), testSecret, nil)
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
	interceptor := UnaryServerInterceptor(fakeBuilder("kevlar"), testSecret, nil)
	info := &grpc.UnaryServerInfo{FullMethod: "/weft.WeftCoordinator/Query"}

	tok := signUserJWT(t, "kevlar")
	got, err := interceptor(
		incomingCtx("authorization", "Bearer "+tok),
		nil, info, passthroughHandler,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	session, _ := got.(*Session)
	if session == nil {
		t.Fatal("session not injected into context")
	}
	if session.ActiveCustomerName != "kevlar" {
		t.Errorf("customer = %q, want kevlar", session.ActiveCustomerName)
	}
}

func TestUnaryInterceptor_SkipListBypassesAuth(t *testing.T) {
	skip := SkipMethods{"/weft.WeftCoordinator/IngestStream": true}
	interceptor := UnaryServerInterceptor(fakeBuilder("kevlar"), testSecret, skip)
	info := &grpc.UnaryServerInfo{FullMethod: "/weft.WeftCoordinator/IngestStream"}

	// No auth header — but this method is skipped, so it should pass.
	got, err := interceptor(context.Background(), nil, info, passthroughHandler)
	if err != nil {
		t.Fatalf("skipped method should not require auth: %v", err)
	}
	if session, _ := got.(*Session); session != nil {
		t.Error("skipped method should not inject session")
	}
}
