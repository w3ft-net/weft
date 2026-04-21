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

## 3. ASG `desired_capacity` driven by the reconciler — done (coordinator side)

**Status.** Coordinator side landed. Formula is:

```
desired(tier) = Σ bucket.target_ingesting_count (buckets where tier=tier)
              + count(READ_ONLY registry entries with Tier == tier)
```

Targets cover LAUNCHING+INGESTING in one number (a LAUNCHING in
flight and an already-INGESTING shard both occupy one target slot).
READ_ONLY adds past-window shards whose instances are still hot.
ARCHIVED is excluded — §4 terminates those explicitly.

`syncASGDesiredCapacity` runs once per tick at the end of `check()`,
after the per-bucket reconciler has minted any LAUNCHING rows. It
clamps to the ASG's `MaxSize` (queried via `DescribeAutoScalingGroups`)
with a log tripwire when the clamp kicks in. The
`SetDesiredCapacity(+1)` ratchet in `ASGLauncher.LaunchInstance` is
gone; the method is now a stub that errors to catch stale callers.
`PollInterval`/`PollAttempts`/`instances`/`known`/`asgInstanceIDs`/
`instancePrivateIP` are all dead code, removed.

**TF companion change — still pending, in `infrastructure-catalog`:**

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

**Paired with §4** to close the ARCHIVED-instance consistency gap:
§3 excludes ARCHIVED from the desired formula and §4 terminates
those instances explicitly with `ShouldDecrementDesiredCapacity=true`,
so ASG actual and coordinator-computed desired stay lockstep.

**Size.** Small (~50 lines reconciler + TF changes) but depends on
§1+§2 being in place first.

## 4. Terminate-on-archive — done

**Status.** Landed as `TerminateArchivedInstanceOp` (op 6 from §1's
table). Reconcile emits the op for any `StateArchived` entry with
non-empty `InstanceID`; Apply calls `launcher.TerminateInstance`
which routes to `TerminateInstanceInAutoScalingGroup` with
`ShouldDecrementDesiredCapacity=true`, then nulls `InstanceID` +
`Addr` so subsequent ticks don't re-emit. One-tick latency between
archive (op 5 fires tick T) and terminate (op 6 fires tick T+1)
because the op 5 side effect isn't visible in the same tick's
snapshot; acceptable.

The footgun §3 flagged (coordinator-computed `desired` dropping
below ASG actual-count, letting scale-in pick the wrong instance)
is closed: `desired` excludes ARCHIVED and the terminate call is
now the mechanism that brings ASG actual down in lockstep.

**IAM.** Coordinator role needs
`autoscaling:TerminateInstanceInAutoScalingGroup` on the shard
ASGs. Existing use in `reapDeadShards` (pre-§4) already required
this, so no new IAM grant needed for §4 itself — but worth
verifying next time the role is audited.

Integration test (`integration/terminate_on_archive_test.go`)
exercises the full aws-sdk-go-v2 request path against a stub
`ASGApi` (LocalStack Community doesn't implement ASG). LocalStack
seeds a realistic EC2 instance id so the asserted terminate call
mirrors prod wiring.

## 5. Pre-warm + window-rotation rechristen — done

**Status.** Landed as `PrewarmNextWindowOp` (op 4 from §1's table).
When any INGESTING shard in a bucket is within `PrewarmLead` of its
`WindowEnd` and no next-window LAUNCHING rows exist yet, the
reconciler emits `bucket.Target` pre-warm ops. Each op calls
`mintLaunchingRowForWindow` to INSERT a LAUNCHING row with
`window_start = next window boundary`. The tier-level
`syncASGDesiredCapacity` factors these rows in via
`CountShardsInBucketForFutureWindows`, so ASG's `desired_capacity`
rises in lockstep with the pre-warm commitment — ASG brings up new
instances, the lifecycle hook claims the pre-warm rows, and those
shards are ready to serve writes when the current window closes.

The policy decision is a method on `AppBucket`:
`ShouldPrewarmNextWindow(now, ingesting) bool`. Today it's simple
date math against a package-scoped `coordinator.PrewarmLead`. The
method exists so future policies (ingestion burn rate, disk-fill
slope, p99 latency tracker) can swap the implementation without
touching every caller. `PrewarmLead` is a package var rather than a
method argument because future strategies may not use a lead
duration at all.

`PrewarmLead` defaults to zero (disabled); `main.go` overrides for
prod (suggested: ~5 minutes for dev, ~1 hour for prod — less than
the min acceptable write-gap at a window boundary).

Apps-snapshot freshness is already handled: christen re-reads
`bucket.Apps` at Configure time, so a pre-warmed shard that sits in
LAUNCHING for minutes still picks up any apps that got onboarded
during that window.

**Desired formula update.** §3's formula grew a third term:

```
desired(tier) = Σ bucket.target + count(READ_ONLY entries in tier)
              + count(LAUNCHING rows in tier with window_start > current)
```

The new term is "pre-warm overhead" — LAUNCHING rows for future
windows, beyond what `bucket.target` covers for the current
window. Without this term ASG's scale-in would kill pre-warmed
instances as fast as the coordinator requested them.

**Integration tested** against real Postgres
(`integration/prewarm_test.go`): seeds a bucket with a 5-minute
window, an INGESTING shard close to `WindowEnd`, and verifies the
reconciler INSERTs a next-window LAUNCHING row with the correct
bounds and empty `instance_id`.

## 6. App-onboarding race window — done

**Status.** The drain-async race this section originally described
is moot: `OnboardApp` runs synchronously inside `IngestStream`'s
receive path (per-stream mutex) since commit `de752cf`, so a row
for a new app can't reach a shard before the shard's `Apps` list
has been updated. The §1 reconciler preserves that property —
the `AddAppToShardOp.Apply` path goes through the new
`Registry.AddAppToShard` method which takes the registry's write
lock, so concurrent `IngestingShard` / `MatchingShards` readers
can't tear on the `Apps` slice header.

A residual cross-stream visibility window still exists during
`OnboardApp`'s ~5–10ms DB write; it's bounded and benign because
no shard holds data for the new app during that window anyway.
See `docs/shard-lifecycle.md` → Coordinator contract → App
onboarding path for the permanent architecture description.

**Original write-up below, preserved for context.**

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

## 7. Lifecycle hook consultation of reconciler intent — won't do

**Status.** Analysed and discarded. The proposal was: if both a
pending LAUNCHING row AND an orphan recoverable volume exist for
the tier+AZ, prefer the LAUNCHING row (CHRISTEN) so the
reconciler's intent wins. In practice that inverts the priority
order we actually want:

1. **Durability wins.** An orphan volume tagged with a recognised
   shard_id carries a living shard's data. Letting it go unattached
   (while we christen fresh) strands the data; any subsequent
   cleanup — a manual `aws ec2 delete-volume`, cost-reaper,
   anything — destroys it silently.
2. **Reconciler intent is self-healing.** If a LAUNCHING row gets
   pre-empted by recovery of an unrelated shard, the row stays
   unclaimed. Next tick's `syncASGDesiredCapacity` still sees it
   (via `CountShardsInBucketForFutureWindows` + target math), the
   ASG launches another instance, and the LAUNCHING row gets
   claimed on that one. The cost is one tick of latency, not data.

Post-§2 the hook already does the safe thing: RECOVER when a
recognised orphan volume exists, CHRISTEN-via-LAUNCHING when it
doesn't, ABANDON when ambiguous or when neither a row nor a
volume matches. The priority comment + observability log added
in this session make the policy explicit at the decide() site.

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

## 10. Pre-existing test failures — done

Both were test drift rather than code bugs:

- **`TestProvisioner_TickerContinuesAfterBootstrapFailure`** — the
  test pre-dated the reconciler's bucket-config-driven scale-up.
  With no bucket, the reconciler has nothing to scale toward, so
  no launches fire. Fix: seed a bucket in the test. The
  self-healing behaviour the test asserted is real — each ticker
  tick re-emits `LaunchShardOp`s while the launcher is failing.

- **`TestRegistry_PersistAndRestore`** — the entry's `BucketID` was
  zero, which violates migration 020+022's NOT NULL FK. `SaveShard`
  returned an error that `registry.persist` silently discarded, so
  the insert never landed and the restore saw nothing. Fix: look
  up the default bucket in the test + pass its id. Separately,
  `registry.persist` now logs errors instead of discarding, so the
  next silent-persistence bug is findable.

**Size.** Each is small (tens of lines) once investigated.

## 11. Remaining gaps from `docs/shard-lifecycle.md` — done

All 9 original gaps from the shard-lifecycle doc's inventory are
closed. The failure-recovery-terminate variant (gap #5) landed as
`RecoveryTerminator.TerminateForReplacement` on `ASGLauncher`,
called from `reapDeadShards`'s EBS-backed branch: stale heartbeat
+ bound volume → coordinator asks ASG to replace with
`ShouldDecrementDesiredCapacity=false` and nulls the entry's
`InstanceID` so the terminate doesn't re-fire during the 1-3
minute replacement window. Rebind restores the `InstanceID` from
the replacement's heartbeat.

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

- **`BucketManager.EvaluateSplit` threshold is not configurable.**
  Hard-coded 0.6 on the struct field. Fine for now; flag if an
  operator ever asks to tune it.

## Suggested sequencing

Work in progress — checked items are landed:

- [x] **§2 (LAUNCHING state)** — reconciler/hook/christen now agree
  on a shared LAUNCHING row.
- [x] **§1 (reconciler), ops 1/2/3/5** — per-bucket reconciler with
  explicit op types, idempotent and state-gated.
- [x] **§3 (ASG desired wiring)** — coordinator side done;
  `syncASGDesiredCapacity` computes absolute desired per tick,
  clamps to `MaxSize`. TF companion change still pending in
  `infrastructure-catalog` (remove `desired_capacity` + add
  `ignore_changes = [desired_capacity]` + set `max_capacity`).
- [x] **§4 (terminate-on-archive)** — op 6 lands; ARCHIVED
  instances now get reclaimed via
  `TerminateInstanceInAutoScalingGroup`.
- [x] **§5 (pre-warm + rechristen)** — op 4 lands; next-window
  shards come up before the current window closes. `PrewarmLead`
  is a package var; per-bucket policy lives in
  `AppBucket.ShouldPrewarmNextWindow` for future swap-in of
  burn-rate-based strategies.
- [x] **§7 (lifecycle hook intent)** — won't do. Priority analysis
  in §7's own section; inverting current RECOVER-first logic would
  strand orphan-volume data.

§8 (reload-config), §9 (env prefix cleanup), §10 (test failures), §11
doc revisions are all independent small PRs that can interleave.

§6 (onboarding race) is small and can go any time; probably worth
doing after §1 since the reconciler is a nice place to put the
buffering logic if we go that route.
