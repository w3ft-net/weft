package logdb

// ShardState is the persisted lifecycle state of a shard, written by
// the shard FSM's external-storage mutator (a narrow state-column
// UPDATE) and read back through the stateless ExternalStorage
// accessor.
//
// The six states divide cleanly: LAUNCHING/INGESTING/READ_ONLY are
// the "healthy lifecycle" line; SUSPENDED is the recoverable-failure
// branch; ARCHIVED is terminal-for-data (data in S3, instance gone)
// but still query-serving via a ColdShard; GONE is the cleanup sink
// for launch failures, data-already-lost cases, and admin deletes.
// EXPIRING — declared but never written under the prior reconciler-
// driven model — is gone.
//
// See docs/plans/asg-as-api-stateless.md for the full transition
// table.
type ShardState string

const (
	StateLaunching ShardState = "LAUNCHING"
	StateIngesting ShardState = "INGESTING"
	StateReadOnly  ShardState = "READ_ONLY"
	StateSuspended ShardState = "SUSPENDED"
	StateArchived  ShardState = "ARCHIVED"
	StateGone      ShardState = "GONE"
)

// BucketState is the persisted state of a bucket, mirroring
// ShardState in shape (string constants, written via the FSM's
// ExternalStorage). Buckets are coarser-grained than shards: their
// state communicates whether a cascade is in flight and which phase.
type BucketState string

const (
	BucketStasis       BucketState = "STASIS"
	BucketWarming      BucketState = "WARMING"
	BucketRotating     BucketState = "ROTATING"
	BucketArchiving    BucketState = "ARCHIVING"
	BucketScalingDown  BucketState = "SCALING_DOWN"
)
