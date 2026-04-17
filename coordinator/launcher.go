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

	// TerminateInstance removes a shard instance. Both id (cloud
	// instance id) and addr (host:port) are passed so each launcher can
	// pick whichever identifier it tracks: ASGLauncher prefers id (works
	// for shards adopted from heartbeats that were never seen by
	// LaunchInstance), LocalLauncher uses addr. Either may be empty if
	// the caller doesn't have it.
	TerminateInstance(ctx context.Context, id, addr string) error
}
