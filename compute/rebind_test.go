package compute

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

type namedShardServer struct {
	pb.UnimplementedShardQueryServer
	name string
}

func (s *namedShardServer) Query(_ *pb.QueryRequest, stream grpc.ServerStreamingServer[pb.ResultRow]) error {
	return stream.Send(&pb.ResultRow{Raw: s.name})
}

func startNamedShard(t *testing.T, name string) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	gs := grpc.NewServer()
	pb.RegisterShardQueryServer(gs, &namedShardServer{name: name})
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)
	return lis.Addr().String()
}

// A shard's volume moving to another instance changes the address in the
// descriptors the engine is handed. A query that resolved its handle just
// before that must still be able to run on it: closing the handle on rebind
// fails that query with "the client connection is closing".
func TestEngine_RebindLeavesHandleResolvedBeforeItUsable(t *testing.T) {
	oldAddr := startNamedShard(t, "old")
	newAddr := startNamedShard(t, "new")
	e := &Engine{}
	ctx := context.Background()

	before, err := e.resolveShard(ctx, &pb.ShardDescriptor{ShardId: "s1", GrpcAddr: oldAddr})
	if err != nil {
		t.Fatalf("resolve old: %v", err)
	}
	after, err := e.resolveShard(ctx, &pb.ShardDescriptor{ShardId: "s1", GrpcAddr: newAddr})
	if err != nil {
		t.Fatalf("resolve new: %v", err)
	}

	if _, err := before.Query(ctx, "SELECT 1"); err != nil {
		t.Errorf("handle resolved before the rebind failed after it: %v", err)
	}
	rows, err := after.Query(ctx, "SELECT 1")
	if err != nil || len(rows) != 1 || rows[0].Raw != "new" {
		t.Errorf("after rebind: rows=%v err=%v, want the new instance's row", rows, err)
	}
}
