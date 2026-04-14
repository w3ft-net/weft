package coordinator

import "context"

// ShardLauncher abstracts shard instance lifecycle.
// Implementations only manage infrastructure — starting and stopping
// instances. The provisioner handles configuration (Configure RPC),
// health checks, and retry logic.
type ShardLauncher interface {
	// LaunchInstance starts a new shard instance and returns the
	// gRPC address (host:port) where it will be reachable.
	LaunchInstance(ctx context.Context, tier string) (addr string, err error)

	// TerminateInstance removes a shard instance.
	TerminateInstance(ctx context.Context, addr string) error
}
