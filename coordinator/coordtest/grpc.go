//go:build test

package coordtest

import (
	"net"
	"testing"

	"google.golang.org/grpc"

	"github.com/w3ft-net/weft/compute"
	computesvc "github.com/w3ft-net/weft/compute/service"
	"github.com/w3ft-net/weft/coordinator"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shard"
	shardsvc "github.com/w3ft-net/weft/shard/service"
)

// GRPCQueries runs a few of the log queries again with the shards and compute
// behind gRPC servers, as in production: the coordinator reaches each shard
// through a client and sends every query to a compute client.
func GRPCQueries(t *testing.T, newSubject func(t *testing.T) Subject) {
	for _, c := range []struct {
		name string
		run  func(*testing.T, *queryFixture)
	}{
		{"AwkLevelFilter", queryAwkLevelFilter},
		{"GrepTimeout", queryGrepTimeout},
		{"JqSelectStatus500", queryJqSelectStatus500},
		{"HeadLimit", queryHeadLimit},
		{"TenantIsolation", queryTenantIsolation},
		{"ComputeSort", querySort},
		{"ComputeWcCount", queryWcCount},
		{"TimeRangeScopingShard0Only", queryShard0Only},
	} {
		t.Run(c.name, func(t *testing.T) {
			pool, err := compute.NewClient(serve(t, func(srv *grpc.Server) {
				pb.RegisterWeftComputeServer(srv, computesvc.NewServer(&compute.Engine{}))
			}))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { pool.Close() })

			f := newBareFixture(t, newSubject(t), coordinator.CoordinatorConfig{Compute: pool})
			f.seedShards(t, func(t *testing.T, live *shard.DuckDBShard) (shard.Shard, string) {
				addr := serve(t, func(srv *grpc.Server) {
					pb.RegisterShardQueryServer(srv, shardsvc.NewServer(live))
				})
				client, err := shard.NewGRPCShard(live.ID(), addr, "")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { client.Close() })
				return client, addr
			})
			c.run(t, f)
		})
	}
}

// serve starts a gRPC server on a free local port, registered by register, and
// returns its address.
func serve(t *testing.T, register func(*grpc.Server)) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	register(srv)
	go srv.Serve(lis)
	t.Cleanup(srv.GracefulStop)
	return lis.Addr().String()
}
