# Shard Lifecycle

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

## Shard states

```
                ┌─────────────┐
                │  LAUNCHING  │  (row in shard_records;
                └─────────────┘   no instance yet, or
                       │          instance not yet heart-
                       │ instance beating)
                       │ heartbeats
                       ▼
                ┌─────────────┐
                │  INGESTING  │◀─┐
                └─────────────┘  │ rebind
                       │         │ (known id
                       │         │  at new addr)
                       │ window  │
                       │ closes  │
                       ▼         │
                ┌─────────────┐  │
                │  READ_ONLY  │◀─┘
                └─────────────┘
                       │
                       │ retention
                       │ boundary
                       ▼
                ┌─────────────┐
                │  ARCHIVING  │◀─┐ rebind
                └─────────────┘  │ (export resumes
                       │         │  on new instance)
                       │ export  │
                       │ to S3   │
                       ▼         │
                ┌─────────────┐  │
                │  ARCHIVED   │◀─┘ (Lambda-served cold)
                └─────────────┘
                       │
                       │ cold retention
                       ▼
                ┌─────────────┐
                │  TERMINATED │
                └─────────────┘
```

- **LAUNCHING** — a persistent row in `shard_records` that
  represents the reconciler's intent to bring up a new shard. Has a
  minted `shard_id`, heartbeat token, bucket, window, and tier, but
  (at first) no `instance_id` and no `addr`. The lifecycle hook
  stamps `instance_id` onto the oldest unmatched row for the tier
  when AWS brings up an instance; the instance's first heartbeat
  promotes the row to INGESTING. Synchronous launchers (LocalLauncher
  for dev/test) skip this state — they Configure the shard inline
  before it ever heartbeats.
- **INGESTING** — Configure RPC has been sent; shard is registered
  and serving writes + queries for its `(window, apps)`. Writes for
  rows whose timestamp falls in the window and whose app is in the
  set route here. Queries overlapping the window route here.
- **READ_ONLY** — wall-clock time has passed the window's end. No
  new writes arrive. Queries still route here until the retention
  boundary.
- **ARCHIVING** — the shard is past the hot retention boundary. The
  coordinator has kicked off a Parquet export to S3. Queries still
  route here while the export is in progress.
- **ARCHIVED** — the export is complete, the Parquet file is on S3,
  the `lambda-cold-shard` function can serve queries against it.
  The hot-shard EBS volume is no longer needed.
- **TERMINATED** — instance stopped, EBS deleted, row removed from
  `shard_records`.

Exactly one state transition per boundary — the coordinator is the
only actor that moves a shard from one state to the next. Instances
don't self-transition.

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

Two configuration knobs govern the whole scheme:

- `w.duration` — how long a single window covers (e.g. `7d`).
- `w.count` — how many hot windows are retained simultaneously
  (e.g. `2`: current and previous). Hot retention = `w.count *
  w.duration`.

The interesting part of the lifecycle is what happens near the end of
a hot window. Walkthrough assumes `w.duration = 7d`, `w.count = 2`
(so: current window ingesting, previous window still hot for reads).

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

Window handoff (approaching W0's end, call it T - handoff_lead, with
`handoff_lead` configurable, nominally 24h):

```
       │ W-prev       │ W0               │ W1
───────┼──────────────┼──────────────────┼──────────────────
       │              │                  │
       │  READ_ONLY   │  INGESTING       │  INGESTING (new,
       │  or          │  (old fleet,     │   just christened,
       │  ARCHIVING   │   primary)       │   empty)
```

For the last `handoff_lead` interval of W0, **both W0 and W1 fleets
are INGESTING**. The coordinator christens W1's shards early so
they're warmed up, christened, and in the registry before cutover.
Writes route by timestamp: rows whose ts falls in W0 still go to
W0's fleet; rows crossing into W1 go to W1's fleet.

At boundary crossing, W0 transitions to READ_ONLY and what used to
be W-prev transitions to ARCHIVING.

Fleet-count math in terms of the config:

- At steady state: `w.count` fleets hot (one INGESTING, the rest
  READ_ONLY or ARCHIVING).
- At handoff: `w.count + 1` fleets briefly — `w.count` existing
  plus one warming. For `w.count = 2`, that's the "3× per-window
  fleet size" I had written before.

Worst-case provisioner capacity planning must reserve for
`(w.count + 1) × fleet_size`, not `w.count × fleet_size`.

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

Happy path:

1. Instance I-A hosts shard S, EBS volume V-S attached.
2. I-A dies (SIGKILL, hardware failure, spot reclaim — whatever).
3. Heartbeat for S stops. Coordinator notices after ~2 minutes.
4. Coordinator's failure path — distinct from the "reap" path:
   - Check: is the shard's window still within hot retention and
     not yet ARCHIVED?
     - **No** → reap normally; terminate instance; don't replace.
     - **Yes** → recover.
5. Recovery (issued via ASG APIs so we don't race the ASG):
   1. Look up V-S by `shard_id` — either from `shard_records.ebs_volume_id`
      (cached) or by listing volumes filtered on
      `tag:shard_id=<name>` (authoritative, tags are the source of
      truth).
   2. `DetachVolume V-S` with `Force: true` (I-A is dead so the
      soft detach would hang).
   3. Call `TerminateInstanceInAutoScalingGroup` on I-A with
      `ShouldDecrementDesiredCapacity = false`. The ASG launches
      I-B from the (naked) launch template to maintain desired
      capacity. **Critical**: the launch template must not attach
      an EBS volume of its own — see Gap #4 — otherwise I-B boots
      with an empty volume and our attach in the next step races
      against its existing one.
   4. Poll for I-B to reach `InService`, then `AttachVolume V-S`
      to I-B at the agreed device path (`/dev/sdf`).
   5. I-B's startup script mounts the volume, reads
      `/opt/app/data/shard.json`, starts weft-shard with the
      recovered identity.
   6. I-B heartbeats with the existing shard id — this is the
      rebind path described above. Coordinator updates `addr` /
      `instance_id`, keeps state/window/apps.

Nothing about the shard's identity, window, or data changed during
the recovery. From a writer's or querier's perspective, there was a
~2–3 minute interruption.

Pre-existing constraints to be mindful of:
- EBS volumes are AZ-scoped. The recovery instance has to come up
  in the same AZ. Either every shard's ASG is pinned to one AZ, or
  the launcher tracks the volume's AZ and launches there. See
  [az-resilience.md](az-resilience.md) for the longer-term design
  where write shards intentionally span AZs.
- `ec2:AttachVolume`, `ec2:DetachVolume`, `ec2:DescribeVolumes`,
  and `autoscaling:TerminateInstanceInAutoScalingGroup` are needed
  by the coordinator. Some are already granted; see Gap #4 for the
  missing set.

## Archiving and cold storage

Nothing in this section is a new mechanism — the export path, the
cold-storage bucket, and the Lambda cold-shard function already
exist. Missing piece is the **scheduling**: calling the existing
export at the right time on a window boundary.

At the retention boundary (shard's window older than
`w.count * w.duration` past its end), the coordinator:

1. Transitions the shard to ARCHIVING.
2. Invokes the existing shard-side exporter (`shard/cold_shard*`).
   The shard writes its DuckDB contents to Parquet and uploads
   directly to S3 at a canonical path under `weft-cold-<acct>/…`.
   The bucket already exists in
   `infrastructure-catalog/stacks/asg-weft-application` — no
   terraform change.
3. Records the resulting Parquet path in `shard_records.parquet_path`.
4. Transitions to ARCHIVED.
5. Terminates the instance via ASG and deletes the EBS volume.
6. Subsequent queries overlapping the window route to the Lambda
   `cold-shard` invoker (already wired) instead of the hot shard
   that's no longer there.

Queries during ARCHIVING still hit the hot shard until transition
completes — no double-count, no gap.

Rebind during ARCHIVING: if the hot shard's instance dies mid-export
and gets recovered, the export is interrupted. Current implementation
restarts the export from scratch on the new instance; incremental
resume is not required (the export is idempotent as long as we write
to a distinct object key per run and finalise atomically).

## Coordinator contract

The coordinator's provisioner is the single writer of shard identity
and state. It runs a **per-bucket reconciler** every tick (and
synchronously from `OnboardApp`): the reconciler reads a snapshot
of bucket config + registry + in-flight LAUNCHING count and emits
an ordered list of idempotent ops, which are then applied. Each
responsibility below is one op type; order inside a tick is
app-drift → window-closed → under-capacity → retention-archive.
Future responsibilities (pre-warm, terminate-on-archive) slot in as
additional ops without reshaping the scaffold.

Concretely the provisioner owns:

- **Launch intent.** INSERT a LAUNCHING row in `shard_records`
  before the ASG ever learns about the new shard. The reconciler's
  per-bucket snapshot sees the row on the next tick so no duplicate
  launch fires.
- **ASG desired_capacity.** Once per tick per tier, compute
  `Σ bucket.target + count(READ_ONLY entries in tier)` and call
  `SetDesiredCapacity(tier, desired)` as an absolute assignment.
  Clamped to `MaxSize` with a log tripwire. This is the sole
  mechanism for ASG scale-up and scale-down; there is no
  per-instance bump.
- **Rotation scheduling.** "The current window ends in ≤`PrewarmLead`;
  mint W1 LAUNCHING rows now." Running on a ticker.
- **Christening.** When an unclaimed heartbeat arrives (shard has no
  `shard.json`), find the LAUNCHING row the lifecycle hook stamped
  onto this instance, Configure the shard with the row's identity,
  UPSERT state LAUNCHING → INGESTING.
- **Window transition.** Move a shard INGESTING → READ_ONLY when
  wall-clock passes `WindowEnd`.
- **Archive scheduling.** Move READ_ONLY → ARCHIVING at retention
  boundary, kick off export.
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
  coordinator + shard containers with small real values for
  `w.duration` / `w.count` (e.g. `30s`, `2`) so rotation and
  retention boundaries fire inside a test lifetime. Real
  wall-clock time; no mock clock. Covers happy-path rotation and
  failure-recovery with real DuckDB.
- **Prod smoke** (one-off, via `kevcons ssh` or a dedicated script)
  — after AMI rollout, kill an ASG instance mid-window and confirm
  a fresh instance picks up the EBS with the same shard id and no
  data loss. Not cheap to run; do it on deploys that touch this
  path.

Remaining work toward this target lives in
[`cleanup.md`](cleanup.md), which is the authoritative inventory of
open items and their sequencing.
