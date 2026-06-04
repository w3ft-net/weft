# Shard Lifecycle

> **Launch mechanism updated (2026-06) — see
> [`plans/shard-ec2-direct.md`](plans/shard-ec2-direct.md).** The state machine,
> windows, rotation, recovery, archival, and EBS-volume handling below are
> current. What changed: there is no Auto Scaling Group. Wherever this doc
> describes the launch handoff via the ASG — `ASGProvisioner.Launch`,
> `SetDesiredCapacity`, `EC2_INSTANCE_LAUNCHING` lifecycle hooks, SNS/SQS claim,
> `Pending:Wait`, ABANDON/CONTINUE, `syncASGDesiredCapacity`, the
> `autoscaling:*` IAM, "the ASG replaces the instance" — the coordinator now
> does it directly: `EC2Provisioner.Launch` runs one instance via
> `ec2:RunInstances` (tier launch template + striped AZ's subnet), tags
> `shard_id` at birth, waits for `running`, attaches the volume; recovery reaps
> with `ec2:TerminateInstances` and relaunches. The coordinator is the sole
> capacity authority — no ASG to relaunch instances behind its back — so the
> "ghost instance" failure mode is gone. The IAM is `ec2:RunInstances` /
> `ec2:TerminateInstances` / `ec2:CreateTags` / scoped `iam:PassRole` instead of
> the `autoscaling:*` actions.

This doc is the target architecture. It's broadly implemented as of
2026-04 but not complete — see [`cleanup.md`](cleanup.md) for the
live inventory of remaining work. Anywhere this doc and the code
disagree and `cleanup.md` doesn't mention it, the code is a bug; file
it.

## What a shard is

A **shard** is a logical, addressable container of log rows for a
contiguous window of time. Concretely:

```
shard identity = (stable name, time window, app set, EBS volume)
```

- **stable name** — e.g. `shard-2026w16-small-01`. Once assigned, it
  never changes for the life of the shard. Used as the key in the
  registry, in the `shard_records` table, and as the EBS tag that
  binds storage to identity.
- **time window** — the half-open `[WindowStart, WindowEnd)`
  interval of log timestamps this shard is authoritative for.
  Writes outside this window go to a different shard. Queries
  overlapping this window route to this shard.
- **app set** — which apps' rows are accepted. May be empty
  (catch-all) or a specific list (per-app shards, for high-volume
  apps that want their own fleet).
- **EBS volume** — the gp3 volume holding `/opt/app/data/shard.duckdb`.
  This is where the shard's actual data lives. It outlives the
  compute instance that currently hosts it.

A shard is **not** an EC2 instance. An EC2 instance is the current
compute host for a shard. If that instance dies, the EBS volume gets
reattached to a fresh instance and the shard continues under the same
identity.

This is the critical distinction: **instance lifecycle ≠ shard
lifecycle**. A shard can outlive many instances.

> **FSM rewrite (current).** A shard is now its own finite state
> machine — one `logdb.Shard` that is the persisted row, the runtime
> object, and the `qmuntal/stateless` state machine. Each shard's own
> `Evaluate()` (run once per reconciler tick) drives its convergence;
> there is no separate reconciler with numbered "ops", and the SNS/SQS
> lifecycle hook is gone (deleted in `7b0de94`). The transition table
> lives in `logdb/states.go` + `logdb/triggers.go`; the design is in
> `docs/plans/asg-as-api-stateless.md` and `docs/plans/shard-type-collapse.md`.
> The "LAUNCHING handoff", "Rotation", and reconciler-op narrative
> further down this doc predates that rewrite — read it as history.

## Shard states

```
   ┌─────────────┐  launch (Evaluate)        ┌─────────────┐
   │  LAUNCHING  │ ── ASGProvisioner.Launch ▶│  INGESTING  │◀─┐ heartbeat
   └─────────────┘    + first heartbeat      └─────────────┘  │ (rebind /
        │ retry budget                        │   │   ▲       │  promote)
        │ spent                     ingest    │   │   │ cooldown
        ▼                       failures ≥ N  │   │   │ (half-open)
     ┌──────┐                                 │   ▼   │
     │ GONE │◀── stranded / admin-delete      │ ┌──────────┐
     └──────┘     ▲          ▲                │ │ DEGRADED │ (no writes,
        ▲         │          │  window closes │ └──────────┘  still reads)
        │  hb     │          ▼                │      │
        │ stale   │   ┌─────────────┐         │      │ window closes
   ┌───────────┐ └───│  SUSPENDED  │◀────hb───┘      ▼
   │ (volume?) │     └─────────────┘  stale    ┌─────────────┐
   └───────────┘   reap instance,             │  READ_ONLY  │
                   relaunch ▶ LAUNCHING        └─────────────┘
                                                      │ retention sweep
                                                      ▼
                                               ┌─────────────┐
                                               │  ARCHIVING  │ export → cold
                                               └─────────────┘  → retire vol
                                                      │          → terminate
                                                      ▼
                                               ┌─────────────┐
                                               │  ARCHIVED   │ ColdShard on S3,
                                               └─────────────┘  queryable forever
```

- **LAUNCHING** — a persisted row with a minted `shard_id`, token,
  bucket, window, tier, AZ + stripe index, but no `instance_id` yet.
  The shard's `Evaluate` calls `ASGProvisioner.Launch` (async),
  which bumps the tier/AZ ASG's desired capacity, creates+attaches an
  EBS volume, and stamps the instance; the instance's first heartbeat
  promotes it to INGESTING. Exhausting the launch retry budget fires
  LaunchFailed → GONE.
- **INGESTING** — serving writes + queries for its `(window,
  services)`. A bucket holds `Target` INGESTING shards at once; writes
  round-robin across them.
- **DEGRADED** — the ingest circuit breaker: consecutive ingest
  failures crossed `SuspendFailureThreshold`. Out of write routing
  (not INGESTING) but still read-queryable; `Evaluate` re-admits it to
  INGESTING after a cooldown (half-open). Replaces the removed Suspended
  ingest latch.
- **READ_ONLY** — past the window's end; no new writes, still queried
  until the retention sweep archives it.
- **ARCHIVING** — the shard's `Evaluate` runs export-to-Parquet →
  swap live handle for a ColdShard → snapshot+detach+delete the volume
  → terminate the instance, each step gated on a persisted fact so a
  crash mid-cleanup re-drives it. Query-routable throughout (serves
  from the live DuckDB until the ColdShard swaps in). Fires
  ArchiveComplete → ARCHIVED itself once all steps land.
- **ARCHIVED** — data on S3, instance + volume gone, ColdShard serves
  queries via Lambda. Terminal for the shard's identity; AdminDelete is
  the only edge out (→ GONE).
- **SUSPENDED** — recoverable instance death: a stale heartbeat on an
  INGESTING/READ_ONLY/DEGRADED shard with a surviving EBS volume lands
  here. `Evaluate` reaps the dead instance and relaunches (→ LAUNCHING,
  reattaching the volume). If the volume is gone → Stranded → GONE.
- **GONE** — the cleanup sink (launch-budget exhaustion, data-already-
  lost, admin delete). `onEnterGone` removes the entry from the registry
  and deletes the row.

Transitions are fired only from a shard's own `Evaluate`/`Fire`
(no instance self-transitions); `stateless`'s FiringQueued mode
serializes triggers per shard.

### LAUNCHING handoff

Launching a new shard via the async (ASG) path is a three-party
choreography around a single DB row. Each party's read of the row is
the authoritative answer to "what's the reconciler already doing?":

```
 reconciler             lifecycle hook          shard process
 ──────────             ──────────────          ─────────────
 INSERT LAUNCHING  →
   (shard_id,
    bucket, window,
    token, tier,
    instance_id='')
        │
        └─ SetDesiredCapacity +1
                                │
                                ASG launches instance
                                │
                                ▼
                  ClaimLaunchingForTier(tier, i-X)
                  stamps instance_id = i-X, tags
                  volume with shard_id, attaches
                                │
                                └─ CompleteLifecycleAction(CONTINUE)
                                                       │
                                                       ▼
                                                  instance boots,
                                                  reads empty volume,
                                                  heartbeats with
                                                  shard_id=""
                                                  instance_id=i-X
                                                       │
 FindLaunchingByInstance(i-X) ──────────────────────── ┘
 returns the row; Configure RPC sends the row's
 fields; UPSERT flips state LAUNCHING → INGESTING
```

A missing row at any hand-off point is a bug, not a fallback:

- Lifecycle hook with no matching row → ABANDON the instance (ASG
  terminates it).
- Christen with no matching row → error, shard stays unclaimed and
  will eventually be reaped.

The stale-LAUNCHING reaper sweeps rows older than ~10 minutes: empty
`instance_id` means the ASG never brought one up (capacity issue);
populated `instance_id` means the instance never heartbeated (bad
AMI, userdata failure) and gets ABANDON-signalled to the ASG.

### Rebind: prodigal-son heartbeats

The "rebind" arrows on the diagram are not state transitions; they're
addr/instance_id updates **within** the shard's current state.

If an instance hosting shard S dies and a replacement instance comes
up with the same EBS volume attached, the shard process reads
`shard.json`, heartbeats with the existing `shard_id` at the new
addr, and the coordinator sees an already-registered shard reporting
in. The coordinator updates `addr` and `instance_id`, reconnects the
gRPC handle, clears any `Suspended` flag, and leaves the state
untouched. The shard was INGESTING before → it's INGESTING after.
READ_ONLY → READ_ONLY. ARCHIVING → ARCHIVING (the export resumes or
restarts depending on how far it got; see Archiving section).

No christening happens. Concretely: `kevcons chaos --role shard` on a
live shard should put it through this path — kill the instance, the
ASG-replaced instance reattaches the same EBS, the shard process
comes up with the same identity, and the coordinator just re-wires
its gRPC handle.

LAUNCHING is the only state an empty-`shard_id` heartbeat can
terminate in. A shard with a populated `shard.json` heartbeats its
durable id on every ping — its path is always rebind, never
christen.

## Rotation

Window cadence is a **per-bucket** property, persisted on the
`buckets` table:

- `bucket.WindowDuration` — how long one window covers (e.g. `7d`).
- `bucket.RetentionPeriod` — how long past `WindowEnd` a READ_ONLY
  shard stays hot before archiving (e.g. `7d`).
- `coordinator.PrewarmLead` (package var, not per-bucket yet) —
  how far before a shard's `WindowEnd` the reconciler starts
  pre-warming next-window shards.

Walkthrough assumes `WindowDuration = 7d`, `RetentionPeriod = 7d`
(current INGESTING + previous window still hot for reads).

Steady state (call it day N within the current window):

```
       │ window W-prev        │ window W0 (current)       │ window W1 (future)
───────┼──────────────────────┼───────────────────────────┼───────────────
       │  7..6..5..4..3..2..1 │  7..6..5..4..3..2..1..0   │
       │                      │                           │
       │  READ_ONLY fleet     │  INGESTING fleet          │
       │  (queries only)      │  (all writes, queries)    │
```

All writes go to W0's shards. Queries spanning W-prev and W0 fan
out to both fleets.

Window handoff (approaching W0's end, when `now + PrewarmLead ≥
WindowEnd`):

```
       │ W-prev       │ W0               │ W1
───────┼──────────────┼──────────────────┼──────────────────
       │              │                  │
       │  READ_ONLY   │  INGESTING       │  LAUNCHING then
       │              │  (primary)       │   INGESTING (pre-
       │              │                  │   warmed via op 4)
```

For the last `PrewarmLead` interval of W0, **both W0 and W1 shards
exist**. The reconciler's op 4 (pre-warm) inserts LAUNCHING rows
for W1 ahead of the boundary; `syncASGDesiredCapacity` adds them
to ASG desired_capacity (§3's formula includes future-window
LAUNCHING); the ASG brings up instances; the lifecycle hook claims
the rows; heartbeats promote them to INGESTING before W0's
WindowEnd. Writes route by timestamp: rows in W0 go to W0's
shards; rows crossing into W1 go to W1's shards.

At boundary crossing, op 3 transitions W0 shards INGESTING →
READ_ONLY. When RetentionPeriod past `WindowEnd`, op 5 archives,
op 6 terminates the instance.

Fleet-count math in terms of config:

- At steady state: `ceil(RetentionPeriod / WindowDuration) + 1`
  concurrent fleets (current INGESTING + past-window READ_ONLYs).
- At handoff: one more briefly, for the pre-warmed next window.
  For `WindowDuration = RetentionPeriod = 7d`: `(1+1) + 1 = 3`
  per-window fleets at peak.

`max_size` on the ASG is sized against this peak (see §3 in
[`cleanup.md`](cleanup.md) for the formula).

## Identity persistence

When an instance comes up, it discovers its shard identity like this:

1. EBS volume attached to the instance carries
   `/opt/app/data/shard.json`, which holds at minimum:
   ```json
   {
     "shard_id": "shard-2026w16-small-01",
     "window_start": "2026-04-13T00:00:00Z",
     "window_end":   "2026-04-20T00:00:00Z",
     "apps":         ["weft"],
     "heartbeat_token": "…hex…"
   }
   ```
2. The shard process reads `shard.json` and heartbeats as that
   identity. This is the rebind path — the coordinator finds a
   matching registry entry and re-wires its gRPC handle.
3. If `shard.json` doesn't exist (fresh EBS volume), the shard
   heartbeats with an empty `shard_id`. The coordinator looks up the
   LAUNCHING row stamped with this instance's id (the lifecycle hook
   claimed it earlier), sends Configure with the row's
   `(shard_id, window, apps, heartbeat_token)`, and UPSERTs the row
   from LAUNCHING → INGESTING. The shard writes `shard.json` on
   receipt of Configure.

EBS volumes are tagged with the shard id so the coordinator can
find "the volume that used to host shard X" when an instance dies.
For a christening instance the lifecycle hook applies this tag as
part of claiming the LAUNCHING row — the volume already carries its
real shard id when the instance boots, not a placeholder to be
renamed later.

## EBS re-attach on instance death

Instance death splits into two lanes depending on whether the
shard's EBS volume survived:

**Lane A — `ebs_volume_id` set on the record.** The shard's data
is intact on the volume; recovery reattaches it to a fresh
instance.

1. Instance I-A hosts shard S, EBS volume V-S attached.
2. I-A dies (SIGKILL, hardware failure, spot reclaim).
3. Heartbeat for S stops. `reapDeadShards` notices after
   `deadFor` (default 2 minutes) and preserves the registry
   entry — the `ebs_volume_id` field is what distinguishes
   "keep, will recover" from "gone, reap."
4. ASG's own health check replaces I-A with I-B (via the same
   `desired_capacity` syncASG sets, not via a coordinator call).
5. Lifecycle hook fires for I-B. It sees V-S in the available
   pool tagged with S's `shard_id`, filters by recognised-as-dead
   (registry entry exists + heartbeat stale), and issues a
   RECOVER decision: attach V-S to I-B at `/dev/sdf`, signal
   `CONTINUE` on the ASG hook, return.
6. I-B's startup script mounts the volume, reads `shard.json`,
   starts weft-shard with the recovered identity.
7. I-B heartbeats with S's existing shard id — rebind path:
   coordinator updates `addr` / `instance_id`, state/window/apps
   preserved.

Nothing about the shard's identity, window, or data changed.
From a writer's or querier's perspective the shard was offline
for the heartbeat-detection + instance-boot window (~2–3 min).

**Lane B — `ebs_volume_id` empty on the record.** No volume to
recover; the shard's data is gone with the instance. Reaper
removes the registry entry and asks the launcher to terminate
any lingering instance. The reconciler's next tick will bring up
a replacement via op 2 under the standard LAUNCHING handoff.

Pre-existing constraints:
- EBS volumes are AZ-scoped. Recovery requires the replacement
  instance in the same AZ. See
  [az-resilience.md](az-resilience.md) for the cross-AZ design.
- Coordinator role needs `ec2:AttachVolume`, `ec2:DetachVolume`,
  `ec2:DescribeVolumes`, `ec2:CreateVolume`, `ec2:DeleteVolume`,
  `ec2:CreateTags`,
  `autoscaling:TerminateInstanceInAutoScalingGroup`,
  `autoscaling:SetDesiredCapacity`,
  `autoscaling:DescribeAutoScalingGroups`.

Step 4 is coordinator-driven: when `reapDeadShards` sees a stale
heartbeat on an EBS-backed shard, it calls
`ASGLauncher.TerminateForReplacement` (which passes
`ShouldDecrementDesiredCapacity=false`) and nulls the entry's
`InstanceID`. That's faster than waiting for ASG's own health
check and keeps the ASG's actual count stable.

## Archiving and cold storage

Archive scheduling is op 5 in the reconciler. When a READ_ONLY
shard is past `bucket.RetentionPeriod` (measured from
`WindowEnd`, not `CreatedAt`), `ArchiveShardOp` fires. Apply runs
the export sequence synchronously:

1. Export DuckDB contents to Parquet. Local-disk destination in
   dev; `s3://weft-cold-<acct>/...` in prod via DuckDB's httpfs
   and S3 credentials in `shard/s3config.go`.
2. Swap the shard handle from a `DuckDBShard` to a `ColdShard`
   (or `LambdaColdShard` when `LAMBDA_FUNCTION_URL` is set).
   Queries continue uninterrupted; they just read from Parquet now.
3. Record the Parquet path in `shard_records.parquet_path` and
   flip state READ_ONLY → ARCHIVED.
4. Retire the EBS volume: snapshot (tagged with shard_id +
   parquet_path for lineage), force-detach, delete.

Op 6 (terminate-on-archive) then fires on the NEXT tick: the
snapshot sees state=ARCHIVED + non-empty `instance_id`, Apply
calls
`TerminateInstanceInAutoScalingGroup(ShouldDecrementDesiredCapacity=true)`
and nulls `instance_id`. `desired_capacity` drops in lockstep
with ASG actual, so scale-in policies never pick the wrong
instance to kill.

ARCHIVED is terminal. The ColdShard keeps serving queries from
S3 for as long as the Parquet file exists. There's no further
state transition.

Archive today is synchronous inside op 5's Apply — no separate
ARCHIVING state. If export time ever outgrows a tick
(multi-hour bulk exports), an ARCHIVING intermediate slots in
between READ_ONLY and ARCHIVED; queries would continue hitting
the DuckDB shard during the export and the ColdShard swap
would become an atomic tail operation. Not needed today.

## Coordinator contract

The coordinator's provisioner is the single writer of shard identity
and state. It runs a **per-bucket reconciler** every tick (and
synchronously from `OnboardApp`): the reconciler reads a snapshot
of bucket config + registry + in-flight LAUNCHING counts and emits
an ordered list of idempotent ops, which are then applied. Order
inside a tick: app-drift → window-closed → under-capacity →
pre-warm → retention-archive → terminate-on-archive. A separate
tier-level `syncASGDesiredCapacity` pass runs after the per-bucket
loop.

Concretely the provisioner owns:

- **Launch intent.** INSERT a LAUNCHING row in `shard_records`
  before the ASG ever learns about the new shard. The reconciler's
  per-bucket snapshot sees the row on the next tick so no duplicate
  launch fires.
- **ASG desired_capacity.** Once per tick per tier, compute
  `Σ bucket.target + count(READ_ONLY in tier) +
  count(future-window LAUNCHING in tier)` and call
  `SetDesiredCapacity(tier, desired)` as an absolute assignment.
  Target covers current-window LAUNCHING+INGESTING; READ_ONLY
  covers past-window shards still serving queries; future-window
  LAUNCHING covers pre-warmed next-window shards (op 4). Clamped
  to `MaxSize` with a log tripwire. This is the sole mechanism
  for ASG scale-up and scale-down; there is no per-instance
  bump.
- **Rotation scheduling.** "The current window ends in ≤`PrewarmLead`;
  mint W1 LAUNCHING rows now." Running on a ticker. Decision lives
  in `AppBucket.ShouldPrewarmNextWindow` — today date math against
  a coordinator-global `PrewarmLead`, swappable in future for
  burn-rate-based policies without changing callers.
- **Christening.** When an unclaimed heartbeat arrives (shard has no
  `shard.json`), find the LAUNCHING row the lifecycle hook stamped
  onto this instance, Configure the shard with the row's identity,
  UPSERT state LAUNCHING → INGESTING.
- **Window transition.** Move a shard INGESTING → READ_ONLY when
  wall-clock passes `WindowEnd`.
- **Archive scheduling.** Move READ_ONLY → ARCHIVING at retention
  boundary, kick off export.
- **Terminate-on-archive.** An ARCHIVED shard whose EC2 instance
  is still around gets reclaimed via
  `TerminateInstanceInAutoScalingGroup(ShouldDecrementDesiredCapacity=true)`.
  The `InstanceID` on the record gets nulled so the op doesn't
  re-fire. Paired with the ASG desired_capacity sync above: both
  the coordinator's `desired` calculation and the ASG's actual
  count drop in lockstep when a shard completes archive.
- **Failure recovery.** Detect dead instance, decide reap vs.
  recover based on shard state + retention.
- **Stale-LAUNCHING reap.** Sweep LAUNCHING rows older than the
  stuck-launch threshold. Empty `instance_id` → DELETE. Populated
  `instance_id` → ABANDON-signal the ASG and DELETE.
- **Registry sync.** The in-memory registry is a cache of
  `shard_records` for states INGESTING onward. LAUNCHING rows live
  only in the DB — there is nothing in-memory to cache. Every
  transition persists; Remove-from-registry pairs with
  delete-from-store for terminal states, update-for-state-transition
  for everything else.

Shards do nothing autonomously beyond:
- Write `shard.json` when told via Configure.
- Heartbeat `(shard_id, addr, instance_id, state, row_count,
  disk_bytes)`. Empty `shard_id` on first boot with a fresh volume;
  the durable id once Configure has landed.
- Accept writes.
- Answer queries.
- Export to S3 when told (if the hot-shard does the export; an
  alternative is the coordinator doing it remotely via DuckDB over
  gRPC).

The lifecycle hook is a third actor, run per-instance by AWS ASG on
`EC2_INSTANCE_LAUNCHING`. It bridges "ASG brought an instance up"
with "reconciler minted a LAUNCHING row." Its decisions are:

- **RECOVER** — an orphan EBS volume with a recognised `shard_id`
  exists in this tier+AZ. The hook attaches it to the new instance;
  the shard boots, reads its `shard.json`, heartbeats with its
  durable id, and the coordinator takes the rebind path.
- **CHRISTEN** — no orphan volume. Claim the oldest unmatched
  LAUNCHING row for this tier (stamp `instance_id` onto the row,
  create an EBS volume tagged with the row's `shard_id`, attach).
  The subsequent empty-id heartbeat promotes the row.
- **ABANDON** — ambiguous recovery (multiple orphan volumes), no
  LAUNCHING row to claim, or any error. The ASG terminates the
  instance.

### App onboarding path

A new app's first ingest row threads through onboarding inline,
in the receive path of `IngestStream`:

```
Recv row → (per-stream mu.Lock) →
    SeenApps.Add(customer, app)     // atomic dedup, true only first time
    └─ if true:
         OnboardApp(app)
         ├─ Buckets.AddAppToBucket  // DB write, ~5ms
         └─ reconcileBucket          // emits AddAppToShardOp per INGESTING shard;
                                     // Apply → registry.AddAppToShard (writer-
                                     // locked mutation + persist)
    row → batch → (mu.Unlock)
```

A few properties that fall out:

- **Rows for a new app never reach a shard before that shard's
  `Apps` list contains the app.** `OnboardApp` runs inside the
  stream mutex, same mutex the batch-flush path takes; by the
  time a batched row gets written the `AddAppToShardOp` has
  applied.
- **Registry mutations are lock-protected.** `AddAppToShard` takes
  the registry's write lock; concurrent readers
  (`IngestingShard`, `MatchingShards`) under RLock see a
  consistent `Apps` slice — no torn reads.
- **Residual cross-stream window.** Between `SeenApps.Add` and
  the registry write, a query on another goroutine filtering by
  the new app sees empty results. Duration is the
  `OnboardApp` latency (~5–10ms, dominated by the bucket DB
  write). Practically invisible because no shard holds data for
  the new app during that window either — stream A's first row
  is still in-batch, not yet ingested.

The `pending_sources` + `AddSource` RPC flow takes the same
`OnboardApp` call synchronously, so operator-registered apps are
propagated on the RPC's return.

## Known ambiguities to decide before implementation

- **Dedicated fleets for high-throughput apps.** Day-1 model is
  that every onboarded app joins the default fleet's `apps` list
  at christening. Later we will want the flexibility to carve out
  a high-throughput app onto its own fleet (possibly meatier
  instance types, different `w.duration` / `w.count`). The data
  model already supports this — just don't append the app to the
  default fleet and christen a separate fleet whose `apps = [that
  app]`. No schema change needed when we add the mechanism.
- **Spot vs. on-demand purchase model.** Shard ASG launch
  templates can request EC2 capacity as spot (cheaper, ~70% off
  on-demand, but AWS can reclaim with 2 min notice) or on-demand
  (stable). Spot viability depends on three things together:
  (a) recovery robust enough that reclaim = routine instance
  death; (b) DuckDB fsync cadence tight enough that the
  worst-case lost-writes window is acceptable; (c) mixed-instance
  policy so we fall back to on-demand when spot capacity tightens.
  **Day-1: on-demand.** Revisit once we have recovery working in
  prod and have measured fsync cadence.
- **Multi-AZ.** EBS is AZ-scoped, so a shard recovered after
  instance death must come up in the same AZ. A full-AZ outage
  blocks recovery for any shards in that AZ. Longer-term we want
  write shards intentionally split across AZs and cross-AZ
  snapshotting. See [az-resilience.md](az-resilience.md) for that
  design. Not day-1.

## Testing story

Three layers worth building out in lockstep with the above:

- **Unit** (`coordinator/` package) — christening, rotation
  scheduling, failure-recovery decision tree. All with mocked
  launcher + mocked store. Fast, deterministic, covers the state
  machine.
- **Integration** (`integration/` package, docker-compose) — full
  coordinator + shard containers with small bucket values for
  `WindowDuration` / `RetentionPeriod` (e.g. `5m`, `24h`) so
  rotation and retention fire inside a test lifetime. Real
  wall-clock time; no mock clock. Real Postgres via the same
  harness. LocalStack for EC2 where the test exercises EBS /
  instance paths; stubs for ASG API (LocalStack Community
  doesn't implement it). See
  `integration/prewarm_test.go`,
  `integration/terminate_on_archive_test.go`,
  `integration/lifecycle_test.go` for worked examples.
- **Prod smoke** (one-off, via `kevcons ssh` or a dedicated script)
  — after AMI rollout, kill an ASG instance mid-window and confirm
  a fresh instance picks up the EBS with the same shard id and no
  data loss. Not cheap to run; do it on deploys that touch this
  path.

Remaining work toward this target lives in
[`cleanup.md`](cleanup.md), which is the authoritative inventory of
open items and their sequencing.
