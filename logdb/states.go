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
// UPGRADING is the operator-driven sibling of SUSPENDED: same
// reap-then-relaunch recovery logic, but no heartbeat-promotion edge.
// RelaunchShard fires TriggerRelaunchRequested against a HEALTHY shard
// whose process keeps heartbeating — landing in SUSPENDED would race
// the reap and silently no-op the roll. UPGRADING has no heartbeat
// permit at all, so the only exit is TriggerRelaunch (post-reap) →
// LAUNCHING or TriggerStranded → GONE. HeartbeatStale still routes to
// SUSPENDED, preserving its transient-blip rescue behavior.
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
	StateUpgrading ShardState = "UPGRADING"
	StateArchived  ShardState = "ARCHIVED"
	StateGone      ShardState = "GONE"
	// StateDegraded is an alive shard whose ingest is persistently failing
	// (consecutive failures crossed SuspendFailureThreshold). It's a
	// circuit-breaker pause on INGESTING: excluded from write routing
	// (ingestCandidate requires INGESTING) but still read-queryable (its
	// data is intact on a live instance) and still counted as part of the
	// bucket's live cohort so the breaker window doesn't churn a
	// replacement. Evaluate re-admits it to INGESTING after a cooldown
	// (half-open); a re-failure degrades it again, and a dead instance
	// drops it to SUSPENDED like any INGESTING shard. Replaces the removed
	// one-way Suspended ingest latch.
	StateDegraded ShardState = "DEGRADED"
)

// UpgradePhase is where an UPGRADING shard is in the move of its volume to
// a warm-pool spare (docs/plans/shard-seamless-upgrade.md). UPGRADING is a
// serving state: routing never leaves the shard, and calls that arrive
// while its volume is moving are held by the shard's gate and the client.
type UpgradePhase string

const (
	// PhaseClaiming: a spare is being claimed (or launched on demand) and
	// awaited; the old instance serves normally.
	PhaseClaiming UpgradePhase = "claiming"
	// PhasePausing: the old instance refuses new calls with a redirect to
	// the spare and drains, then closes DuckDB and unmounts.
	PhasePausing UpgradePhase = "pausing"
	// PhaseSwapping: the volume moves to the spare, which mounts it and
	// opens DuckDB.
	PhaseSwapping UpgradePhase = "swapping"
	// PhaseResumed: the spare serves the shard. The demoted instance keeps
	// redirecting until every coordinator routes to the new address and the
	// pool's replacement spare heartbeats, then it is terminated.
	PhaseResumed UpgradePhase = "resumed"
	// PhaseRollingBack: the spare never readied; the volume goes back to
	// the demoted instance, which remounts and resumes.
	PhaseRollingBack UpgradePhase = "rolling_back"
)

// BucketState is the persisted state of a bucket, mirroring
// ShardState in shape (string constants, written via the FSM's
// ExternalStorage). Buckets are coarser-grained than shards: their
// state communicates whether a cascade is in flight and which phase.
type BucketState string

// No ARCHIVING bucket state: archival is a shard concern (the shard's
// ARCHIVING state) that the bucket only *fires* via a fire-and-forget
// retention sweep — it never waits on it, so there's no gate and thus no
// state. The bucket cascade is purely the rotation that must never gap
// the hot window. See docs/plans/asg-as-api-stateless.md (FSM redesign
// 2026-05-25).
const (
	BucketStasis      BucketState = "STASIS"
	BucketWarming     BucketState = "WARMING"
	BucketRotating    BucketState = "ROTATING"
	BucketScalingDown BucketState = "SCALING_DOWN"
)
