package service

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/kevlarcon/weft/coordinator"
	"github.com/kevlarcon/weft/logdb"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/store"
)

// newRegisteredShard builds a Server with one registry entry that has a
// known heartbeat token. Used by every heartbeat-auth test below.
func newRegisteredShard(t *testing.T, shardID, token string) *Server {
	t.Helper()
	coord := coordinator.NewCoordinator(coordinator.CoordinatorConfig{})
	coord.Provisioner.Add(&logdb.ShardEntry{
		ID:             shardID,
		State:          logdb.StateIngesting,
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
	coord := coordinator.NewCoordinator(coordinator.CoordinatorConfig{})
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

// TestHeartbeat_EmptyToken_Rejected pins the post-grandfather behavior:
// shards whose registry entry carries an empty HeartbeatToken can't be
// authenticated, so we reject rather than waving them through. The
// pre-migration-015 fleet that this clause used to cover has rotated.
func TestHeartbeat_EmptyToken_Rejected(t *testing.T) {
	srv := newRegisteredShard(t, "shard-legacy", "")
	_, err := srv.Heartbeat(context.Background(), &pb.ShardHeartbeatRequest{
		ShardId: "shard-legacy",
		Addr:    "10.0.0.1:9000",
	})
	if err == nil {
		t.Fatal("empty-token shard heartbeat should be rejected")
	}
	if s, _ := status.FromError(err); s.Code() != codes.Unauthenticated {
		t.Errorf("expected Unauthenticated, got %s", s.Code())
	}
}

// TestHeartbeat_UnknownShardID_Rejected — once §8's grandfather clause
// is gone, an unknown shard id (not in registry, not in Store) is an
// unauthenticated request. The legitimate "registry empty after coord
// restart" case is now handled by falling back to a Store lookup
// (TestHeartbeat_UnknownInRegistry_LoadFromStore).
func TestHeartbeat_UnknownShardID_Rejected(t *testing.T) {
	coord := coordinator.NewCoordinator(coordinator.CoordinatorConfig{})
	srv := &Server{Coord: coord}

	_, err := srv.Heartbeat(context.Background(), &pb.ShardHeartbeatRequest{
		ShardId: "shard-unknown",
		Addr:    "10.0.0.2:9000",
	})
	if err == nil {
		t.Fatal("unknown shard heartbeat should be rejected")
	}
	if s, _ := status.FromError(err); s.Code() != codes.Unauthenticated {
		t.Errorf("expected Unauthenticated, got %s", s.Code())
	}
}

// TestHeartbeat_UnknownInRegistry_LoadFromStore covers the
// rebind-after-coord-restart path: live shards aren't reloaded into
// the in-memory registry on coord boot (LoadFromStore filters to
// ARCHIVED), so the first heartbeat from a live shard arrives with the
// registry empty for that id. The auth path must look the token up in
// the persisted ShardRecord — otherwise tightening the grandfather
// clause would lock the fleet out of the coordinator after every
// rolling restart.
func TestHeartbeat_UnknownInRegistry_LoadFromStore(t *testing.T) {
	tx, cleanup := testStore(t)
	defer cleanup()

	const shardID = "shard-restart-rebind"
	token, err := store.GenerateShardHeartbeatToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.SaveShard(&store.ShardRecord{
		ID:             shardID,
		Apps:           "[]",
		WindowStart:    "2026-04-21T00:00:00Z",
		WindowEnd:      "2026-04-22T00:00:00Z",
		State:          string(logdb.StateIngesting),
		HeartbeatToken: token,
		Tier:           "small",
		BucketID:       1,
		CreatedAt:      "2026-04-21T00:00:00Z",
	}); err != nil {
		t.Fatalf("seed shard record: %v", err)
	}

	coord := coordinator.NewCoordinator(coordinator.CoordinatorConfig{})
	srv := &Server{Coord: coord, Store: tx}

	_, err = srv.Heartbeat(heartbeatCtx(token), &pb.ShardHeartbeatRequest{
		ShardId: shardID,
		Addr:    "10.0.0.5:9000",
		State:   "INGESTING",
	})
	if err != nil {
		t.Fatalf("post-restart heartbeat with valid persisted token rejected: %v", err)
	}

	// And bad bearer is still rejected on the same path.
	_, err = srv.Heartbeat(heartbeatCtx("not-the-real-token"), &pb.ShardHeartbeatRequest{
		ShardId: shardID,
		Addr:    "10.0.0.5:9000",
	})
	if err == nil {
		t.Fatal("bad bearer against persisted token should be rejected")
	}
	if s, _ := status.FromError(err); s.Code() != codes.Unauthenticated {
		t.Errorf("expected Unauthenticated, got %s", s.Code())
	}
}
