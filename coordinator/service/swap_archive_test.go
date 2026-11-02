package service

import (
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/graybeardcorp/weft/logdb"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// SwapShardArchive is how a converted archive is swapped in
// (docs/plans/per-service-tables.md, "Archive layout"). Its logic is
// logdb.Provisioner.SwapArchive; these cover the RPC around it.

// A swap that finds the shard elsewhere is a precondition failure the operator
// can read, not an internal error.
func TestShardOpStatus_ArchivePathChangedIsAPreconditionFailure(t *testing.T) {
	err := shardOpStatus(fmt.Errorf("shard x is at y: %w", logdb.ErrArchivePathChanged))
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", got)
	}
}

func TestSwapShardArchive_RequiresTheAdminToken(t *testing.T) {
	target := startTestAdminServer(t, helperServer(t), "expected-token")
	client, closeConn := newAdminClient(t, target)
	defer closeConn()
	_, err := client.SwapShardArchive(ctxWithBearer(""), &pb.SwapShardArchiveRequest{ShardId: "shard-a", OldPath: "a", NewPath: "b"})
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Errorf("without a token: code = %v, want Unauthenticated", got)
	}
}

// helperServer's one shard is INGESTING: only an ARCHIVED shard can be swapped.
func TestSwapShardArchive_RefusesAShardThatIsNotArchived(t *testing.T) {
	target := startTestAdminServer(t, helperServer(t), "tok")
	client, closeConn := newAdminClient(t, target)
	defer closeConn()
	_, err := client.SwapShardArchive(ctxWithBearer("tok"), &pb.SwapShardArchiveRequest{ShardId: "shard-a", OldPath: "a", NewPath: "b"})
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Errorf("code = %v (%v), want FailedPrecondition", got, err)
	}
}

func TestSwapShardArchive_UnknownShard(t *testing.T) {
	target := startTestAdminServer(t, helperServer(t), "tok")
	client, closeConn := newAdminClient(t, target)
	defer closeConn()
	_, err := client.SwapShardArchive(ctxWithBearer("tok"), &pb.SwapShardArchiveRequest{ShardId: "nope", OldPath: "a", NewPath: "b"})
	if got := status.Code(err); got != codes.NotFound {
		t.Errorf("code = %v (%v), want NotFound", got, err)
	}
}

func TestSwapShardArchive_NeedsAllThreeFields(t *testing.T) {
	target := startTestAdminServer(t, helperServer(t), "tok")
	client, closeConn := newAdminClient(t, target)
	defer closeConn()
	for _, req := range []*pb.SwapShardArchiveRequest{
		{OldPath: "a", NewPath: "b"},
		{ShardId: "shard-a", NewPath: "b"},
		{ShardId: "shard-a", OldPath: "a"},
		{ShardId: "shard-a", OldPath: "a", NewPath: "a"}, // swapping to itself would tombstone the live archive
	} {
		_, err := client.SwapShardArchive(ctxWithBearer("tok"), req)
		if got := status.Code(err); got != codes.InvalidArgument {
			t.Errorf("%+v: code = %v (%v), want InvalidArgument", req, got, err)
		}
	}
}
