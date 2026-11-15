package logdb

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
	// pick whichever identifier it tracks: the EC2Provisioner uses id
	// (read from shard_records), the dev DockerLauncher uses addr. Either
	// may be empty if the caller doesn't have it.
	TerminateInstance(ctx context.Context, id, addr string) error
}

// The old optional-extension interfaces (asyncLauncher / TierScaler /
// RecoveryTerminator) and isLaunchingLauncher are gone — they served the
// LAUNCHING-row handoff + ASG desired-capacity sync the FSM replaced. The
// FSM's launch primitive is ShardLaunchAPI (provisioner_asg.go).
