# Cleanup

Open items from the 2026-04-20/21 session. Grouped by theme. Each entry
covers what's missing today, why it matters, and what the change shape
looks like. "Size" is a rough estimate, assuming familiarity with the
code around it.

## 1. Reconciler rewrite of `check()` — partial

**Status.** Scaffold landed (`coordinator/reconciler.go`):
`ReconcileOp` interface, `ReconcilerSnapshot`, a pure
`reconcile(snap) []ReconcileOp` function, and `reconcileBucket`
helpers wire it into the ticker and into `OnboardApp`. Four of the
six ops from the target table are in:

| # | Drift | Op | Status |
|---|---|---|---|
| 1 | app-drift | `AddAppToShardOp` | landed |
| 2 | under-capacity | `LaunchShardOp` | landed (replaced `scaleBucketsToTarget` + `effectiveCapacity`) |
| 3 | window-closed | `RotateToReadOnlyOp` | landed (replaced Path A) |
| 4 | pre-warm next window | (future) | pending — see §5 |
| 5 | retention-archive | `ArchiveShardOp` | landed (per-bucket retention is authoritative; `ProvisionerConfig.RetentionPeriod` dropped) |
| 6 | terminate-on-archive | (future) | pending — see §4 |

`check()` is now just bucket-split evaluation + a `reconcileBucket`
loop. Ops 4 and 6 slot in as new op types without further scaffold
changes; they belong to §5 and §4 respectively.

**Depends on.** §2 (LAUNCHING state, done), §3 (ASG desired
wiring).

## 2. `LAUNCHING` state in `shard_records` — done

**Status.** Landed. Reconciler INSERTs LAUNCHING rows before calling
the ASG (`provisioner.mintLaunchingRow`); lifecycle hook claims the
oldest unmatched row via `ClaimLaunchingForTier` and tags the EBS
volume with the claimed shard id; the subsequent heartbeat promotes
the row to INGESTING through `christen` (strict — no mint-at-christen
fallback). Stale-LAUNCHING reaper deletes orphan rows every tick.
Effective capacity is the DB's `COUNT(LAUNCHING + INGESTING)` for
async launchers; the registry-only count remains only for
synchronous launchers (LocalLauncher, tests) where there is nothing
async to coordinate with. Choice of path flows from
`asyncLauncher.UsesLaunchingRows()`, not from store availability.

**Original write-up below, preserved for context.**

**Today.** `StateLaunching` is defined in `coordinator/registry.go:18`
but nothing uses it. When the reconciler wants to scale up, it has no
persistent record that "a launch is in flight" — next tick's count
read is missing the pending instance and it'll issue another launch.

**Target.** A LAUNCHING row represents a reconciler's intent to have a
new shard come up. Lifecycle:

```
reconciler       lifecycle hook      heartbeat
     │                 │                │
     ▼                 ▼                ▼
LAUNCHING  ──────► LAUNCHING  ───► INGESTING
(pre-asg)   (stamps instance_id)   (christen upgrades state)
```

1. Reconciler mints a shard id, INSERTs LAUNCHING row with
   `bucket_id`, intended window, empty `instance_id`. Calls the
   ASG to scale up.
2. ASG launches an instance. Lifecycle hook fires, finds the oldest
   unmatched LAUNCHING row for this tier and stamps `instance_id`
   onto it. Attaches EBS tagged with the row's shard id.
3. Instance boots with fresh volume → heartbeats empty shard_id.
   `ProcessHeartbeat.christen` looks up the LAUNCHING row by
   `instance_id`, sends `Configure` using the row's fields, UPDATEs
   state LAUNCHING → INGESTING.

Counts flow naturally: `SELECT COUNT(*) WHERE bucket_id=$b AND state
IN ('LAUNCHING', 'INGESTING')` is the single source of truth for
"effective capacity in this bucket."

**Cleanup.** Stale-LAUNCHING reaper: rows older than ~10 minutes
without an `instance_id` get DELETEd (launch failed, ASG capacity
exhausted, bad AMI). Rows with an `instance_id` older than the same
window get ABANDON-signalled to the ASG and deleted.

**Size.** Medium (~200 lines across the reconciler, lifecycle hook,
and ProcessHeartbeat christen path).

## 3. ASG `desired_capacity` driven by the reconciler

**Today.** TF sets `aws_autoscaling_group.shard_small.desired_capacity
= var.shard_desired` (default 2). The coordinator's
`ASGLauncher.LaunchInstance` bumps it via `SetDesiredCapacity(+1)` on
every bucket-sizing call — ratchets under any launch latency because
nothing tracks in-flight launches (see §2). No path ever decreases
desired_capacity.

**Target.** ASG `desired_capacity` is coordinator-managed, driven by a
simple formula:

```
desired_capacity(tier) = COUNT(*) FROM shard_records
                          WHERE tier = $tier
                            AND state IN ('LAUNCHING','INGESTING','READ_ONLY','ARCHIVING')
```

Once per reconciler tick, per tier: compute that number, call
`SetDesiredCapacity(tier, desired)`. No more incremental bumps; this
is an absolute assignment.

**TF companion change.**

```hcl
resource "aws_autoscaling_group" "shard_small" {
  # desired_capacity removed — coordinator-managed at runtime
  min_capacity = 0
  max_capacity = var.shard_small_max   # see below
  lifecycle {
    ignore_changes = [desired_capacity]
  }
}
```

`max_capacity` is the static upper bound. Needs to accommodate peak
handoff where all buckets in the tier are pre-warming at once:

```
max_capacity(tier) = Σ (2 + ceil(retention_b / window_b)) * target_b
                       for bucket b with tier=tier
                   + headroom (~25%)
```

For the day-1 small tier with only the default bucket (target=2,
retention=7d, window=7d): `(2+1)*2 = 6`, +2 headroom ⇒ 8.

A guard: if the reconciler's computed `desired > max_capacity`, log +
clamp. That's the tripwire that says "someone added a bucket to this
tier without bumping max."

**Size.** Small (~50 lines reconciler + TF changes) but depends on
§1+§2 being in place first.

## 4. Terminate-on-archive

**Today.** `archive()` exports the shard's DuckDB to Parquet, swaps in
a ColdShard, marks ARCHIVED, calls `retireVolume` (snapshot + detach +
delete EBS). But the **EC2 instance** that hosted the shard is still
running. ASG still counts it toward `desired_capacity`. Nothing
reclaims it.

**Target.** After `retireVolume` completes, for each ARCHIVED row with
a non-empty `instance_id`:

```go
asg.TerminateInstanceInAutoScalingGroup(
    InstanceId: aws.String(entry.InstanceID),
    ShouldDecrementDesiredCapacity: aws.Bool(true),
)
```

Then null out `instance_id` on the row so we don't re-fire. The ASG
terminates the specific instance we name (not one of its own choosing
via OldestInstance / scale-in policy).

**Why targeted.** ASG scale-in policies choose an instance by
heuristic — oldest, lowest protection score, etc. We need to terminate
exactly the instance whose shard just archived, not whichever the ASG
picks. The `TerminateInstanceInAutoScalingGroup` API (distinct from
`ec2:TerminateInstances`) decrements desired atomically and targets a
specific instance.

**IAM.** Coordinator role needs
`autoscaling:TerminateInstanceInAutoScalingGroup` on the shard ASGs.

**Size.** Small (~30 lines + IAM).

## 5. Pre-warm + window-rotation rechristen

**Today.** `check()` Path A flips INGESTING → READ_ONLY at
`WindowEnd`. Nothing creates a new INGESTING shard for the next
window. Instances keep running with READ_ONLY shards bound; ASG count
doesn't change; no new writes have anywhere to go.

**Target.** Reconciler's size-drift op (from §1) handles this once the
plumbing is in place: after Path A transitions the current-window
INGESTING shards to READ_ONLY, `COUNT(INGESTING+LAUNCHING) for bucket`
drops below target and the reconciler inserts LAUNCHING rows for the
new window.

Pre-warm is a separate reconciler op: when `WindowEnd - PrewarmLead ≤
now < WindowEnd` on a current-window INGESTING shard **and** no
LAUNCHING row exists for the next window in that bucket, insert N
LAUNCHING rows for window+1 immediately. This gives the ASG and
lifecycle hook time to bring up fresh instances before the old
shards transition.

`PrewarmLead` is a bucket-level config (or global, initially — call
it 5 minutes for dev, 1 hour for prod) and should be < min acceptable
write-gap at a boundary.

**Size.** Small as a reconciler op once §1+§2 exist. Requires the
`Configure` RPC to accept a bucket's current `apps` snapshot — so
pre-warmed shards get whatever apps were onboarded up to the moment
they were christened, not stale values from when the ASG launched
them.

## 6. App-onboarding race window

**Today.** First row for a new app arrives on IngestStream:

```
IngestStream handler       SeenApps     OnboardApp         propagateAppsToIngesting
        │                     │             │                          │
        ▼                     ▼             ▼                          ▼
   row arrives  ──► drain   SeenApps.Add  AddAppToBucket      UPDATE shards.apps
   (Ring buffer)    (ASYNC)  fires ONCE    (persists)        (in-memory + DB)
        │            │
        ▼            ▼
   drain picks    meanwhile: no shard.hasApp(new_app) returns true
   IngestingShard  so IngestingShard(new_app) = nil
   (new_app)      → falls to DefaultIngestingShard (any INGESTING)
```

The drain's fallback to `DefaultIngestingShard` rescues the first row
— it lands on *some* shard's DuckDB. But while `OnboardApp` is
still in flight (DB write + registry mutation), a concurrent **query**
for the new app finds no matching shard via `hasApp` and misses the
fresh row. Window is millisecond-scale but non-zero.

**Target.** Options, ordered by invasiveness:

- **Accept the race.** Document that a subset of rows in the first
  few hundred ms of a new app's life may be invisible to queries
  issued in that same window. Probably fine for most use cases.
- **Pre-buffer on first sight.** Hold the batch in the ring until
  `OnboardApp` completes for that app; release then. Need a
  SeenApps-aware gate on `Ingest.Write`.
- **Synchronously OnboardApp before drain.** Move the OnboardApp
  call into the IngestStream *receive* path (not drain), blocking the
  first row until propagation completes. Adds latency to the first
  row only.

None feel urgent; first option is the cheap status-quo.

**Size.** Small (options 2 or 3 each ~30 lines). Depends on nothing.

## 7. Lifecycle hook consultation of reconciler intent

**Today.** `LifecycleHandler.decide()` picks christen vs recover vs
abandon purely on orphan-volume existence (`lifecycle.go:151`). It
has no knowledge of whether the reconciler *asked* for this instance
to be a new shard (christen) or is getting an unprompted ASG
auto-replacement (recover, if a volume exists).

**Target.** Hook consults the LAUNCHING-row table (from §2):

```go
if pending := h.findPendingLaunchForTier(tier, az); pending != nil {
    // Reconciler is waiting on us. Stamp instance_id, christen path.
    return DecisionChristen, ...
}
// No intent — check for orphan volume, fall back to abandon.
```

Without this, the reconciler's "I want +1 fresh shard" can lose to
the hook's "orphan volume exists, let's recover" if an old
recoverable volume happens to be in the same AZ. We'd get an old
shard back when we wanted a new one.

**Size.** Small (~50 lines in `lifecycle.go`). Depends on §2.

## 8. Runtime config reload

**Today.** `reload-config.sh` (in the infra repo's ansible role) fires
on SSM config-version bumps. It calls `fetch-config.sh` (which now
correctly delegates to `load-app-env` — fixed in commit `8935137`) and
then `systemctl restart weft-coordinator`. But **coordinator.env** is
not regenerated between those two steps. The restart picks up the
stale file.

**Target.** Have `reload-config.sh` call `startup.sh` (or a subset
of it) instead of just `fetch-config.sh`. `startup.sh` already
regenerates coordinator.env as part of its boot flow; invoking it on
reload gives us the fresh values.

Lives in `infrastructure-catalog/packer/ansible/files/scripts/`. Not
a weft-repo change — requires an infra-catalog commit + re-AMI.

**Size.** Tiny (~5 line change in reload-config.sh) but cross-repo.

## 9. `WEFT_` env-prefix consistency

**Today.** Partial rename in commit `382a384`: `LIFECYCLE_QUEUE_URL`,
`PARQUET_DIR`, `LAMBDA_FUNCTION_URL`, `SHARD_VOLUME_DEVICE` dropped
their `WEFT_` prefix. `SHARD_ASG_SMALL` family got renamed in commit
`8935137` (coordinator.env side). Still prefixed:

- `WEFT_DB_DSN` — the Go code reads this name as the fallback for
  `--db-dsn` (`cmd/coordinator/main.go:191`, `cmd/api/main.go:31`).
- `WEFT_S3_REGION`, `WEFT_S3_ACCESS_KEY`, `WEFT_S3_SECRET_KEY`,
  `WEFT_S3_ENDPOINT`, `WEFT_S3_USE_SSL` — read by `shard.S3ConfigFromEnv`
  (`shard/s3config.go:35`). Docker/MinIO tests rely on the names as-is.
- `WEFT_AGENT_TOKEN`, `WEFT_TOKEN`, `WEFTD_TOKEN` — auto-discovered from
  Secrets Manager by name; rename requires a secret rename in AWS.

Docker's `entrypoint.sh` continues to use `WEFT_SHARD_ASG_*` and friends
— its own convention, separate from the AMI path. Two naming schemes
coexist.

**Target.** Full rename to non-prefixed names across Go code, secrets,
Docker entrypoint. Coordinated cutover: rename Go reads to accept both
names for one release, then drop the prefixed fallback after all env
sources have migrated.

**Size.** Medium (~100 lines across 5-10 files + Secrets Manager
renames + Docker env var migration docs).

## 10. Pre-existing test failures

Neither caused by work in this session. Both are known.

- **`TestProvisioner_TickerContinuesAfterBootstrapFailure`**
  (`coordinator/provisioner_lifecycle_test.go:25`). Asserts the
  provisioner's ticker retries `bootstrap()` when the initial launch
  fails. `Start()` never implemented that retry; it only calls
  bootstrap once at startup. Either implement the retry (makes the
  system self-healing after a transient launch failure) or delete the
  test.

- **`TestRegistry_PersistAndRestore`** (`coordinator/archiver_test.go:219`).
  Fails looking for a Parquet file the cold-shard path expected to
  find at a `t.TempDir()` location. Likely a test-setup bug — the
  archive path writes to one directory but the test reads from
  another, or the test's lifecycle expectation doesn't match the
  current archive implementation. Would need ~30 minutes of reading
  to diagnose precisely.

**Size.** Each is small (tens of lines) once investigated.

## 11. Remaining gaps from `docs/shard-lifecycle.md`

The original gap inventory in shard-lifecycle.md. Current status:

| # | Gap | Status |
|---|---|---|
| 1 | Durable shard identity on EBS | done (christen writes shard.json via Configure RPC) |
| 2 | Coordinator christening path | done — now LAUNCHING-row promotion in `ProcessHeartbeat.christen` |
| 3 | Rotation on wall-clock windows | partial — Path A transitions, no replacement yet (see §5) |
| 4 | EBS per-shard attach/detach | partial — lifecycle hook claims LAUNCHING rows and tags volumes with real shard ids; recovery branch exists |
| 5 | Failure-recovery branch | partial — `reapDeadShards` preserves rows with `ebs_volume_id`; rebind path works; the `TerminateInstanceInAutoScalingGroup` call from §4 is the missing piece |
| 6 | Archive scheduling on retention | done (check's retention branch) |
| 7 | Registry ↔ store consistency | done (persist on every transition; FK ensures referential integrity) |
| 8 | Clean up `shard_records` cruft | done via migration 019 |
| 9 | `apps` populated at christening | done — bucket read is authoritative; LAUNCHING row's snapshot is for observability only |

Gaps #3, #4, #5 fold into the reconciler work (§1, §5, §4
respectively). Once those land, the shard-lifecycle doc's "Gap
analysis" section can be removed — the doc becomes a statement of
architecture rather than a work-in-progress inventory.

## Minor observability and ergonomics

Small but worth flagging:

- **`shard_records.row_count` is stale.** Only updated on state
  transitions (via `persist()`), not on heartbeats. The `ListShards`
  output shows the row count at last transition, not current reality.
  Heartbeats have a `row_count` field that's written to
  `shard_heartbeats` only. If row_count in shard_records needs to be
  live, the Heartbeat handler has to also call `PersistState` — but
  that'd be a write per heartbeat per shard, noisy. Alternative:
  query `shard_heartbeats` for the latest when you need current row
  counts.

- **DLQ silence.** When there are zero INGESTING shards, `drain` DLQs
  every batch. Logged as `drain: DLQ'd N rows for app (no healthy
  shards)`. No metric, no alert. A heartbeat-like periodic summary
  ("N rows DLQ'd in the last minute") would be visible in ops
  consoles.

- **`assigned apps ... 0 shards` log.** `cmd/coordinator/main.go:274`
  still runs, still logs, still does nothing because the registry is
  empty at that point. The "assign apps to all shards" code block
  pre-dates the bucket model and should be deleted — app assignment
  now flows through christen + OnboardApp.

- **`BucketManager.EvaluateSplit` threshold is not configurable.**
  Hard-coded 0.6 on the struct field. Fine for now; flag if an
  operator ever asks to tune it.

## Suggested sequencing

Work in progress — checked items are landed:

- [x] **§2 (LAUNCHING state)** — reconciler/hook/christen now agree
  on a shared LAUNCHING row.
- [x] **§1 (reconciler), ops 1/2/3/5** — per-bucket reconciler with
  explicit op types, idempotent and state-gated. Ops 4 (pre-warm)
  and 6 (terminate-on-archive) land with §5 and §4 respectively.
- [ ] **§3 (ASG desired wiring)** + **§4 (terminate-on-archive)** —
  flip ASG to coordinator-managed. At this point the shard-lifecycle
  doc's §3/§5 gaps close.
- [ ] **§5 (pre-warm + rechristen)** — completes the window-rotation
  story.
- [ ] **§7 (lifecycle hook intent)** — can land anywhere after §2;
  defensible as its own PR.

§8 (reload-config), §9 (env prefix cleanup), §10 (test failures), §11
doc revisions are all independent small PRs that can interleave.

§6 (onboarding race) is small and can go any time; probably worth
doing after §1 since the reconciler is a nice place to put the
buffering logic if we go that route.
