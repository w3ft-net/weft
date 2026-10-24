package service

import (
	"context"
	"errors"
	"net"
	"testing"

	"google.golang.org/grpc"

	"github.com/graybeardcorp/weft/compute"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// A shard failure on the compute worker must reach the coordinator as a
// MissingRowsError naming the shard, not as an opaque error string: the
// coordinator fails the whole run on it.
func TestExecuteQueryStream_ShardFailureCrossesTheWire(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	pb.RegisterWeftComputeServer(srv, NewServer(&compute.Engine{}))
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	client, err := compute.NewClient(lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })

	// A descriptor with neither a gRPC address nor a Lambda URL can't be
	// resolved, so the shard's rows never arrive.
	stream, err := client.ExecuteQuery(context.Background(), &pb.ExecuteQueryRequest{
		Sql:    "SELECT 1",
		Shards: []*pb.ShardDescriptor{{ShardId: "shard-unreachable"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, ok, err := stream.Next(context.Background())
		if err != nil {
			var missing *compute.MissingRowsError
			if !errors.As(err, &missing) || missing.ShardID != "shard-unreachable" {
				t.Fatalf("stream failed with %v (%T), want a MissingRowsError for shard-unreachable", err, err)
			}
			return
		}
		if !ok {
			t.Fatal("the stream ended cleanly although shard-unreachable's rows never arrived")
		}
	}
}
