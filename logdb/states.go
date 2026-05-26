package logdb

// ShardState is the persisted lifecycle state of a shard, written by
// the shard FSM's external-storage mutator (a narrow state-column
// UPDATE) and read back through the stateless ExternalStorage
// accessor.
//
// The states divide cleanly: LAUNCHING/INGESTING/READ_ONLY are the
// "healthy lifecycle" line; ARCHIVING is READ_ONLY's tail — the
// export-in-progress phase, query-routable just like READ_ONLY (it
// keeps serving from the live DuckDB until the export swaps in a
// ColdShard); SUSPENDED is the recoverable-failure branch; ARCHIVED
// is terminal-for-data (data in S3, instance + volume gone) but still
// query-serving via a ColdShard; GONE is the cleanup sink for launch
// failures, data-already-lost cases, and admin deletes. EXPIRING —
// declared but never written under the prior reconciler-driven model
// — is gone.
//
// ARCHIVING is conceptually a substate of READ_ONLY but is NOT a
// stateless SubstateOf: it must not inherit READ_ONLY's
// HeartbeatStale → SUSPENDED edge (its instance is being torn down by
// design). The "substate" relationship lives at the query-routing
// layer instead — MatchingEntries treats ARCHIVING exactly like
// READ_ONLY.
//
// See docs/plans/asg-as-api-stateless.md for the full transition
// table.
type ShardState string

const (
	StateLaunching ShardState = "LAUNCHING"
	StateIngesting ShardState = "INGESTING"
	StateReadOnly  ShardState = "READ_ONLY"
	StateArchiving ShardState = "ARCHIVING"
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
	BucketStasis      BucketState = "STASIS"
	BucketWarming     BucketState = "WARMING"
	BucketRotating    BucketState = "ROTATING"
	BucketArchiving   BucketState = "ARCHIVING"
	BucketScalingDown BucketState = "SCALING_DOWN"
)
