package coordinator

import "context"

// ShardLauncher abstracts shard instance lifecycle management.
// Production uses ASG, tests use in-process or Docker.
type ShardLauncher interface {
	// Launch requests a new shard instance of the given tier.
	// Returns the address (host:port) where the shard's gRPC server is reachable.
	Launch(ctx context.Context, tier string) (addr string, err error)

	// Terminate removes a shard instance.
	Terminate(ctx context.Context, addr string) error
}
