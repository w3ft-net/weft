package service

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/coordinator"
	"github.com/graybeardcorp/weft/logdb"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/store"
)

// newRegisteredShard builds a Server with one registry entry that has a
// known heartbeat token. Used by every heartbeat-auth test below.
func newRegisteredShard(t *testing.T, shardID, token string) *Server {
	t.Helper()
	coord := coordinator.NewCoordinator(coordinator.CoordinatorConfig{Compute: &compute.Engine{}})
	logdb.SeedShard(coord.Provisioner, &logdb.Shard{
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
	coord := coordinator.NewCoordinator(coordinator.CoordinatorConfig{Compute: &compute.Engine{}})
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

// TestHeartbeat_EmptyToken_Rejected: shards whose registry entry carries an
// empty HeartbeatToken can't be authenticated, so we reject rather than
// waving them through.
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

// TestHeartbeat_UnknownShardID_Rejected — an unknown shard id (not in
// registry, not in Store) is an unauthenticated request. The legitimate
// "registry empty after coord restart" case falls back to a Store lookup
// (TestHeartbeat_UnknownInRegistry_LoadFromStore).
func TestHeartbeat_UnknownShardID_Rejected(t *testing.T) {
	coord := coordinator.NewCoordinator(coordinator.CoordinatorConfig{Compute: &compute.Engine{}})
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
	if err := (logdb.ShardStore{Store: tx}).SaveShard(&logdb.Shard{
		ID:             shardID,
		WindowStart:    time.Date(2026, 4, 21, 0, 0, 0, 0, time.UTC),
		WindowEnd:      time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC),
		State:          logdb.StateIngesting,
		HeartbeatToken: token,
		Tier:           "small",
		BucketID:       1,
		CreatedAt:      time.Date(2026, 4, 21, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seed shard record: %v", err)
	}

	// Pass Store via CoordinatorConfig so NewCoordinator wires it into
	// the provisioner's ShardOps — the auth fallback path now reads
	// the persisted token through the provisioner, not directly off
	// Server.Store (big lift moved auth into Provisioner.AuthenticateShard).
	coord := coordinator.NewCoordinator(coordinator.CoordinatorConfig{Compute: &compute.Engine{}, Store: tx})
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
