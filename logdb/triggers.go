package logdb

// Shard FSM triggers. Each constant names exactly one edge or one
// family of edges in the shard state machine (see
// docs/plans/asg-as-api-stateless.md for the table). All triggers
// are exported as `Trigger…` so call sites read uniformly.
//
// Heartbeat is the only trigger fired from outside the
// provisioner's tick — it lands on the heartbeat RPC path.
// Everything else is fired by either:
//   - the shard's own Evaluate() (autonomous: HeartbeatStale,
//     LaunchFailed, Stranded), or
//   - the bucket FSM (cascade-directed: Launch, RotateRequested,
//     ArchiveRequested).
type Trigger string

const (
	// Launch is fired by the bucket FSM when minting a fresh shard
	// (warming, scale-up, or replacement for a LAUNCHING→GONE shard
	// mid-cascade). Pre-condition: SM just created in (none) state.
	TriggerLaunch Trigger = "launch"

	// Heartbeat carries one store.ShardHeartbeat. Fired by the
	// heartbeat RPC handler for every heartbeat regardless of source
	// state; guards inside the SM pick the correct edge.
	TriggerHeartbeat Trigger = "heartbeat"

	// RotateRequested is fired by the bucket FSM on
	// ROTATING.OnEntry to flip an INGESTING shard to READ_ONLY. The
	// shard does not own this decision.
	TriggerRotateRequested Trigger = "rotate-requested"

	// ArchiveRequested is the bucket FSM's retention-sweep directive:
	// fired (fire-and-forget) on a READ_ONLY shard whose retention has
	// expired, moving it to ARCHIVING. The bucket owns the decision but
	// never waits — the export runs on the shard. The shard does not own
	// this decision.
	TriggerArchiveRequested Trigger = "archive-requested"

	// ArchiveComplete is fired by the shard's own Evaluate() once the
	// ARCHIVING work is fully done (data in S3, ColdShard swapped in,
	// volume retired, instance terminated). Moves ARCHIVING → ARCHIVED.
	// Shard-driven, like the heartbeat that completes LAUNCHING →
	// INGESTING: only the shard knows its export finished.
	TriggerArchiveComplete Trigger = "archive-complete"

	// HeartbeatStale is fired by the shard's Evaluate() when its
	// LastHeartbeatAt is older than the staleness threshold. Target
	// state depends on EBSVolumeID (SUSPENDED if non-empty, GONE if
	// empty).
	TriggerHeartbeatStale Trigger = "heartbeat-stale"

	// LaunchFailed is fired by the shard's Evaluate() when its
	// LAUNCHING phase has exhausted its retry budget.
	TriggerLaunchFailed Trigger = "launch-failed"

	// IngestDegraded is fired by an INGESTING shard's Evaluate() when its
	// consecutive ingest-failure count has crossed SuspendFailureThreshold.
	// Moves INGESTING → DEGRADED, pulling the shard out of write routing
	// while leaving it read-queryable.
	TriggerIngestDegraded Trigger = "ingest-degraded"

	// IngestRecovered is fired by a DEGRADED shard's Evaluate() once the
	// breaker cooldown has elapsed. Moves DEGRADED → INGESTING (half-open),
	// resetting the failure count so writes get re-admitted; a fresh run of
	// failures degrades it again.
	TriggerIngestRecovered Trigger = "ingest-recovered"

	// Stranded is fired by the shard's Evaluate() when SUSPENDED for
	// long enough AND the volume has gone missing or been re-tagged
	// (the "reapStrandedSuspended" case in the old reconciler).
	TriggerStranded Trigger = "stranded"

	// Relaunch is fired by the shard's Evaluate() once SUSPENDED has
	// reaped the dead instance (Terminate + clear InstanceID). It hands
	// off to LAUNCHING, which relaunches a replacement reattaching the
	// surviving volume (EBSVolumeID is still set). Splitting reap
	// (SUSPENDED) from relaunch (LAUNCHING) keeps InstanceID
	// unambiguous: in SUSPENDED it's always the dead instance, in
	// LAUNCHING always empty-or-replacement.
	TriggerRelaunch Trigger = "relaunch"

	// AdminDelete is fired by operator action on an ARCHIVED shard.
	// The only edge out of ARCHIVED.
	TriggerAdminDelete Trigger = "admin-delete"
)

// Bucket FSM triggers. Each names a cascade-advancing edge in the
// bucket SM. The bucket FSM is driven entirely by its own
// Evaluate() — no external fires.
const (
	// RotationNeeded is fired when the bucket is in STASIS and some
	// INGESTING shard is within PrewarmLead of its WindowEnd.
	TriggerRotationNeeded Trigger = "rotation-needed"

	// ScaleUpRequested is fired when target > current INGESTING.
	// Reuses WARMING — same shape (mint shards, wait for INGESTING).
	TriggerScaleUpRequested Trigger = "scale-up-requested"

	// ScaleDownRequested is fired when target < current INGESTING.
	TriggerScaleDownRequested Trigger = "scale-down-requested"

	// AllReplacementsReady is fired when every incoming shard
	// (window_start = nextWindowStart) is INGESTING. Gates
	// WARMING → ROTATING.
	TriggerAllReplacementsReady Trigger = "all-replacements-ready"

	// OldShardsRotated is fired when every outgoing shard (the rotate
	// cohort) has reached READ_ONLY. Gates ROTATING → STASIS and clears
	// cascade_started_at. (Archival is not part of the cascade — it's a
	// fire-and-forget retention sweep, so there's no ARCHIVING bucket
	// phase to gate into.)
	TriggerOldShardsRotated Trigger = "old-shards-rotated"

	// Settled is fired when SCALING_DOWN's excess shards have all
	// rotated to READ_ONLY. They flow through the normal cascade
	// path on subsequent ticks.
	TriggerSettled Trigger = "settled"
)
