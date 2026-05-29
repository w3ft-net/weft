package service

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/kevlarcon/weft/coordinator"
	"github.com/kevlarcon/weft/logdb"
	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// shardAdminServer builds a Server whose registry holds one shard in the
// given state — enough to drive the admin force-archive / delete
// preconditions without a launcher or DB.
func shardAdminServer(t *testing.T, shardID string, state logdb.ShardState) *Server {
	t.Helper()
	coord := coordinator.NewCoordinator(coordinator.CoordinatorConfig{})
	logdb.SeedShard(coord.Provisioner, &logdb.Shard{ID: shardID, State: state})
	return &Server{Coord: coord}
}

func wantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got nil error", want)
	}
	if got, _ := status.FromError(err); got.Code() != want {
		t.Fatalf("expected %s, got %s: %v", want, got.Code(), err)
	}
}

// The API server relies on these gRPC codes to render 404 / 409 — it no
// longer pre-validates shard state itself, so the mapping is the contract.

func TestForceArchiveShard_UnknownShard_NotFound(t *testing.T) {
	srv := shardAdminServer(t, "shard-a", logdb.StateReadOnly)
	_, err := srv.ForceArchiveShard(context.Background(), &pb.ForceArchiveShardRequest{ShardId: "missing"})
	wantCode(t, err, codes.NotFound)
}

func TestForceArchiveShard_WrongState_FailedPrecondition(t *testing.T) {
	srv := shardAdminServer(t, "shard-a", logdb.StateIngesting)
	_, err := srv.ForceArchiveShard(context.Background(), &pb.ForceArchiveShardRequest{ShardId: "shard-a"})
	wantCode(t, err, codes.FailedPrecondition)
}

func TestDeleteArchivedShard_UnknownShard_NotFound(t *testing.T) {
	srv := shardAdminServer(t, "shard-a", logdb.StateArchived)
	_, err := srv.DeleteArchivedShard(context.Background(), &pb.DeleteArchivedShardRequest{ShardId: "missing"})
	wantCode(t, err, codes.NotFound)
}

func TestDeleteArchivedShard_WrongState_FailedPrecondition(t *testing.T) {
	srv := shardAdminServer(t, "shard-a", logdb.StateReadOnly)
	_, err := srv.DeleteArchivedShard(context.Background(), &pb.DeleteArchivedShardRequest{ShardId: "shard-a"})
	wantCode(t, err, codes.FailedPrecondition)
}

// RelaunchShard is the in-place binary-upgrade admin op; same error-mapping
// contract as ForceArchive (NotFound + FailedPrecondition).
func TestRelaunchShard_UnknownShard_NotFound(t *testing.T) {
	srv := shardAdminServer(t, "shard-a", logdb.StateIngesting)
	_, err := srv.RelaunchShard(context.Background(), &pb.RelaunchShardRequest{ShardId: "missing"})
	wantCode(t, err, codes.NotFound)
}

func TestRelaunchShard_WrongState_FailedPrecondition(t *testing.T) {
	// LAUNCHING is one of the rejected states (already coming up on the
	// current AMI; nothing to relaunch).
	srv := shardAdminServer(t, "shard-a", logdb.StateLaunching)
	_, err := srv.RelaunchShard(context.Background(), &pb.RelaunchShardRequest{ShardId: "shard-a"})
	wantCode(t, err, codes.FailedPrecondition)
}

