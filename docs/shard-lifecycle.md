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
          ┌────▶│  INGESTING  │──────┐
          │     └─────────────┘      │
  coord.  │                          │ window
christens │     ┌─────────────┐      │ closes
new shard └─────│   ADOPTED   │      │
                └─────────────┘      ▼
                       ▲      ┌─────────────┐
                  adopt from  │  READ_ONLY  │
                  heartbeat   └─────────────┘
                                     │
                                     │ retention
                                     │ boundary
                                     ▼
                              ┌─────────────┐
                              │  ARCHIVING  │
                              └─────────────┘
                                     │
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

- **ADOPTED** — an instance has come up with an EBS volume attached
  and has reported in. The coordinator recognises the shard (by the
  identity stored on the volume) but has not yet christened it with
  a window/app assignment. New instances for an existing shard also
  briefly pass through this state.
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

## Rotation

The interesting part of the lifecycle is what happens near the end of
a hot window. Assume 7-day hot retention and a 7-day write window.

Steady state (call it day N within the current window):

```
       │ window W0                 │ window W1 (future)
───────┼───────────────────────────┼───────────────
  day  │  7..6..5..4..3..2..1..0   │
       │                           │
       │  ingesting: shards owned  │
       │  by W0 (1+ per tier)      │
```

Hot storage covers one window at a time. All writes go to W0's
shards.

Window handoff (approaching W0's end, call it T-24h):

```
       │ window W0            │ window W1
───────┼──────────────────────┼────────────────
       │                      │
       │  ingesting W0        │  ingesting W1
       │  (old fleet)         │  (new fleet, just
       │                      │   came up, empty)
```

For the last ~24h of W0, **both fleets are live**. The coordinator
christens W1's shards early so they're warmed up and in the registry
before the cutover. Writes for a row are routed based on its
timestamp: rows in W0's window still go to W0's fleet; rows crossing
into W1 start going to W1's fleet.

Counts to keep in your head:
- 1 fleet ingesting at any instant (nominal).
- Up to 2 fleets in INGESTING state overlapping for the last 24h of
  a window (while W1 is christened but W0 is still authoritative
  for in-window writes).
- Add N fleets in READ_ONLY / ARCHIVING for however many past
  windows are still within hot retention. With 7-day hot retention
  and 7-day windows, N=1 at steady state (the previous window).

Worst-case bottom-of-window shard count = **3 × per-window fleet
size** briefly: W-prev (READ_ONLY or ARCHIVING), W0 (INGESTING,
primary), W1 (INGESTING, warming). Provisioner planning must account
for this, not collapse to "desired_capacity = per-window size".

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
5. Recovery:
   1. Detach V-S from I-A (even though I-A is dead, the API call
      may need `Force: true`).
   2. Launch I-B in the shard's ASG via the launcher.
   3. Attach V-S to I-B at `/dev/xvdf` (or whatever).
   4. I-B's startup script mounts the volume, reads
      `/opt/app/data/shard.json`, starts weft-shard with the
      recovered identity.
   5. I-B heartbeats. Coordinator observes the shard is back at a
      new addr, updates the registry entry's `addr` / `instance_id`,
      keeps everything else.

Nothing about the shard's identity, window, or data changed during
the recovery. From a writer's or querier's perspective, there was a
~2–3 minute interruption.

Pre-existing constraints to be mindful of:
- EBS volumes are AZ-scoped. The recovery instance has to come up
  in the same AZ. Either every shard's ASG is pinned to one AZ, or
  the launcher handles AZ-to-subnet mapping.
- `ec2:AttachVolume` and `ec2:DetachVolume` are not in the current
  coordinator IAM policy (see Gap analysis #4).

## Archiving and cold storage

At the retention boundary (e.g. hot shard N days past window end),
the coordinator:

1. Transitions the shard to ARCHIVING.
2. Triggers the Parquet export: a DuckDB query on the shard writes
   all rows into a Parquet file at a canonical S3 path, e.g.
   `s3://weft-cold-<acct>/<yyyy>/<mm>/<dd>/<shard_id>.parquet`.
3. Records the Parquet path in `shard_records`.
4. Transitions to ARCHIVED.
5. Terminates the instance, detaches & deletes the EBS volume.
6. Subsequent queries overlapping the window route to the Lambda
   cold-shard invoker instead of the now-gone hot shard.

Queries during ARCHIVING still hit the hot shard until transition
completes — no double-count, no gap.

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
- **Rotation**: not implemented. The provisioner's `check()` loop
  (see `provisioner.go`) rotates based on row count / disk bytes /
  `MaxDuration` (default 8 hours), not windows, and only creates
  the next shard in series — no notion of N-fleet overlap or
  wall-clock window alignment.
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
   - Recognise a known shard id → update addr, move on.
   - Recognise an unclaimed new instance (no shard id in
     heartbeat) → assign identity, Configure, transition to
     INGESTING.

3. **Rotation on wall-clock windows.** A ticker in the provisioner
   that, when the current window is within some lead time of its
   end, christens the next fleet, sized per tier. Distinct from
   the row-count / disk-bytes rotation logic currently in
   `check()`, which should probably just go away once windows are
   real.

4. **EBS per-shard attach/detach.** Two sub-problems:
   a. Tag volumes with `shard_id` at creation; don't delete on
      instance termination; make the shard ASG's launch template
      create the volume separately.
   b. On failure recovery, orchestrate
      `Detach(force) → Attach(to new instance) → wait`. Needs
      `ec2:AttachVolume`, `ec2:DetachVolume`, `ec2:CreateVolume`,
      `ec2:DeleteVolume` in the coordinator IAM policy.

5. **Failure-recovery branch.** Before reaping, ask: is this
   shard's window within hot retention and not ARCHIVED? If so,
   recover (re-launch + re-attach EBS) rather than reap. Only
   fully-expired shards get reaped and terminated.

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

## Known ambiguities to decide before implementation

- **Per-app or shared shards?** The `apps` field is modelled as a
  set. Today most rows have `null`. Decide whether per-app
  partitioning is a day-1 feature or deferred. Either way,
  adopting the identity-on-EBS model doesn't block it.
- **Exporter location.** Hot shard exports itself to S3 vs.
  coordinator exports it remotely. The existing
  `coordinator/archiver*` suggests coordinator-side; the
  `shard/cold_shard*` path suggests shard-side. Pick one.
- **Spot vs. on-demand for shards.** Spot reclaims make the
  failure-recovery path hot. If recovery is robust, spot is
  viable. If not, on-demand only.
- **Cross-AZ failure.** EBS is AZ-scoped. If an entire AZ drops,
  recovery can't proceed until the AZ comes back. Acceptable?
  Otherwise we need snapshot-based recovery into a different AZ
  (slower, more $).

## Testing story

Three layers worth building out in lockstep with the above:

- **Unit** (`coordinator/` package) — christening, rotation
  scheduling, failure-recovery decision tree. All with mocked
  launcher + mocked store. Fast, deterministic, covers the state
  machine.
- **Integration** (`integration/` package, docker-compose) — full
  coordinator + shard containers, fake time advance via a mockable
  clock hook so we can simulate "7 days passed" in milliseconds.
  Covers the happy-path rotation and failure-recovery with real
  DuckDB.
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
