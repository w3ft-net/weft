# Shard Lifecycle

Status of the code as of this document: **intended design only**. The
gap analysis at the end of this doc lists the parts of the system that
don't yet match this model — several of them don't exist. Read the
"Gap analysis" and "What exists today" sections before touching code.

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
          ┌────▶│  INGESTING  │◀─┐───┐
          │     └─────────────┘  │   │
  coord.  │                      │   │ window
christens │     ┌─────────────┐  │   │ closes
new shard └─────│   ADOPTED   │  │   │
                └─────────────┘  │   ▼
                       ▲         │  ┌─────────────┐
                 fresh EBS,      │  │  READ_ONLY  │◀─┐
                 no identity     │  └─────────────┘  │
                                 │         │        rebind
                  rebind ────────┘         │        (known id
                  (known id)               │         at new
                                           │         addr)
                                           │
                                           │ retention
                                           │ boundary
                                           ▼
                                    ┌─────────────┐
                                    │  ARCHIVING  │◀─┐
                                    └─────────────┘  │
                                           │        rebind
                                           │ export to S3
                                           ▼
                                    ┌─────────────┐
                                    │  ARCHIVED   │  (Lambda-served cold)
                                    └─────────────┘
                                           │
                                           │ cold retention
                                           ▼
                                    ┌─────────────┐
                                    │  TERMINATED │
                                    └─────────────┘
```

- **ADOPTED** — an instance has come up with a **fresh EBS volume**
  (no `shard.json` on disk) and has reported in. The coordinator
  hasn't yet christened it with a window/app assignment. Only new
  shards in a brand-new fleet pass through this state.
- **INGESTING** — coordinator has assigned the shard a window and
  app set. Writes for rows whose timestamp falls in the window and
  whose app is in the set route here. Queries overlapping the
  window route here.
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

The ADOPTED state is only entered by a shard with a fresh EBS volume
and no `shard.json`. That's a brand-new shard in a brand-new fleet,
not a restarted one.

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
     "apps":         ["weft"]
   }
   ```
2. The shard process reads `shard.json` and heartbeats as that
   identity.
3. If `shard.json` doesn't exist (fresh EBS volume), the shard
   reports as ADOPTED / UNCLAIMED — no identity yet. The coordinator
   christens it on the next tick: assigns a shard id, window, app
   set, and tells the shard (via a Configure RPC) to write
   `shard.json`. That file is the durable record of identity.

EBS volume must be tagged with the shard id so the coordinator can
find "the volume that used to host shard X" when an instance dies.

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
and state. Concretely it owns:

- **Rotation scheduling.** "The current window ends in ≤24h; mint
  W1 shards now." Running on a ticker.
- **Christening.** When an ADOPTED shard appears, assign
  `(shard_id, window, apps)`, persist to `shard_records`, tell the
  shard to write its `shard.json`, transition to INGESTING.
- **Window transition.** Move a shard INGESTING → READ_ONLY when
  wall-clock passes `WindowEnd`.
- **Archive scheduling.** Move READ_ONLY → ARCHIVING at retention
  boundary, kick off export.
- **Failure recovery.** Detect dead instance, decide reap vs.
  recover based on shard state + retention.
- **Registry sync.** The in-memory registry is a cache of
  `shard_records`. Both must agree. Remove-from-registry always
  pairs with delete-from-store for terminal states (TERMINATED),
  and with update-for-state-transition for everything else.

Shards do nothing autonomously beyond:
- Write `shard.json` when told.
- Heartbeat `(shard_id, addr, state, row_count, disk_bytes)`.
- Accept writes.
- Answer queries.
- Export to S3 when told (if the hot-shard does the export; an
  alternative is the coordinator doing it remotely via DuckDB over
  gRPC).

## What exists today

Read this before implementing anything above.

- **Identity**: shards today get `-id=<something>` at launch. For
  ASG-launched shards, `<something>` is the EC2 instance id, so
  identity is coupled 1:1 with the instance. For local/docker
  shards, a timestamp is used. `shard_records.id` reflects whichever
  was used. There is no `shard.json` on disk.
- **EBS**: ASG launch templates today use ephemeral EBS
  provisioned per-launch. Nothing attaches/detaches volumes by
  shard id. When an instance dies, its volume dies with it.
- **Windows**: the heartbeat proto used to carry `window_start` /
  `window_end` / `apps`; those were removed in commit `f4c310e`
  (Apr 14) with the comment "coordinator manages windows now." The
  coordinator-side assignment was never written. Result:
  ASG-adopted shards have `WindowStart = WindowEnd = 0001-01-01T00:00:00Z`
  in the registry and in `shard_records`. Only local-launcher
  shards (via `provisioner.launchAndConfigure`, used by
  `bootstrap` in dev) have real windows.
- **Rotation**: partial. The provisioner's `check()` loop (see
  `provisioner.go`) flips INGESTING shards to READ_ONLY on the
  wall-clock `WindowEnd` boundary, and the bucket-sizing pass
  below provisions replacements (via ASG in prod, via
  `launchAndConfigure` in dev/docker). What's still missing from
  the target design: no handoff-lead pre-warming of the next
  window's fleet, no N-fleet overlap tracking, and no archive-on-
  retention boundary (that path exists in `archive()` but only
  fires when `RetentionPeriod > 0 && ParquetDir != ""`, both
  opt-in).
- **Archiving**: `StateExpiring` and `StateArchived` exist in the
  registry enum. The export path exists in `coordinator/archiver*`
  and `shard/cold_shard*`. Coordinator-driven scheduling of
  EXPIRING → ARCHIVING → ARCHIVED on a retention boundary does
  **not** exist; today the transitions happen only when called
  manually via an RPC.
- **Failure recovery**: `provisioner.reapDeadShards` removes
  heartbeat-stale entries from the registry and calls
  `launcher.TerminateInstance`. There is no branch that asks "is
  the shard's data still needed?" — everything that stops
  heartbeating is reaped. Data loss is therefore silent when an
  instance dies mid-window.

## Gap analysis

Things that must be built or changed for the system to actually
match this doc. Numbered so we can track them.

1. **Durable shard identity on EBS.** Shards read/write
   `/opt/app/data/shard.json`. A `Configure` RPC from the
   coordinator is the one thing allowed to write it. Heartbeats
   report the identity read from disk, not from `-id=`.

2. **Coordinator christening path for ADOPTED shards.** Today
   `adoptFromHeartbeats` quietly accepts anything heartbeating and
   slots it into `StateIngesting` with zero-value windows. It must
   instead:
   - Recognise a known shard id (heartbeat's `shard_id` already in
     the registry) → rebind path: update `addr` / `instance_id` /
     gRPC handle, clear `Suspended`, preserve state. No christening.
   - Recognise an unclaimed new instance (shard reports "no
     identity on disk") → christen: mint the next shard name for
     the active window, populate `apps` from the current onboarded
     set (see gap item 9), write `shard.json` via a Configure RPC,
     transition to INGESTING.

3. **Rotation on wall-clock windows.** A ticker in the provisioner
   that, when the current window is within some lead time of its
   end, christens the next fleet, sized per tier. Distinct from
   the row-count / disk-bytes rotation logic currently in
   `check()`, which should probably just go away once windows are
   real.

4. **EBS per-shard attach/detach.** Several related pieces:
   a. **Naked launch templates** for the shard ASGs — no
      `block_device_mappings`. New instances come up with the root
      volume only; the shard's data volume is always attached from
      outside by the coordinator. Prevents the collision where an
      ASG-launched replacement boots with its own empty volume
      that would race our re-attach.
   b. **Tag volumes with `shard_id`** at creation. The tag is the
      authoritative `shard → volume` binding; operators can find a
      shard's volume with `describe-volumes
      --filters Name=tag:shard_id,Values=<name>`.
   c. **Cache `ebs_volume_id` in `shard_records`** to avoid a
      describe-volumes call on every recovery, and to make the
      binding visible in Postgres.
   d. **Recovery orchestration** uses the ASG-friendly dance:
      `TerminateInstanceInAutoScalingGroup` (with
      `ShouldDecrementDesiredCapacity=false` so the ASG launches
      our replacement), then `DetachVolume(force)` →
      `AttachVolume(new-instance)`.
   e. **IAM**: the coordinator instance role needs
      `ec2:DescribeVolumes`, `ec2:AttachVolume`, `ec2:DetachVolume`,
      `ec2:CreateVolume`, `ec2:DeleteVolume`, `ec2:CreateTags`.

5. **Failure-recovery branch.** Before reaping, ask: is this
   shard's window within hot retention and not ARCHIVED? If so,
   recover (re-attach EBS into ASG-launched replacement) rather
   than reap. Only fully-expired shards get reaped and terminated.

6. **Archive scheduling on retention boundary.** Currently export
   is manual. Should be driven by the same ticker that does
   rotation: when wall-clock − `WindowEnd` > hot-retention, move
   to ARCHIVING and invoke the existing exporter.

7. **Registry ↔ store consistency on termination.** `shard_records`
   currently accumulates rows for long-dead instances.
   `Registry.Remove` must always reach store and delete the row for
   TERMINATED shards; transitional states must update, not delete.

8. **Clean up existing `shard_records` cruft.** One-time: any row
   whose `id` is an `i-…` with zero-value window and no recent
   heartbeat is a legacy artifact of the old model and can be
   deleted. A migration-or-script can do it.

9. **`apps` populated at christening; appended on app onboarding.**
   Every shard record always has a non-empty `apps` list — there's
   no "NULL means catch-all" special case. At christening,
   populate from the current set of onboarded apps for the
   namespace. When a new app is onboarded (via weftd onboarding or
   CLI), append it to the `apps` list on every INGESTING shard
   that should host it (= all current write shards, until the
   dedicated-fleet mechanism in a future iteration carves out
   exceptions). Query routing is then a direct membership check
   (`app ∈ shard.apps`), no special cases.

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

## Rollout plan

This doc is the target. Actual work should land in roughly this
order so each step is independently useful and testable:

1. **#1 + #2** together: durable identity on EBS, christening
   path. Without rotation or recovery the system still works
   (instances come up, get christened once, stay christened,
   windows are real). Unblocks queries immediately. Sets up the
   invariants everything else depends on.
2. **#7 + #8**: registry/store consistency + one-time cleanup.
   Cheap, makes the state table observable.
3. **#4**: EBS attach/detach. Needs the IAM changes and a small
   change to the shard ASGs' launch templates. Test by manually
   terminating one instance and watching the recovery.
4. **#5**: failure-recovery decision logic on top of #4. Guarded
   by a flag initially so we can smoke-test one shard at a time.
5. **#3 + #6**: rotation + archive scheduling. These are the
   "proactive" parts of the provisioner — they change behavior at
   window boundaries. Last because the biggest behaviour change.
