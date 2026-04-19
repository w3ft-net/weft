package service

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/kevlarcon/weft/coordinator"
	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// newRegisteredShard builds a Server with one registry entry that has a
// known heartbeat token. Used by every heartbeat-auth test below.
func newRegisteredShard(t *testing.T, shardID, token string) *Server {
	t.Helper()
	coord := coordinator.NewCoordinator(nil, "kevlar", nil)
	coord.Registry.Add(&coordinator.ShardEntry{
		ID:             shardID,
		State:          coordinator.StateIngesting,
		Addr:           "10.0.0.1:9000",
		HeartbeatToken: token,
	})
	return &Server{Coord: coord}
}

func heartbeatCtx(token string) context.Context {
	if token == "" {
		return context.Background()
	}
	return metadata.NewIncomingContext(
		context.Background(),
		metadata.Pairs("authorization", "Bearer "+token),
	)
}

func TestHeartbeat_ValidToken_Accepted(t *testing.T) {
	srv := newRegisteredShard(t, "shard-a", "good-token")
	_, err := srv.Heartbeat(heartbeatCtx("good-token"), &pb.ShardHeartbeatRequest{
		ShardId: "shard-a",
		Addr:    "10.0.0.1:9000",
		State:   "INGESTING",
	})
	if err != nil {
		t.Fatalf("valid heartbeat rejected: %v", err)
	}
}

func TestHeartbeat_MissingBearer_Rejected(t *testing.T) {
	srv := newRegisteredShard(t, "shard-a", "good-token")
	_, err := srv.Heartbeat(heartbeatCtx(""), &pb.ShardHeartbeatRequest{
		ShardId: "shard-a",
	})
	if err == nil {
		t.Fatal("expected Unauthenticated, got nil")
	}
	if s, _ := status.FromError(err); s.Code() != codes.Unauthenticated {
		t.Errorf("expected Unauthenticated, got %s", s.Code())
	}
}

func TestHeartbeat_BadBearer_Rejected(t *testing.T) {
	srv := newRegisteredShard(t, "shard-a", "good-token")
	_, err := srv.Heartbeat(heartbeatCtx("evil-token"), &pb.ShardHeartbeatRequest{
		ShardId: "shard-a",
	})
	if err == nil {
		t.Fatal("expected Unauthenticated, got nil")
	}
	if s, _ := status.FromError(err); s.Code() != codes.Unauthenticated {
		t.Errorf("expected Unauthenticated, got %s", s.Code())
	}
}

// TestHeartbeat_EmptyShardID_Christen verifies that the christen path
// (a fresh shard with no identity yet) bypasses auth — it has to,
// because the shard can't present a token it hasn't been given yet.
// The coordinator's provisioner is what mints the identity + token in
// response.
func TestHeartbeat_EmptyShardID_Christen(t *testing.T) {
	coord := coordinator.NewCoordinator(nil, "kevlar", nil)
	srv := &Server{Coord: coord}

	_, err := srv.Heartbeat(context.Background(), &pb.ShardHeartbeatRequest{
		ShardId:    "",
		Addr:       "10.0.0.99:9000",
		InstanceId: "i-fresh",
		State:      "ADOPTED",
	})
	if err != nil {
		t.Fatalf("empty-ID christen heartbeat rejected: %v", err)
	}
}

// TestHeartbeat_PreMigrationShard_Accepted pins the grandfather clause:
// shards that predate migration 015 have an empty HeartbeatToken in
// the registry and must still be accepted during the rollout. Once the
// fleet rotates, those tokens get minted at next christen and the
// accepting branch stops firing.
func TestHeartbeat_PreMigrationShard_Accepted(t *testing.T) {
	srv := newRegisteredShard(t, "shard-legacy", "")
	_, err := srv.Heartbeat(context.Background(), &pb.ShardHeartbeatRequest{
		ShardId: "shard-legacy",
		Addr:    "10.0.0.1:9000",
	})
	if err != nil {
		t.Fatalf("pre-migration shard heartbeat rejected: %v", err)
	}
}

// TestHeartbeat_UnknownShardID_Accepted verifies that heartbeats for
// shard ids not in the registry pass through to the provisioner —
// that's how rebind (EBS volume moved to a new instance) and other
// adoption paths work. If this changes we need to make sure the
// provisioner has its own gate before accepting a non-christen-path
// unknown ID.
func TestHeartbeat_UnknownShardID_Accepted(t *testing.T) {
	coord := coordinator.NewCoordinator(nil, "kevlar", nil)
	srv := &Server{Coord: coord}

	_, err := srv.Heartbeat(context.Background(), &pb.ShardHeartbeatRequest{
		ShardId: "shard-unknown",
		Addr:    "10.0.0.2:9000",
	})
	if err != nil {
		t.Fatalf("unknown-ID heartbeat rejected: %v", err)
	}
}
