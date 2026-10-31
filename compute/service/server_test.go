package service

import (
	"context"
	"errors"
	"net"
	"testing"

	"google.golang.org/grpc"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/compute/computetest"
	"github.com/graybeardcorp/weft/planner"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
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

// badRowShard answers every query with one row of a keyed sum whose shard saw a
// value it could not add: the aggregate cannot be shown exact.
type badRowShard struct{ shard.Shard }

func (badRowShard) ID() string { return "bad" }
func (badRowShard) QueryToSink(_ context.Context, _ string, _ []any, sink shard.RowSink) {
	sink.SendRow(&pb.ResultRow{Raw: "x", MetaVars: map[string]string{
		planner.CountColumn: "1", planner.SumColumn: "1", planner.AbsSumColumn: "1", planner.BadColumn: "1",
	}}, "bad")
	sink.SendSentinel("bad")
}

// An aggregate the shards could not make exact reaches the coordinator as an
// InexactAggregateError, which it answers by running the base plan; as an
// error string it would be a failure the user sees.
func TestExecuteQueryStream_InexactAggregateCrossesTheWire(t *testing.T) {
	engine := &compute.Engine{}
	engine.RegisterResolver(computetest.NewInProcessResolver("bad", badRowShard{}))
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	pb.RegisterWeftComputeServer(srv, NewServer(engine))
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	client, err := compute.NewClient(lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })

	stream, err := client.ExecuteQuery(context.Background(), &pb.ExecuteQueryRequest{
		Sql: "SELECT 1", MergeKey: "raw", Shards: []*pb.ShardDescriptor{{ShardId: "bad"}},
		Group: &pb.GroupSpec{Render: pb.GroupRender_GROUP_RENDER_KEY_SUM},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = stream.Next(context.Background())
	var inexact *compute.InexactAggregateError
	if !errors.As(err, &inexact) {
		t.Fatalf("stream failed with %v (%T), want an InexactAggregateError", err, err)
	}
}
