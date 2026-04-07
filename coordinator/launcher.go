package coordinator

import (
	"context"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// ShardLauncher abstracts shard instance lifecycle management.
// Every implementation handles the full lifecycle: start instance,
// connect via gRPC, call Configure RPC, return the address.
// Production uses ASG, tests use in-process or Docker.
type ShardLauncher interface {
	// Launch starts a new shard instance, configures it via the Configure
	// RPC, and returns the address (host:port) where it's reachable.
	Launch(ctx context.Context, tier string, cfg *pb.ConfigureRequest) (addr string, err error)

	// Terminate removes a shard instance.
	Terminate(ctx context.Context, addr string) error
}
