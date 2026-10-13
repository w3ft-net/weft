package service

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/coordinator"
	"github.com/graybeardcorp/weft/logdb"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// NewAdminGRPCServer wraps the admin gRPC listener setup: it registers a
// WeftAdmin server gated by a SharedToken interceptor. The coordinator
// boots it on a 127.0.0.1 listener; tests run it on an ephemeral port.
// This is what these tests prove out — that the admin surface is
// unreachable without the correct shared bearer token.

// startTestAdminServer returns the dial target ("127.0.0.1:<port>") for a
// fresh admin gRPC server bound to localhost. The server is torn down on
// test cleanup.
func startTestAdminServer(t *testing.T, server *Server, token string) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := NewAdminGRPCServer(server, token)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { srv.GracefulStop() })
	return lis.Addr().String()
}

// newAdminClient dials the test admin server with optional bearer
// metadata. Returns a thin context that attaches that bearer to outgoing
// RPCs.
func newAdminClient(t *testing.T, target string) (pb.WeftAdminClient, func()) {
	t.Helper()
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return pb.NewWeftAdminClient(conn), func() { conn.Close() }
}

func ctxWithBearer(token string) context.Context {
	ctx, _ := context.WithTimeout(context.Background(), 2*time.Second)
	if token == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

// helperServer is a Server with one INGESTING shard pre-seeded — enough
// for the admin RPCs to do their state checks. The point of these tests is
// only auth, so we don't need a real DB.
func helperServer(t *testing.T) *Server {
	t.Helper()
	coord := coordinator.NewCoordinator(coordinator.CoordinatorConfig{Compute: &compute.Engine{}})
	logdb.SeedShard(coord.Provisioner, &logdb.Shard{ID: "shard-a", State: logdb.StateIngesting})
	return &Server{Coord: coord}
}

func TestAdminGRPC_RejectsMissingToken(t *testing.T) {
	target := startTestAdminServer(t, helperServer(t), "expected-token")
	client, close := newAdminClient(t, target)
	defer close()

	_, err := client.RelaunchShard(ctxWithBearer(""), &pb.RelaunchShardRequest{ShardId: "shard-a"})
	if err == nil {
		t.Fatal("expected Unauthenticated for missing token, got nil")
	}
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %s: %v", got, err)
	}
}

func TestAdminGRPC_RejectsWrongToken(t *testing.T) {
	target := startTestAdminServer(t, helperServer(t), "expected-token")
	client, close := newAdminClient(t, target)
	defer close()

	_, err := client.RelaunchShard(ctxWithBearer("wrong-token"), &pb.RelaunchShardRequest{ShardId: "shard-a"})
	if err == nil {
		t.Fatal("expected Unauthenticated for wrong token, got nil")
	}
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %s: %v", got, err)
	}
}

func TestAdminGRPC_AcceptsCorrectToken(t *testing.T) {
	target := startTestAdminServer(t, helperServer(t), "expected-token")
	client, close := newAdminClient(t, target)
	defer close()

	// With the right token the request must reach the handler. We use
	// GetShard because its nil-store guard returns codes.Unavailable
	// cleanly (no panic), and Unavailable proves we made it past the auth
	// interceptor — auth would have been Unauthenticated.
	_, err := client.GetShard(ctxWithBearer("expected-token"), &pb.GetShardRequest{ShardId: "shard-a"})
	if err == nil {
		return // handler ran successfully — even better
	}
	if got := status.Code(err); got == codes.Unauthenticated {
		t.Fatalf("auth gate must allow the correct token through, got Unauthenticated: %v", err)
	}
}
