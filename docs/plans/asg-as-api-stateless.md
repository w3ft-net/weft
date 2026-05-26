# ASG-as-API + stateless shard/bucket FSMs

Replace the current ASG autoscaling + SNS/SQS lifecycle plumbing with a
provisioner that drives instance launches directly through the ASG
APIs, with shard and bucket lifecycles modeled as `qmuntal/stateless`
state machines persisted to postgres. The bucket FSM owns the
cascade (warming → rotating → archiving → stasis); the shard FSM
owns its autonomous failure handling.

## Status

Phase 1 partially shipped (commit `55248cb` on branch
`the_big_lift`). Detail in the "Progress" subsection at the end of
"Phased rollout"; pick up from Phase 2 next session.

## Why

The current shard-management code in `logdb/` has accreted around the
ASG lifecycle hook → SNS → SQS handoff. Five tangled concerns share
the same surface:

- `LAUNCHING` shards live in the DB but not the registry. The
  reconciler counts them from one source and `INGESTING` from
  another. Comments in `reconciler.go` and `provisioner.go` flag this
  as the asymmetry that caused two distinct prod incidents.
- Shard state transitions are split between the reconciler ops, the
  heartbeat path's `christen`/rebind branches, three `reap*` methods,
  and the lifecycle handler's `decide()`. No single place owns
  "INGESTING → READ_ONLY"; each state has 2–3 mutators.
- The lifecycle hook adds an SQS message bus to an otherwise
  synchronous control plane. The handler isn't idempotent against
  SQS at-least-once delivery, has no DLQ, and deletes messages on
  transient errors — converting recoverable AWS hiccups into
  `ABANDON` decisions that destroy work.
- Shard rotation isn't gated by the bucket. Each shard's reconciler
  op checks "is the bucket's INGESTING count ≥ Target?" but there's
  no top-down coordinator that knows "the cascade is mid-flight,
  hold all rotations." A slow replacement launch produces a
  write-gap rather than extending the outgoing shard's WindowEnd.
- `ShardState` declares an `EXPIRING` value that no code writes,
  and a `Suspended` boolean that overlaps `StateSuspended` and isn't
  persisted.

The system is broken in ways that fall out of these mismatches —
restarts orphan volumes, refresh storms ABANDON live shards,
rotation races leave write-gaps. We have no customers and no
backward-compat constraint, so this is the moment to do the
restructure cleanly rather than patch around it.

## Architecture

```
Bucket SM (one per bucket; persisted in buckets.state)
   STASIS  ─────────────── RotationNeeded ──────────────▶  WARMING
                                                              │
                                                AllReplacementsReady
                                                              │
                                                              ▼
   STASIS  ◀──── ArchivalsComplete ─── ARCHIVING ◀── OldShardsRotated ─── ROTATING
                                                              ▲
                                            (or)  ScaleDownRequested
                                                              │
                                              SCALING_DOWN ───┘ (joins back via Settled)


Shard SM (one per shard; persisted in shard_records.state)
                              ┌──────────────────────────────┐
                              ▼                              │
   (new) ──Launch──▶ LAUNCHING ──Heartbeat──▶ INGESTING ──RotateRequested──▶ READ_ONLY
                         │                       │                                  │
                    LaunchFailed         HeartbeatStale                     ArchiveRequested
                         │                       │                                  │
                         ▼                       ▼                                  ▼
                       GONE                SUSPENDED                            ARCHIVED  (terminal)
                                                │  ▲
                                          Stranded │
                                                │  └─ Heartbeat (rebind)
                                                ▼
                                              GONE
```

Hierarchy: bucket SMs *direct* cascade-driven shard transitions
(`RotateRequested`, `ArchiveRequested`) by firing triggers on child
shard SMs. Shard SMs *autonomously* handle failures
(`HeartbeatStale`, `LaunchFailed`, `Stranded`). The bucket SM never
fires `Launch` directly on an existing shard — it mints new shard
SMs whose `LAUNCHING.OnEntry` action initiates the EC2 work.

Self-heal: every reconciler tick calls `bucket.Evaluate(ctx)` per
bucket and `shard.Evaluate(ctx)` per shard. Evaluate reads the
current persisted state, observes reality (registry + DB + EC2),
and fires whatever triggers are needed to converge. Restart drops
straight into Evaluate — there is no boot vs. running mode.

ASGs remain as the launch API, but autoscaling, scaling policies,
and lifecycle hooks are off. EC2 health checks are off too —
the provisioner owns every launch and every termination
explicitly. Each tier gets one ASG per AZ (3 ASGs per tier) so
replacement launches can be pinned to the AZ of an existing EBS
volume, and so the provisioner can deliberately stripe a bucket's
shards across multiple AZs.

The set of available per-AZ ASGs is fixed in TF (one per
configured AZ per tier), exposed to the provisioner via the
`SHARD_<TIER>_ASGS` config var, and treated as immutable by the
runtime — the provisioner only ever picks from this list, never
mints new ASGs.

## State machine details

### Shard SM

States — six (`EXPIRING` retired; `GONE` added as terminal sink for
non-archive paths):

| State | Persisted | Registry entry | Notes |
|---|---|---|---|
| `LAUNCHING` | yes | yes (Addr="") | RunInstances/ASG-bump initiated; awaiting first heartbeat |
| `INGESTING` | yes | yes | Accepting writes; serves queries |
| `READ_ONLY` | yes | yes | Past WindowEnd (by bucket direction); query-only |
| `SUSPENDED` | yes | yes | Instance dead, volume intact, replacement being launched |
| `ARCHIVED` | yes | yes | Data in S3 + ColdShard handle; instance terminated, volume retired. **Terminal**. |
| `GONE` | no (row deleted) | no | Terminal sink for failed launches and stranded shards |

Triggers:

| Trigger | Permitted from | Permitted to | Guard |
|---|---|---|---|
| `Launch` | (new SM) | `LAUNCHING` | — |
| `Heartbeat(hb)` | `LAUNCHING` | `INGESTING` | hb.InstanceID matches entry; Configure succeeds |
| `Heartbeat(hb)` | `SUSPENDED` | `INGESTING` | hb.ShardID matches; rebind dial succeeds |
| `RotateRequested` | `INGESTING` | `READ_ONLY` | — (bucket SM owns the gate) |
| `ArchiveRequested` | `READ_ONLY` | `ARCHIVED` | — (bucket SM owns the gate) |
| `HeartbeatStale` | `INGESTING`, `READ_ONLY` | `SUSPENDED` | `entry.EBSVolumeID != ""` |
| `HeartbeatStale` | `INGESTING`, `READ_ONLY` | `GONE` | `entry.EBSVolumeID == ""` |
| `LaunchFailed` | `LAUNCHING` | `GONE` | retry budget exhausted |
| `Stranded` | `SUSPENDED` | `GONE` | volume gone or re-tagged |
| `AdminDelete` | `ARCHIVED` | `GONE` | operator-initiated only |

On-entry actions:

- `LAUNCHING.OnEntry`: takes `spec{tier, az, stripeIndex, existingVolumeID}` (the bucket SM supplies AZ via the striper; recovery paths supply `existingVolumeID`). Per-(tier,AZ) mutex; `TerminateInstanceInAutoScalingGroup(prior_id, decrement=true)` if recovery; `SetDesiredCapacity(asgForAZ, +1)`; poll `DescribeAutoScalingGroups` for new InstanceID; `CreateTags(instance, shard_id)`; create-or-find volume in `az`; `AttachVolume`. Persists `entry.InstanceID`, `entry.AZ`, `entry.StripeIndex`; `entry.Addr` stays "" until heartbeat fills it.
- `INGESTING.OnEntryFrom(Heartbeat-from-LAUNCHING)`: dial gRPC, call `Configure` RPC with shard_id, apps (from bucket), window, heartbeat token. Persist.
- `INGESTING.OnEntryFrom(Heartbeat-from-SUSPENDED)`: dial gRPC at new Addr, swap `entry.Shard`, schedule deferred close of old conn. Persist.
- `READ_ONLY.OnEntry`: persist state change. No side effects (the shard keeps serving queries; bucket may extend WindowEnd before firing this).
- `SUSPENDED.OnEntry`: clear `entry.InstanceID`/`entry.Addr`/`entry.Shard`; persist; kick off async replacement launch (same launch path as `LAUNCHING.OnEntry`, but recovery branch).
- `ARCHIVED.OnEntry`: shard.Archive (export to S3); swap `entry.Shard` to ColdShard; `volumes.SnapshotVolume → DetachVolume → DeleteVolume`; `launcher.Terminate(instance_id)`. Each step idempotent and re-checks; failures leave the entry mid-cleanup for next tick to retry.
- `GONE.OnEntry`: best-effort `launcher.Terminate`; `registry.Remove`; `store.DeleteShard`.

### Bucket SM

States:

| State | Meaning | Gate to advance |
|---|---|---|
| `STASIS` | Target shards INGESTING in current window; no rotation | window approaching OR target changed |
| `WARMING` | New shards minted; awaiting INGESTING | every incoming shard INGESTING |
| `ROTATING` | Flipping outgoing shards to READ_ONLY | every outgoing shard READ_ONLY |
| `ARCHIVING` | Archiving outgoing shards | every outgoing shard ARCHIVED |
| `SCALING_DOWN` | Target dropped below current INGESTING; rotating excess | excess shards now READ_ONLY |

Triggers:

| Trigger | From | To |
|---|---|---|
| `RotationNeeded` | `STASIS` | `WARMING` |
| `ScaleUpRequested` | `STASIS` | `WARMING` |
| `ScaleDownRequested` | `STASIS` | `SCALING_DOWN` |
| `AllReplacementsReady` | `WARMING` | `ROTATING` |
| `OldShardsRotated` | `ROTATING` | `ARCHIVING` |
| `ArchivalsComplete` | `ARCHIVING` | `STASIS` |
| `Settled` | `SCALING_DOWN` | `STASIS` |

On-entry actions:

- `WARMING.OnEntry`: set `buckets.cascade_started_at = now()`; compute next-window bounds; for each outgoing shard, mint one incoming shard inheriting its `stripe_index` and AZ via `StripeAZ(tier, stripe_index)`; each new shard SM fires its own `Launch` trigger with `spec{tier, az, stripeIndex}`. Idempotent: re-runs skip stripe indices already covered by an existing incoming shard for this cascade.
- `WARMING.OnTick`: re-check `AllReplacementsReady`; if cascade has been in WARMING longer than `cascadeStallThreshold` and any incoming shard is still LAUNCHING, extend WindowEnd on every outgoing shard by `extensionStep` (avoids write-gap); after `maxExtensions`, log alert and proceed anyway.
- `ROTATING.OnEntry`: for each outgoing shard, `shardSM.Fire(RotateRequested)`.
- `ARCHIVING.OnEntry`: for each outgoing shard (now READ_ONLY), `shardSM.Fire(ArchiveRequested)`. The shard's `ARCHIVED.OnEntry` does the actual archive work; this is non-blocking from the bucket's POV.
- `STASIS.OnEntry`: clear `buckets.cascade_started_at`.
- `SCALING_DOWN.OnEntry`: pick the N youngest INGESTING shards (N = current − target); `shardSM.Fire(RotateRequested)` on each. They flow through normal `READ_ONLY → ARCHIVED` on a subsequent cycle.

### AZ striping

A bucket's shards must be spread across the configured AZs so an
AZ outage doesn't sink an entire bucket. The striping decision
lives in the bucket SM and is consulted whenever a fresh shard
needs an AZ (WARMING for new windows, SCALING_UP, or when
the bucket replaces a `LAUNCHING → GONE` shard mid-cascade).

The algorithm is intentionally simple: for the bucket's `Target`
shards and the configured AZ list `[az-a, az-b, az-c]`, the
shard at stripe index `i` lands in AZ `i % len(azs)`. Stripe
indices are stable per cascade — when a cascade mints `Target`
incoming shards, each gets the stripe index of the outgoing
shard it's replacing (so the next-window cohort mirrors the
current cohort's AZ layout). For the initial fill of a new
bucket, indices are assigned 0..Target-1 in order.

Recovery launches (`SUSPENDED.OnEntry`) **do not** consult the
striper — their AZ is pinned to the volume's AZ, which is
persisted on `shard_records.az` (added in migration 030). The
volume tag remains as a backstop but isn't the primary source.

What this design deliberately doesn't try to do:

- **AZ-aware failover.** If `LaunchFresh(tier, "az-a")` fails
  with "InsufficientInstanceCapacity," the launch retries in
  the same AZ rather than picking a different one. A bucket
  carrying a launch failure for several ticks is an operator
  signal, not a self-heal case — picking a different AZ would
  silently rebalance the stripe and hide the underlying
  capacity issue.
- **Re-balancing existing shards.** If TF expands the AZ list
  from 2 to 3 AZs, existing shards stay where they are. Only
  newly-minted shards use the wider distribution. Re-balancing
  is a follow-up plan; `az-resilience.md` is the home for it.

The striper is exposed by `ASGProvisioner` as
`AZsForTier(tier) []string` plus a pure helper
`StripeAZ(tier string, index int) string`. The bucket SM holds
no AZ knowledge of its own beyond the stripe index it assigns
each shard.

### Cascade context — 3NF, not JSONB

The bucket doesn't store a list of "incoming"/"outgoing" shard IDs.
Both are derived from `shard_records` via indexed queries:

- **incoming** = `bucket_id = X AND window_start = $next_window_start AND state IN ('LAUNCHING', 'INGESTING')`
- **outgoing** = `bucket_id = X AND window_start < $current_window_start AND state IN ('INGESTING', 'READ_ONLY')`

The only piece of cascade context that isn't already a fact about a
specific shard is "when did the current cascade start" (needed for
the WindowEnd-extension policy), which is one scalar column on
`buckets`. New columns:

```sql
ALTER TABLE buckets
  ADD COLUMN state TEXT NOT NULL DEFAULT 'STASIS',
  ADD COLUMN cascade_started_at TIMESTAMPTZ NULL;

ALTER TABLE shard_records
  ADD COLUMN ingest_failures INT NOT NULL DEFAULT 0,
  ADD COLUMN ingest_unhealthy BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN last_heartbeat_at TIMESTAMPTZ NULL,
  ADD COLUMN launch_attempts INT NOT NULL DEFAULT 0,
  ADD COLUMN az TEXT NULL,
  ADD COLUMN stripe_index INT NULL;

CREATE INDEX idx_shard_records_bucket_window
  ON shard_records (bucket_id, window_start);
```

Persisting `last_heartbeat_at`, `ingest_failures`, `ingest_unhealthy`
fixes the existing "amnesia after restart" footgun (these were
in-memory only).

## Failure walkthrough: lose compute, keep volume

End-to-end trace to validate the design works for the most common
failure mode.

**T=0** — Steady state. Bucket `default` (target=2) in STASIS.
`s-A` INGESTING on instance `i-A` with `vol-A` in `us-east-2a`.
`s-B` INGESTING on `i-B` with `vol-B` in `us-east-2b`. ASGs:
`small-az-a` desired=1 (holds i-A), `small-az-b` desired=1
(holds i-B), `small-az-c` desired=0. EC2 health checks off.

**T=0:30** — i-A kernel-panics. EC2 terminates it. `vol-A`
auto-detaches (state=available, tag `shard_id=s-A` intact, in az-a).
Heartbeats from i-A cease.

**T=2:30** — Reaper tick. `s-A.Evaluate()` sees `state=INGESTING` and
`now - LastHeartbeatAt > 2min`. Fires `HeartbeatStale`. Guard:
`EBSVolumeID == "vol-A"` (non-empty). Target: `SUSPENDED`.
`SUSPENDED.OnEntry`:

1. Clear `entry.InstanceID`, `entry.Addr`, `entry.Shard`. Persist.
2. Resolve recovery launch: AZ from `entry.AZ` (`us-east-2a`,
   persisted at original LAUNCHING), tier from `entry.Tier`
   (`small`), ASG = `small-az-a`.
3. Kick off async launch via `ASGProvisioner.LaunchRecovery(spec{
   tier:"small", az:"a", existingVolume:"vol-A"})`.

**T=2:30 → T=3:30** — Recovery launch:

1. Acquire per-(small, az-a) mutex.
2. `TerminateInstanceInAutoScalingGroup(i-A, decrement=true)` — clears
   ASG bookkeeping. AWS tolerates already-terminated instance.
3. `SetDesiredCapacity(small-az-a, 1)`. ASG launches into az-a.
4. Poll `DescribeAutoScalingGroups(small-az-a)` every 5s for ≤3min.
   New InstanceID `i-A2` appears.
5. `CreateTags(i-A2, {shard_id: "s-A"})`.
6. `AttachVolume(vol-A, i-A2, /dev/sdf)`. Wait for `in-use`.
7. Set `entry.InstanceID = "i-A2"`. Reset `launch_attempts = 0`.
   Persist. SM stays in SUSPENDED — the heartbeat is what completes
   the transition.

If any step fails: increment `launch_attempts`; SM stays SUSPENDED.
Next reap tick's `Evaluate()` retries the launch. No automatic flip
to GONE — losing data on transient hiccups is worse than waiting for
operator attention. Self-heal continues to retry indefinitely.

**T=4:00** — i-A2 boots. entrypoint.sh starts the shard binary
in unconfigured mode (gRPC listening, no DuckDB open). Binary mounts
/dev/sdf at /opt/app/data. Reads `/opt/app/data/shard.json` → finds
`shard_id=s-A`, heartbeat token, window. Opens DuckDB on existing
data (164k rows preserved). Starts heartbeating: `shard_id=s-A`,
`instance_id=i-A2`, `addr=10.0.1.7:9000`.

**T=4:05** — `ProcessHeartbeat(hb)` looks up `s-A.SM`. Currently
SUSPENDED. Fires `Heartbeat(hb)`. Guard: ShardID matches, entry's
current InstanceID is "i-A2". Target: INGESTING.
`INGESTING.OnEntryFrom(Heartbeat-from-SUSPENDED)`:

- Dial gRPC at `10.0.1.7:9000`. Set `entry.Addr`, `entry.Shard`.
- Old `entry.Shard` was already nil; no deferred close needed.
- `entry.LastHeartbeatAt = now()`. Persist.

**T=4:05+** — `s-A` back to INGESTING with all data preserved.
Queries route to the new IP. Writes resume.

Total downtime: ~3m35s. Bucket SM stayed in STASIS the whole time —
a shard going briefly SUSPENDED for recovery doesn't escalate to a
bucket-level state change.

**Restart resilience**: if the coordinator crashes between T=2:30
and T=4:05, `LoadFromStore` rehydrates `s-A.SM` in SUSPENDED with
`launch_attempts` whatever was persisted. First tick's `Evaluate()`
sees "in SUSPENDED, no InstanceID, attempts < budget" and retries
the launch. Idempotent: terminating an already-terminated i-A is a
no-op; SetDesiredCapacity is already correct.

## Components to build

### 1. Migration 030 — schema additions

**Files**: `store/migrations/030_state_machine_columns.sql`, regenerate `store/schema.sql` post-deploy.

`ALTER TABLE buckets` adds `state`, `cascade_started_at`. `ALTER
TABLE shard_records` adds `ingest_failures`, `ingest_unhealthy`,
`last_heartbeat_at`, `launch_attempts`. New index
`idx_shard_records_bucket_window`. Backfill `buckets.state =
'STASIS'` for existing rows.

**Effort**: 0.5d.

### 2. `qmuntal/stateless` integration

**Files**: `go.mod`, `go.sum`, `logdb/triggers.go` (trigger constants),
`logdb/states.go` (state constants).

Add the dep; define `ShardState` and `BucketState` typed constants;
define `Trigger` typed constants for both machines. Retire
`StateExpiring`.

**Effort**: 0.25d.

### 3. Shard SM

**Files**: `logdb/shardsm.go` (~400 LOC), `logdb/shardsm_test.go`.

`ShardSM` struct holding `*stateless.StateMachine`, `*ShardEntry`,
and a `ShardDeps` bundle (launcher, volumes, store, registry,
configurer). External-storage accessors read/write
`shard_records.state` via `store.UpdateShardState`. All OnEntry /
OnEntryFrom / guard methods implemented. `Evaluate(ctx)` runs the
autonomous-trigger checks.

Tests: every (state, trigger) pair → target state, side-effects
called, persistence written. Plus the recovery walkthrough
end-to-end with a fake launcher.

**Effort**: 2.5d (1.5d code, 1d tests).

### 4. Bucket SM

**Files**: `logdb/bucketsm.go` (~350 LOC), `logdb/bucketsm_test.go`.

`BucketSM` struct similar to ShardSM. External-storage accessors
read/write `buckets.state`. `Evaluate(ctx)` queries
`shard_records` for incoming/outgoing sets, computes gate status,
fires advancing trigger if met. `WARMING.OnTick` implements the
WindowEnd-extension policy.

Tests: cascade scenarios — clean rotation, slow rotation triggering
extensions, launch failure mid-cascade, scale-up, scale-down.
Restart-mid-cascade scenarios (bucket in WARMING at restart;
Evaluate converges).

**Effort**: 2.5d (1.5d code, 1d tests).

### 5. `ASGProvisioner` (refactor of `ASGLauncher`)

**Files**: `logdb/launcher_asg.go` (rewrite, was 184 LOC, becomes ~350 LOC).

Renamed. Single responsibility: launch one instance into a specific
(tier, AZ) ASG, return its InstanceID once running; terminate one
instance with capacity decrement. Per-(tier,AZ) mutex so concurrent
launches into the same ASG serialize. Polls
`DescribeAutoScalingGroups` to identify the new InstanceID by
diffing against a captured before-set.

Constructor takes a `map[tier]map[az]asgName` (parsed from
`SHARD_<TIER>_ASGS` env vars at boot). Exposes `AZsForTier(tier)
[]string` so the bucket SM can iterate the configured AZs, and
`StripeAZ(tier string, index int) string` as the pure striping
helper.

`LaunchFresh(tier, az)` and `LaunchRecovery(tier, az, volumeID)`
helpers; both share the same SetDesiredCapacity → poll → tag →
attach skeleton, differing only in whether the volume is created
fresh or reattached. Both validate that the (tier, AZ) pair is in
the configured map and error fast otherwise — silently falling
back to a different AZ would hide TF config drift.

Retains `TerminateInstance`. Deletes `LaunchInstance`,
`TerminateForReplacement`, `SetDesiredCapacity` (exported),
`TierMaxSize`. Deletes `asyncLauncher`, `TierScaler`,
`RecoveryTerminator` interfaces.

**Effort**: 1.5d.

### 6. Provisioner refactor

**Files**: `logdb/provisioner.go` (~1400 LOC → ~600 LOC).

`Provisioner.check()` becomes: refresh from store; for each bucket
call `bucketSM.Evaluate(ctx)`; for each shard call
`shardSM.Evaluate(ctx)`. The reconciler ops, `mintLaunchingRow*`,
`christen`, `reapDeadShards`, `reapStaleLaunching`,
`reapStrandedSuspended`, `HasAddr`, `recognisedAsDead`,
`syncASGDesiredCapacity`, `provisionForBucket`'s async/sync fork,
all deleted.

`ProcessHeartbeat` becomes: look up SM by ID → `sm.Fire(Heartbeat,
hb)`. The empty-ShardID / known-id / same-addr / new-addr branches
move into guards and OnEntryFrom paths inside the shard SM. The
`heartbeatMu` becomes per-SM (`stateless.FiringQueued`) instead of
provisioner-global.

`LoadFromStore` instantiates a BucketSM per bucket row and a
ShardSM per shard row in addition to the registry entry.

**Effort**: 2d.

### 7. Delete the lifecycle plumbing

**Files**: delete `logdb/lifecycle.go`, `logdb/lifecycle_poller.go`,
plus all their tests.

Also delete `Suspended` boolean from `ShardEntry`,
`Suspend`/`RecordIngestFailure`/`ResetIngestFailures` from
`ShardRegistry` (the per-shard data moves to persisted columns via
the SM).

Delete from `store`: `ClaimLaunchingForTier`,
`FindLaunchingByInstance`, `ListStaleLaunching`,
`CountShardsInBucket`, `CountShardsInBucketForFutureWindows`.
Add: `UpdateShardState(id, state)`, `UpdateBucketState(id, state)`,
`UpdateCascadeStartedAt(id, ts)`,
`ShardsInBucketWithWindow(bucket_id, window_start)`,
`ShardsInBucketBeforeWindow(bucket_id, window_start, states)`,
`TouchLastHeartbeatAt(id)`.

Update `cmd/coordinator/main.go` to stop reading `LIFECYCLE_QUEUE_URL`.

**Effort**: 1d (mostly deletions; the new store methods are thin
wrappers).

### 8. Terraform restructure

**Files**: `infrastructure-live/stacks/asg-weft-application/*.tf`,
`infrastructure-catalog/asg-weft/*.tf`.

Replace each tier's single ASG with one ASG per AZ (3 per tier, 9
total). Each ASG has `min_size=0, max_size=N, desired_capacity=0`,
no scaling policies, no lifecycle hooks, EC2 health checks
disabled. Launch templates unchanged (one per tier, shared across
the three AZ-specific ASGs).

Delete: `aws_autoscaling_lifecycle_hook.*`,
`aws_sns_topic.shard_lifecycle`,
`aws_sns_topic_subscription.shard_lifecycle_to_sqs`,
`aws_sqs_queue.shard_lifecycle` (+ DLQ if present), all associated
IAM policy statements.

Update `app_config_blob` SSM parameter:
- Remove `LIFECYCLE_QUEUE_URL`.
- Replace `SHARD_<TIER>_ASG_NAME` (single) with
  `SHARD_<TIER>_ASGS` (comma-separated per-AZ list, e.g.
  `weft-shard-small-az-a,weft-shard-small-az-b,weft-shard-small-az-c`).

Per the project-wide TF-safety rule (`CLAUDE.md`): before apply,
verify the plan touches only the resources above plus the
`app_config_blob` SSM update. Snapshot the parameter value
beforehand. The SSM blob change triggers a coordinator restart via
the existing hot-reload — schedule during a routine deploy window.
SQS queue must be empty before destroy; drain via console if
necessary.

**Effort**: 1.5d (mostly TF refactor; verification has the longest
tail).

### 9. Test rewrites

**Files**: roughly half of `logdb/*_test.go`.

Delete: `lifecycle_test.go`, `provisioner_lifecycle_test.go`,
`provisioner_rebind_nil_test.go`, `christen_test.go`,
`launching_test.go`, `reap_test.go`, `reap_recovery_test.go`,
`reap_suspended_test.go`, `heartbeat_addr_test.go`,
`heartbeat_rowcount_test.go`, `boot_order_test.go`,
`instance_refresh_repro_test.go`, `refresh_cascade_test.go`,
`zombie_repro_test.go`, `launcher_asg_test.go`,
`rebind_close_test.go`, `archiver_logdb_test.go`,
`asg_desired_test.go`.

Rewrite as SM scenario tests: `reconciler_test.go` becomes
`bucketsm_test.go` (renamed under §4); `steady_state_test.go`
becomes `bucketsm_stasis_test.go`; `window_rotation_test.go` and
`registry_routing_test.go` get rewritten against the new
machinery. Keep the underlying scenarios (especially the repros) as
"given this DB state, Evaluate fires X" tests.

Keep: `volumes_test.go`, `archive_volumes_test.go` (volume logic
unchanged), the registry read-path tests, `fixtures.go`.

**Effort**: 2d.

### 10. CLAUDE.md + doc updates

**Files**: `/Users/paul/dev/weft/CLAUDE.md`, `docs/shard-lifecycle.md`.

Update the architecture summary in the top-level CLAUDE.md
(`logdb/` section) to describe the SM model. Rewrite the relevant
chunks of `docs/shard-lifecycle.md` so on-call docs match
implementation. The ASG/SNS/SQS material in `docs/architecture.md`
also needs trimming.

**Effort**: 0.5d.

## Phased rollout

No feature flags. The system has no customers; broken paths are
fixed by replacement, not parallel migration.

### Phase 0 — plan landed
This document committed to `docs/plans/`.

### Phase 1 — schema + SM scaffolding
Components 1, 2, 3, 4 in a single PR. The SMs exist with full
transition coverage and tests; nothing in the provisioner uses
them yet. Migration applies cleanly to the existing DB.

### Phase 2 — provisioner cutover
Components 5, 6, 7, 9 in a single PR. This is the big switch — the
provisioner's lifecycle code is replaced wholesale; the lifecycle
poller is deleted; the heartbeat path goes through the SM. On
merge, dev mode runs the new code immediately.

### Phase 3 — TF restructure
Component 8 in a separate TF PR. Sequence:

1. Apply per-AZ ASG creation (new ASGs at desired=0; no instances
   launched yet).
2. Update `app_config_blob` to point at the new ASG list. Coordinator
   restart picks up the new mapping and starts launching into the
   per-AZ ASGs as it needs new shards.
3. Drain the old single-tier ASGs by letting their instances roll
   off through normal rotation, or by manual termination if we want
   to accelerate.
4. Once old ASGs hold zero instances, delete them along with the
   SNS topic, SQS queue, and lifecycle hooks.

### Phase 4 — docs
Component 10. Lands after Phase 3 is verified.

### Progress log

**2026-05-21 — Phase 0 + Phase 1 scaffold shipped.** Plan
committed as `e93e3ef`. FSM scaffolding committed as `55248cb` on
branch `the_big_lift`:

- Migration `store/migrations/056_state_machine_columns.sql` adds
  `buckets.state`, `buckets.cascade_started_at`, and the six new
  `shard_records` columns + the `(bucket_id, window_start)` index.
- `logdb/states.go` and `logdb/triggers.go` define the typed state
  + trigger constants (`StateExpiring` retired).
- `logdb/shardsm.go` (~290 LOC) implements `ShardSM` with every
  transition wired and action bodies stubbed (logging only).
- `logdb/bucketsm.go` (~210 LOC) implements `BucketSM` for the full
  cascade + scale-down branch; action bodies stubbed except for
  `cascade_started_at` set/clear which is real.
- `logdb/shardsm_test.go` and `logdb/bucketsm_test.go` cover every
  transition exhaustively against fakes — all passing.
- Store gained narrow update methods: `UpdateShardState`,
  `UpdateBucketState`, `UpdateCascadeStartedAt`,
  `TouchLastHeartbeatAt`, `ShardsInBucketWithWindow`,
  `ShardsInBucketBeforeWindow`. `Bucket` and `ShardRecord` structs
  gained the new fields.
- `StateExpiring` references removed from `registry.go`,
  `lifecycle.go`, and the now-vestigial
  `TestRegistry_ExpiringShardExcluded`.

`go build ./...`, `go vet ./logdb/ ./store/`, and
`go test ./logdb/ ./store/ -tags test` all clean on the new
commit.

**2026-05-25 — re-homed + Phase 2 started.** The Phase 1 scaffold
was re-homed off `the_big_lift` onto branch `asg-as-api-phase1`,
folded atop the app→service rename; the migration shipped as `056`
(not `030`). Two pre-Phase-2 cleanups to the FSM scaffolding:
`BucketSMDeps` collapsed into `BucketSM` (embeds `*Bucket`, holds a
`*gorm.DB` not a `*store.Store`); dead Phase-2 fields dropped from
`ShardSMDeps` (Launcher/Volumes/Configurer/LaunchRetryBudget) — they
return in slice B with their consumers.

Phase 2 is being landed as four reviewable, individually-compiling
slices on this branch (merged together):

- **Slice A — `ASGProvisioner` (component 5): SHIPPED** (`a795232`).
  Per-AZ launch primitive built *additively* alongside the old
  `ASGLauncher` (which stays until slice C). `Launch(spec)` (one path,
  fresh vs recovery keyed on `spec.ExistingVolumeID`), `Terminate`,
  `AZsForTier`, `StripeAZ`. Full unit coverage in
  `provisioner_asg_test.go`. NOTE: reuses `fakeVolumeManager` from
  `lifecycle_test.go` — re-home it to a surviving helper file when
  that file is deleted in slice D.
- **Slice B — SM action bodies + `Evaluate` (component 6 cont):**
  IN PROGRESS.
  - **B1.1 — ShardSM fresh launch + stale: SHIPPED** (`87eb643`).
    Evaluate-only convergence (OnEntry stays pure). LAUNCHING fresh
    launch (async goroutine + retry budget), INGESTING/READ_ONLY
    stale→SUSPENDED/GONE. `ShardEntry` gains AZ/StripeIndex/
    LaunchAttempts; store `SetShardLaunchResult` + `IncrementShard
    LaunchAttempts`; deps re-add Launcher (`ShardLaunchAPI`) + budget.
  - **B1.2 — ShardSM recovery (SUSPENDED→LAUNCHING): SHIPPED.**
    Recovery is split across two states it already had, not a new
    one: SUSPENDED *reaps* (Terminate dead instance + clear InstanceID,
    inline since it's one fast idempotent call) then fires `Relaunch`
    → LAUNCHING, which *relaunches* reattaching the surviving volume
    (B1.1's recovery launch). This keeps InstanceID unambiguous per
    state (SUSPENDED=dead, LAUNCHING=empty-or-replacement) — the fix
    for the disambiguation worry, without a TERMINATING state.
    `onEnterSuspended` now keeps InstanceID (clears only Addr/Shard);
    stranded check via `Volumes.FindByShard` → Stranded→GONE; new
    `TriggerRelaunch`. Restart-safe (reap idempotent; relaunch in
    LAUNCHING).
  - **B1.3 — `configureOnHeartbeat` gRPC dial + ARCHIVED cleanup:
    SHIPPED.** `configureOnHeartbeat` now dials via a new
    `ShardSMDeps.Configurer` (`ShardConfigurer`) and attaches the live
    handle on the LAUNCHING/SUSPENDED → INGESTING heartbeat; nil
    Configurer keeps the dev/test path (records addr, no handle). No
    deferred close — both entry paths reach it with `entry.Shard` already
    nil (LAUNCHING never had one; SUSPENDED.OnEntry dropped the dead
    conn). A failed dial surfaces the error but the row is already
    INGESTING (stateless writes the destination before OnEntry) — a later
    heartbeat reconnects (slice C). Archive took the lean-Evaluate route:
    `onEnterArchived` just logs; `evaluateArchived` runs export → ColdShard
    swap → volume retire → terminate, each gated on a persisted fact
    (ParquetPath set, volume cleared, instance cleared) so a crash
    mid-cleanup re-drives next tick. New deps: `Configurer`, `ParquetDir`,
    `LambdaFunctionURL`. New store narrow-writes: `SetShardParquetPath`,
    `ClearShardVolume`, `ClearShardInstance`. Tests cover dial
    success/failure and the export/retire/terminate + idempotent-settled +
    export-retry paths.
  - **B2 — BucketSM cascade bodies + `Evaluate`: MERGED INTO SLICE C**
    (decision 2026-05-25). Unlike the shard SM, whose collaborators are
    clean external services (`Launcher`/`Volumes`/`Configurer`), the
    bucket SM's only collaborator is the provisioner itself — its action
    bodies mint child shard SMs and fire triggers on them, which needs
    the provisioner's `shardSMs` map + mint factory that only exist after
    Slice C's `LoadFromStore`. Splitting B2 out would force an invented
    seam; landing it with C avoids a throwaway abstraction. B2's
    gate-only logic (Evaluate's incoming/outgoing `shard_records`
    queries) needs nothing new — the bucket SM holds a `*gorm.DB` and
    queries the cascade sets inline where it needs them (no dedicated
    store methods; gorm's query builder is the tool).
**FSM redesign — 2026-05-25 (supersedes the State-machine-details
section above for archival).** Design review reworked where archival
lives, to protect the hot ingest window:

- **Archival is a shard concern, not a bucket cascade phase.** The shard
  FSM gains an **ARCHIVING** state between READ_ONLY and ARCHIVED:
  LAUNCHING → INGESTING → READ_ONLY → ARCHIVING → ARCHIVED. ARCHIVING is
  the export-in-progress phase; its `Evaluate` runs export → ColdShard
  swap → volume retire → terminate, then fires `ArchiveComplete` →
  ARCHIVED itself (the one transition the shard owns, like the heartbeat
  that completes LAUNCHING → INGESTING). ARCHIVED is now a clean
  terminal. ARCHIVING is query-routable like READ_ONLY (handled in
  `MatchingEntries`, *not* via stateless `SubstateOf` — that would
  wrongly inherit READ_ONLY's `HeartbeatStale → SUSPENDED` edge onto a
  shard whose instance is being torn down on purpose). Trigger
  `ArchiveRequested` now targets ARCHIVING; new trigger
  `ArchiveComplete` for ARCHIVING → ARCHIVED.
- **Bucket FSM is rotation-only:** STASIS → WARMING → ROTATING → STASIS
  (+ SCALING_DOWN). No bucket ARCHIVING state — the bucket never *waits*
  on archival, so there's no gate and thus no state (a bucket ARCHIVING
  state had no coherent exit). `ArchivalsComplete` / `OldShardsRotated`'s
  old ARCHIVING target go away; ROTATING returns straight to STASIS.
- **The bucket still owns *firing* archival**, as a fire-and-forget
  retention sweep in its `Evaluate` (every tick, any state): fire
  `ArchiveRequested` on READ_ONLY shards past retention (`now - WindowEnd
  > retention`, retention = `WindowDuration`). It owns the directive but
  waits on nothing, so a slow S3 export can never block the next roll.
- **Safe capacity sequencing (the hot-window guarantee)** is encoded in
  the rotation gates, overcapacity-before-drain: WARMING → ROTATING only
  once the incoming replacements are all INGESTING (→ 2·Target hot);
  ROTATING → STASIS once the outgoing cohort is READ_ONLY. Cohorts are
  window math off `now`: incoming = `window_start == W+1`, rotate =
  `window_start == W`, archive = `window_start == W-1` (= W − retention).

The shard-side half of this redesign **shipped** as the first Slice C
commit (states/triggers/`shardsm.go` ARCHIVING + `MatchingEntries`
parity + tests; revises the earlier B1.3 archival, which had READ_ONLY →
ARCHIVED with the work in ARCHIVED). The bucket-side half lands with the
provisioner mint factory below.

- **Slice C — provisioner cutover + lifecycle deletion + bucket SM
  bodies (components 6, 7 + B2):** the atomic switch. IN PROGRESS.
  Planned commit order (each compiles; old reconciler stays live until
  the switch commit):
  0. **SHIPPED** — shard FSM ARCHIVING state (the redesign above).
  1. **SHIPPED** — Provisioner builds `bucketSMs`/`shardSMs` maps +
     `ShardController` (mint factory + SM resolver), *additively*: new
     `logdb/provisioner_sm.go` holds `LoadStateMachines` (wired into
     `startProd`/`startDev` after `SetStore`), `MintShard`, `FireShard`,
     `shardSMDeps`; `Provisioner` gains `bucketSMs`/`shardSMs`/`smMu` +
     a concrete `*store.Store` (the interface fields don't expose
     `DB()`). `check()` still runs the old reconciler — nothing here is
     on the live tick. BucketSM bodies are real now: `onEnterWarming`
     mints next-window (rotation) or current-window (fill/scale-up)
     cohorts inheriting stripe indices, idempotently; `onEnterRotating`
     fires `RotateRequested` on the outgoing (older-window) INGESTING
     shards; `onEnterScalingDown` rotates the N youngest. `Evaluate`
     dispatches per state: STASIS decides scale-down/rotation/scale-up,
     WARMING gates on incoming-all-INGESTING, ROTATING gates on
     outgoing-drained, SCALING_DOWN on back-to-Target; a retention sweep
     fires `ArchiveRequested` (fire-and-forget) on past-retention
     READ_ONLY shards every tick. Cohorts derive from inline gorm
     queries keyed on the *latest live window* (robust to clock drift
     mid-cascade). Cohort sets are 3NF — no `ShardsInBucket*` store
     methods. Full unit coverage in `bucketsm_test.go` against a
     `fakeShardController`. **Deferred:** the `WARMING.OnTick`
     WindowEnd-extension anti-write-gap policy — it needs
     `CascadeStallThreshold`/`ExtensionStep`/`MaxExtensions` config that
     doesn't exist yet (TODO marked in `evaluateWarming`).
  2. The switch: `check()` → Evaluate per bucket/shard; `ProcessHeartbeat`
     → `sm.Fire`; delete reconciler ops / christen / reap* /
     mintLaunchingRow* / `Suspended` bool. The obsolete `*_test.go`
     files (slice D deletions) must land in this same commit — deleting
     `lifecycle.go` breaks `lifecycle_test.go`'s compile.
  3. Delete lifecycle plumbing (`lifecycle.go`, `lifecycle_poller.go`,
     dead store methods, `LIFECYCLE_QUEUE_URL`).
  4. New SM scenario tests (the surviving half of slice D).
- **Slice D — test rewrites (component 9).** Deletions fold into the
  switch commit (2); new scenario tests are step 4 above.

**Correction to the 2026-05-21 log:** it claimed Phase 1 added store
methods `UpdateShardState`, `UpdateBucketState`, `UpdateCascadeStartedAt`,
`TouchLastHeartbeatAt`, `ShardsInBucketWithWindow`,
`ShardsInBucketBeforeWindow`. In fact **none of these exist** — the FSMs
do their narrow state / cascade_started_at writes *inline* in their
`writeState` mutators and OnEntry actions (a `gorm` `Update` against the
row), not via store methods, and the bucket SM reads its cascade sets the
same way. There is no `ShardsInBucket*` store method and there shouldn't
be — those queries live in the bucket SM.

### What's left

**Phase 1 leftovers (none expected, but verify):**
- The plan called for `Suspended` boolean removal from
  `ShardEntry` as part of Phase 1 — **not done yet**. Still
  present in `logdb/registry.go` with its `RecordIngestFailure` /
  `ResetIngestFailures` / `Suspend` methods. Defer to Phase 2;
  removing it touches the heartbeat path and the
  `shard.IngestRecorder` interface contract, so it belongs in the
  same PR that rewires `ProcessHeartbeat`.
- `LoadFromStore` in `registry.go` does not yet instantiate
  `ShardSM` instances. The registry's `entries` map still holds
  bare `*ShardEntry` only. Phase 2 task.

**Phase 2 — provisioner cutover (the big one):**

1. **`ASGProvisioner` (component 5).** Rewrite `launcher_asg.go`:
   - Constructor takes `map[tier]map[az]asgName` parsed from
     `SHARD_<TIER>_ASGS` (comma-separated AZ list per tier).
   - Per-(tier, AZ) `sync.Mutex` keyed launches.
   - `LaunchFresh(tier, az)` and
     `LaunchRecovery(tier, az, volumeID)`: capture before-set,
     SetDesiredCapacity, poll DescribeAutoScalingGroups, return
     new InstanceID once running, tag, then create/attach volume.
   - `Terminate(instanceID)` keeps current semantics
     (decrement capacity).
   - Expose `AZsForTier(tier) []string` and pure helper
     `StripeAZ(tier string, index int) string`.
   - Delete `LaunchInstance` (stale stub),
     `TerminateForReplacement`, exported `SetDesiredCapacity`,
     `TierMaxSize`.
   - Delete `asyncLauncher`, `TierScaler`, `RecoveryTerminator`
     interfaces in `launcher.go`.

2. **Provisioner refactor (component 6).** In
   `logdb/provisioner.go`:
   - `Provisioner.check()` becomes
     `registry.RefreshFromStore()` → for each bucket
     `bucketSM.Evaluate(ctx)` → for each shard
     `shardSM.Evaluate(ctx)`.
   - `ProcessHeartbeat`: look up the SM by id; on empty
     `hb.ShardID` log + return (christen now happens via the
     bucket SM's WARMING.OnEntry, not on heartbeat);
     `sm.Fire(ctx, TriggerHeartbeat, hb)`.
   - Delete `mintLaunchingRow*`, `christen`, `reapDeadShards`,
     `reapStaleLaunching`, `reapStrandedSuspended`, `HasAddr`,
     `recognisedAsDead`, `syncASGDesiredCapacity`,
     `provisionForBucket` async/sync fork, `currentWindow`
     helper (folded into bucket SM).
   - `LoadFromStore` instantiates a `BucketSM` per bucket and a
     `ShardSM` per shard alongside the registry entry; stash the
     SMs in new fields on `Provisioner` (`bucketSMs map[int64]*BucketSM`,
     `shardSMs map[string]*ShardSM`).
   - Remove `Suspended` bool from `ShardEntry`. Add `IngestUnhealthy`
     read-through to the persisted column.

3. **Wire the SM action bodies (component 6 cont).** Flesh out the
   stubs in `shardsm.go` and `bucketsm.go`:
   - `ShardSM.onEnterLaunching`: `ASGProvisioner.LaunchFresh` or
     `LaunchRecovery` depending on `spec`; persist `entry.InstanceID`,
     `entry.AZ`, `entry.StripeIndex`. Bump `launch_attempts`; fire
     `TriggerLaunchFailed` if budget exhausted.
   - `ShardSM.configureOnHeartbeat`: dial gRPC via
     `deps.Configurer`, send Configure RPC, update `entry.Shard`,
     `entry.Addr`. On rebind variant, schedule deferred close of old conn.
   - `ShardSM.onEnterSuspended`: clear runtime fields (already
     done in stub), then call `LaunchRecovery` via deps.
   - `ShardSM.onEnterArchived`: `shard.Archive` → ColdShard swap
     → `volumes.SnapshotVolume`/`DetachVolume`/`DeleteVolume` →
     `launcher.Terminate`. Each step idempotent on retry.
   - `ShardSM.Evaluate(ctx)`: stale check (fires HeartbeatStale),
     launch-budget check (fires LaunchFailed), stranded-volume
     check (fires Stranded). The bucket SM iterates these.
   - `BucketSM.onEnterWarming`: compute next-window bounds via
     bucket helper; for each outgoing shard, mint one incoming
     `ShardSM` inheriting `stripe_index` (or assign 0..Target-1
     for an initial fill); fire `TriggerLaunch` on each.
   - `BucketSM.onEnterRotating`: load outgoing shard SMs from
     the provisioner's map; fire `TriggerRotateRequested` on
     each.
   - `BucketSM.onEnterArchiving`: same shape for
     `TriggerArchiveRequested`.
   - `BucketSM.onEnterScalingDown`: load INGESTING shards; pick
     youngest N=current-target; fire `TriggerRotateRequested`.
   - `BucketSM.Evaluate(ctx)`: in STASIS check rotation-due and
     scale conditions; in WARMING re-check incoming → INGESTING
     gate (and run OnTick extension policy if cascade_started_at
     is older than CascadeStallThreshold); in ROTATING / ARCHIVING /
     SCALING_DOWN re-check their gates. Fire the advancing
     trigger when the gate is satisfied.
   - `BucketSM.OnTick` window-extension: load outgoing shards,
     update WindowEnd by +ExtensionStep on each, up to
     MaxExtensions. Persisted via store's existing
     `SaveShard` (full upsert).

4. **Delete the lifecycle plumbing (component 7).** Drop:
   - `logdb/lifecycle.go`
   - `logdb/lifecycle_poller.go`
   - `logdb/lifecycle_test.go`,
     `provisioner_lifecycle_test.go`,
     `provisioner_rebind_nil_test.go`,
     `christen_test.go`, `launching_test.go`,
     `reap_test.go`, `reap_recovery_test.go`,
     `reap_suspended_test.go`,
     `heartbeat_addr_test.go`, `heartbeat_rowcount_test.go`,
     `boot_order_test.go`, `instance_refresh_repro_test.go`,
     `refresh_cascade_test.go`, `zombie_repro_test.go`,
     `launcher_asg_test.go`, `rebind_close_test.go`,
     `archiver_logdb_test.go`, `asg_desired_test.go`.
   - From `store/shard.go`: `ClaimLaunchingForTier`,
     `FindLaunchingByInstance`, `ListStaleLaunching`,
     `CountShardsInBucket`,
     `CountShardsInBucketForFutureWindows`.
   - From `cmd/coordinator/main.go`: any reading of
     `LIFECYCLE_QUEUE_URL`.

5. **Test rewrites (component 9).** New scenario tests against
   the SMs:
   - `bucketsm_cascade_test.go` (full window rotation under
     clean, slow-launch, failure-mid-cascade conditions).
   - `bucketsm_stasis_test.go` (steady-state self-heal).
   - `shardsm_recovery_test.go` (the lose-compute-keep-volume
     trace from the plan's "Failure walkthrough" section).
   - Keep: `volumes_test.go`, `archive_volumes_test.go`,
     `registry_persistence_test.go`,
     `registry_routing_test.go`, `registry_stale_test.go`,
     `registry_empty_shard_test.go`, `fixtures.go`,
     `testhelpers_test.go`.

**Phase 3 — TF restructure (component 8).** Per-AZ ASGs (3 per
tier), autoscaling off, lifecycle hooks gone, SNS/SQS gone,
config blob updated. Detailed in component 8 above. Don't apply
this phase until Phase 2 has merged and dev mode is clean for at
least a day.

**Phase 4 — docs (component 10).** Update top-level CLAUDE.md +
`docs/shard-lifecycle.md` + `docs/architecture.md` to remove
SNS/SQS/ASG-autoscaling references.

### Pickup notes for a fresh session

- Branch is `the_big_lift`. Don't rebase onto main — there's
  other in-flight work (see `git log main..the_big_lift`).
- Stubs in `shardsm.go` / `bucketsm.go` log "stub — Phase 2
  wires …" — grep for those strings to find every action body
  still pending.
- `qmuntal/stateless` v1.8.0 is in `go.mod`. Its public surface:
  `State` and `Trigger` are `any`; main entry is
  `NewStateMachineWithExternalStorage(accessor, mutator,
  FiringQueued)`; `Configure(state).Permit(trigger, dest,
  guards...)`, `.PermitDynamic(trigger, selector, guards...)`,
  `.OnEntry(fn)`, `.OnEntryFrom(trigger, fn)`; firing via
  `FireCtx(ctx, trigger, args...)`. Guards are
  `func(ctx, args...) bool`, actions are
  `func(ctx, args...) error`, dynamic selectors are
  `func(ctx, args...) (State, error)`.
- The shard SM's `heartbeatArg` helper extracts a
  `store.ShardHeartbeat` from variadic args; reuse for any
  trigger that needs to pass payload.
- Test fixture pattern: `makeFixtureShardSM(t, initialState)` and
  `makeFixtureBucketSM(t, initialState)` return `(sm, fakeStore,
  entry/bucket)`. Use those for any new test rather than rolling
  your own.
- The Suspended-bool removal is intentionally deferred to Phase 2
  because it touches the `shard.IngestRecorder` interface
  (`RecordIngestFailure` / `ResetIngestFailures`); the new
  approach is a persisted `IngestUnhealthy` column with the
  registry caching it.

## Estimated effort

| Phase | Components | Effort |
|---|---|---|
| 1 | 1, 2, 3, 4 | 5.75d |
| 2 | 5, 6, 7, 9 | 6.5d |
| 3 | 8 | 1.5d |
| 4 | 10 | 0.5d |
| **Total** | | **~14d** |

Real wall-clock will run longer because Phase 3's TF apply needs
verification windows between steps.

## Non-goals

- **Over-retention cleanup for ARCHIVED shards.** ARCHIVED is
  terminal. A future sweep can fire `AdminDelete` triggers by
  policy, but designing that policy is out of scope here.
- **Bucket-split policy.** Disabled in `EvaluateSplit`; stays
  disabled. See `bucket-split-policy.md`.
- **Multi-region.** Single region; AZ-awareness only.
- **AZ-aware failover.** Striping spreads a bucket across AZs at
  launch time, but a launch failure in one AZ doesn't auto-retry
  in another. Cross-AZ rebalancing of existing shards is also
  out of scope — see `az-resilience.md`.
- **Spot integration.** All shards on on-demand. Spot reclamation
  semantics would re-introduce some of the complexity this plan
  removes.
- **Health-check-driven replacement.** Off. The provisioner is the
  sole authority on instance lifecycle.
- **Sub-second failover.** The reap tick + ASG launch + boot path
  has a ~3–5 minute floor. Closing that is a separate effort
  (warm pool of pre-launched instances, faster heartbeat-stale
  detection, etc.).
- **Bucket reconfiguration mid-cascade.** Changing a bucket's tier
  or WindowDuration while a cascade is in flight is undefined
  behavior; future work to model as a discrete trigger.

## Cross-references

- `docs/shard-lifecycle.md` — what we're replacing.
- `docs/plans/bucket-split-policy.md` — the disabled feature this
  plan preserves the affordance for.
- `docs/plans/az-resilience.md` — per-AZ ASGs are a building block
  toward this.
- `docs/plans/retention-enforcement.md` — over-retention cleanup
  for ARCHIVED shards would slot into the future-work section.
