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
	// pick whichever identifier it tracks: ASGLauncher prefers id (works
	// for shards adopted from heartbeats that were never seen by
	// LaunchInstance), LocalLauncher uses addr. Either may be empty if
	// the caller doesn't have it.
	TerminateInstance(ctx context.Context, id, addr string) error
}

// asyncLauncher is the optional extension launchers can implement to
// declare that LaunchInstance is non-blocking — the instance comes up
// out-of-band (e.g., AWS ASG via lifecycle hook). Launchers that
// satisfy this interface and return true get a LAUNCHING row INSERTed
// before LaunchInstance is called; the lifecycle hook claims the row
// when the instance enters Pending:Wait; heartbeats from the instance
// promote it to INGESTING. Synchronous launchers (LocalLauncher,
// in-process test launchers) don't implement the interface — their
// LaunchInstance returns only when the shard is ready to Configure.
type asyncLauncher interface {
	UsesLaunchingRows() bool
}

// TierScaler is the optional extension async launchers implement so
// the coordinator can set a tier's ASG desired_capacity in absolute
// terms (rather than bumping +1 per launch and never scaling down).
// SetDesiredCapacity runs once per tick per tier from the
// provisioner's post-reconcile sync pass; TierMaxSize lets the sync
// clamp computed desired against the ASG's MaxSize.
type TierScaler interface {
	SetDesiredCapacity(ctx context.Context, tier string, desired int32) error
	TierMaxSize(ctx context.Context, tier string) (int32, error)
}

// RecoveryTerminator is the optional extension async launchers
// implement so the coordinator can explicitly terminate a dead
// instance while keeping the ASG's desired_capacity the same,
// prompting the ASG to launch a replacement. This is the
// failure-recovery counterpart to TerminateInstance (which uses
// ShouldDecrementDesiredCapacity=true, for archive-then-reclaim).
// Called from reapDeadShards when a shard's heartbeat is stale
// AND it has a bound EBS volume — the data survives on the
// volume, the instance doesn't.
type RecoveryTerminator interface {
	TerminateForReplacement(ctx context.Context, instanceID string) error
}

// isLaunchingLauncher reports whether this launcher participates in
// the LAUNCHING-row handoff. Used by the reconciler to pick between
// the INSERT-then-call-ASG path and the synchronous
// launch-and-configure path.
func isLaunchingLauncher(l ShardLauncher) bool {
	if al, ok := l.(asyncLauncher); ok {
		return al.UsesLaunchingRows()
	}
	return false
}
