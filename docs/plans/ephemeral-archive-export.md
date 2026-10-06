# Ephemeral archive export (future work)

Move the ARCHIVING export (DuckDB → partitioned Parquet in S3) off the
shard's own instance and onto a short-lived, right-sized instance
working from a snapshot-derived copy of the shard's volume. The live
shard keeps serving queries, undisturbed, until ARCHIVED.

Status: designed, not started. Phase 0 (instrumentation) is shared with
item 1 of [open-items-2026-10-06.md](open-items-2026-10-06.md).

## Problem

`shard-20260921T0000-4a672d0d` has sat in ARCHIVING for days. It is a
`t4g.micro` (935 MB RAM, 1 GB swap) holding a 2.9 GB `shard.duckdb`.
Every coordinator Evaluate runs `DuckDBShard.Archive` for exactly
`archiveTimeout` (10 min), thrashes, and fails `DeadlineExceeded`. See
open-items item 1 for the evidence.

The sizing doc ([duck-db-tuning.md](../../duck-db-tuning.md)) targets
20–30 GB of `.duckdb` per shard and reasons only about volume size and
query speed. It never models **archive export against RAM**. The export
does three heavy things on a box that is also serving queries:

1. a full-table `customerStats` GROUP BY,
2. `pickPartitionGranularity`,
3. `COPY ... TO s3 (FORMAT PARQUET, PARTITION_BY (_part))`.

A shard that is fast to query can still be too big to export on 1 GiB.
Also, sizing the always-on instance for a once-per-window export means
paying for RAM the shard doesn't need for ~6.95 of its 7 days
(`WindowDuration` default `7d`, `logdb/app_buckets.go`).

## Why not the obvious alternatives

- **Bigger always-on instance.** Pays for export RAM all week. Rejected.
- **Lambda (10 GB RAM, 10 GB `/tmp`, 15 min cap).** The data lives on
  EBS; Lambda can't mount it. It would need the raw `.duckdb` uploaded
  to S3 first, then pulled into `/tmp`. Caps at ~10 GB files and 15 min,
  and the tuning doc's target shard sizes (20–30 GB) exceed both.
- **Attach the live volume to a second instance.** gp3 has no
  Multi-Attach (io1/io2 only), and DuckDB's single-process file lock
  makes concurrent opens across hosts unsafe even where it exists.

## Design

### Flow

READ_ONLY shards are no longer ingesting, so their file is quiescent.

```
READ_ONLY ──ArchiveRequested──► ARCHIVING
  live shard: keeps serving queries from its own volume (unchanged)

ARCHIVING steps, each gated on a persisted fact, in order:
  A. CHECKPOINT the live shard (fold WAL into the main file)
  B. CreateSnapshot(volume)                  → export_snapshot_id
     (poll SnapshotState until completed)
  D. RunInstances(export type) with the snapshot as a block device
     mapping: the restored volume is created and attached at launch,
     and deleted with the instance          → export_instance_id
     boot `weft-shard export`
  E. the export opens the file, runs the existing exportArchive,
     writes Parquet + manifest to S3, then the host terminates itself
  F. Coordinator sees the manifest: SetShardParquetPath,
     RecordArchiveStats, swap Live for ColdShard
  G. Cleanup: ExportResources.Cleanup (any leftover instance, the
     snapshot), then the existing retire-volume + terminate-instance steps
  H. ArchiveComplete → ARCHIVED
```

Steps F–H already exist in `evaluateArchiving` (`logdb/shardsm.go`);
this plan replaces only step 1 ("`liveShard.Archive(...)`") with A–E and
adds the export-resource cleanup to G.

### Completion signal: an S3 manifest, not an RPC

The export instance writes `<prefix>/_manifest.json` last: Parquet path,
per-customer stats (the existing `CustomerStat` set), row/file/byte
counts, durations, attempt id. The coordinator polls for it on its
normal tick. This needs no inbound connectivity to the export instance,
survives a coordinator restart or leader change, and an incomplete
export simply has no manifest, so a partial write is never mistaken for
done.

### Right-sizing the export instance (derived, not escalated)

The instance type is **calculated** from the shard's data, not found by
trial and error.

What the export costs depends on:

- **Time ≈ `dbBytes / throughput`.** The scan streams; DuckDB spills
  what doesn't fit.
- **Memory ≈ `partitions × per-partition writer buffer + fixed
  overhead`.** The partition count comes from `pickPartitionGranularity`
  (`shard/shard.go`), which already derives it from `dbBytes`
  (`estimatedParquet = dbBytes × 0.4`, `targetFiles = est / 150 MB`).
- **The `customerStats` GROUP BY** scales with distinct customers (small).

So `exportInstanceType(dbBytes, customerCount)` is a **pure,
table-driven, unit-tested function** returning a type with headroom,
bounded by a floor and a cap. `customerCount` (cheap, from the DB)
covers skew: many tiny customers produce more, smaller partitions than
the same bytes from a few. `dbBytes` must be the `.duckdb` file alone,
not the heartbeat's `disk_bytes` (which is `.duckdb` + WAL + spill), so
the shard should report `DuckDBBytes` separately or we read it from the
snapshot; otherwise we over-size slightly.

The coefficients (per-partition buffer, throughput per instance type)
are **measured in Phase 0**, not known today. The only data point so far
is 4a672d0d, a failed run that shows 935 MB is too little for 2.9 GB,
not how much suffices. Seed value to validate: a memory-optimized
Graviton (`r7g`/`r8g`) with `memory_limit` ≈ 75% of RAM. Per-second
billing means a 10–30 min export costs cents.

The chosen type is persisted as `export_instance_type`, so retries are
deterministic. There is **no escalation ladder**. The only fallback is a
single defensive step up on a failed attempt, because the coefficients
start out as estimates. A derived size that fails in prod is a formula
bug, and the alert says so rather than silently retrying bigger.

### Persisted state and idempotence

New nullable columns on the shard row: `export_snapshot_id`,
`export_instance_id`, `export_instance_type`, plus
`export_started_at` and `export_attempt`. Every step is skipped if its
fact is already set, so a coordinator crash resumes mid-sequence, the
same pattern as the existing steps 1–3. The snapshot is immutable and the
original volume is untouched, so a retry from any step is clean. Retry
from scratch by clearing the facts (and cleaning up) if the export
instance dies.

## State machine changes

**No new FSM states and no new edges.** Sub-phases are *derived* from the
`export_*` facts, matching the existing pattern where side effects live
in `Evaluate` gated on persisted facts and a restart simply re-drives.
ARCHIVING stays special-cased: no `HeartbeatStale` edge, routed like
READ_ONLY by `MatchingEntries`. `ARCHIVING → ARCHIVED` (`ArchiveComplete`)
and `ARCHIVING → GONE` (`AdminDelete`) are unchanged. The bucket FSM is
unchanged (archival is fire-and-forget from its side).

What changes is `evaluateArchiving` (`logdb/shardsm.go`):

1. **Non-blocking phase machine.** Today step 1 is one blocking call
   bounded by `archiveTimeout` (10 min). The new steps wait on snapshot
   completion, volume creation, instance boot, and a 10–30 min export.
   Each tick reads the facts, polls one AWS state, advances at most one
   phase, and returns (the `evaluateLaunching` async pattern). This also
   stops ticks pinning a slot-pool slot for minutes.
2. **Export no longer depends on the live handle.** The current guard is
   `parquetPath == "" && liveShard != nil`, followed by an unconditional
   volume retire. A shard in ARCHIVING with `Live == nil` and no parquet
   path skips the export and then deletes the volume, a silent
   data-loss path. Whether it is reachable in prod is unconfirmed (the
   eager attach at `LoadFromStore` may prevent it), so Phase 3 starts
   with a failing test for it. The new path keys off the volume and
   snapshot, and **retire is gated on `ParquetPath != ""`**.
3. **A dead shard instance is no longer a stuck state.** Snapshots work
   whether or not the instance is alive, so the export proceeds anyway.
   `CHECKPOINT` is best-effort; a crash-consistent snapshot recovers by
   WAL replay on open.
4. **Stricter completion.** `done` requires a parquet path, no volume,
   no instance, and no `export_snapshot_id` / `export_instance_id`. `AdminDelete` / `ForceRemove` also clean up
   export resources (the sweep is the backstop).
5. **Per-phase deadlines and failure policy.** Deadlines derive from
   `export_started_at`. On export-instance death or timeout the next
   attempt cleans up and retries (one defensive size step up at most; see
   sizing). After a capped number of attempts it alerts and stays
   ARCHIVING; it never auto-escalates to GONE.
6. **Multi-coordinator safety.** Phase transitions use a
   compare-and-set claim on the `export_*` columns, and creates look up by
   tag first (as `findLiveByShard` does for instances), so two
   coordinators can't both create a snapshot. How `Evaluate` ownership is
   split across coordinators must be read before finalizing this.
7. **The export instance must not look like a shard.** Distinct tag keys
   (not `shard_id`), and `weft-export` neither heartbeats nor registers,
   so shard reconciliation (the ghost-instance incident class) never
   sees it.
8. **Global concurrency cap.** A bucket cohort rotates on one `WindowEnd`,
   so many shards hit ARCHIVING in the same tick. Cap in-flight exports
   from the DB (rows with `export_instance_id` set), not per coordinator,
   to stay under EC2 vCPU quotas. Cap value is an open question below.
9. **Observability.** Because derived phases are invisible in the
   `ARCHIVING` state name, log one line per phase transition and surface
   the phase and attempt in `weft-admin` shard output.

### No orphans

The incident that motivated this plan is a ghost-resource class, so the
new resources are fenced from day one:

- All ephemeral resources (snapshot, volume, instance) are tagged
  `weft:export-for=<shard_id>` and `weft:export-attempt=<n>`.
- The export instance launches with `InstanceInitiatedShutdownBehavior =
  terminate`, and `weft-export` shuts the host down on success, on
  fatal error, and on a hard max-lifetime timer.
- A coordinator sweep deletes tagged export resources whose shard is
  ARCHIVED, GONE, or absent, or whose attempt is superseded. This sweep
  is the first piece of the missing account-wide orphan reconciliation
  (see memory `project_volume_orphan_sweep_gap`), scoped to export
  resources only.
- The snapshot is deleted in step G on success. This must not
  reintroduce the unbounded snapshot leak removed 2026-09-15
  (`docs/architecture-overview.md`): snapshots here are ephemeral, and the
  sweep is the backstop.

### Dev / docker

The docker launcher has no EC2. Put the export behind a small interface
(`ArchiveExporter`) with two implementations: the existing in-process
`DuckDBShard.Archive` for dev and tests, and the ephemeral-instance
exporter for prod. This is a real seam (two live callers), not
speculative indirection. `Archive` itself is fixed per Phase 0 to honor
its context and refuse overlapping runs.

## Phases (failing tests first, per the bug-fix rule)

**Phase 0 — instrumentation (shared with open-items #1).**
Per-attempt phase logging in `Archive` (stats-scan duration, sizing
decision, COPY start/end, error, rows/files/bytes); honor `ctx` via
`ExecContext`; in-flight guard; native-memory fields in the shard log
(RSS, `VmSwap`, `duckdb_memory()`, `/proc/pressure/memory`,
`memory_limit`/threads); shard `gc_pauses_p99` alert. Output: the
export-capacity table (bytes → RAM/time per instance type).

**Phase 1 — `weft-shard export` subcommand. DONE.** A subcommand of
`cmd/shard` (no second binary to bake into the AMI), implemented in
`shard/export.go` and `cmd/shard/export.go`:
`weft-shard export --shard-id ID --db PATH --prefix PREFIX --attempt N
[--timeout D]`. It never serves, heartbeats or registers.
- `OpenForExport` opens the restored file without creating tables or
  running migrations, and refuses a missing file or one with no `logs`
  table, so a mis-mounted empty volume fails instead of exporting an
  "empty success" the coordinator would act on. It opens read-write, not
  read-only, so DuckDB can replay a WAL; the volume is a disposable copy.
- `RunExport` runs `Archive` (so the Phase 0 logging, context and
  single-flight apply), then writes the manifest last through a
  `ManifestStore` (`LocalManifestStore` for dev/tests, `S3ManifestStore`
  for `s3://` prefixes). No manifest on failure; a failed manifest write
  fails the export.
- Manifest key: `<prefix>/_manifests/<shard-id>.json` (`ManifestKey`).
  Fields: shard id, attempt, parquet path, rows, db bytes, per-customer
  stats, started/finished. `attempt` lets the coordinator ignore a stale
  manifest.
- Verified: a crash-consistent copy (main file plus un-replayed WAL,
  taken while the shard is open) exports every row, so the pre-snapshot
  `CHECKPOINT` really can be best-effort (resolves part of open question
  2).
- Not yet verified: `S3ManifestStore.Put` against real S3 (no LocalStack
  test), and the export against a snapshot-restored EBS volume.
- Shutting the host down on success or failure is the instance
  user-data's job (Phase 2); the binary just exits 0, 1 (failed) or 2
  (usage).

**Phase 2 — snapshot and export-instance primitives. CODE DONE;
infrastructure not.** The design changed from the original sketch in one
useful way: `RunInstances` takes a block device mapping with a
`SnapshotId`, so the restored volume is created and attached at launch and
deleted with the instance (`DeleteOnTermination`). There is no separate
create-volume or attach phase and no export volume to track or leak, so
the FSM facts `export_volume_id` and the volume half of cleanup are gone.

Code (`logdb/export_resources.go`, a new `ExportResources`; the existing
`VolumeManager` and `EC2Provisioner` are untouched):
- `CreateSnapshot`, `SnapshotState`, `LaunchInstance`, `InstanceState`,
  `Cleanup`. Each is a single non-blocking AWS interaction, idempotent by
  tag, so the shard FSM polls across ticks instead of waiting in a call.
- Tags are `export_shard_id`, `export_attempt`, `export_prefix` and
  `Role=export`. **Never `shard_id` or `shard_tier`**, which the shard
  lookups (`findLiveByShard`, `FindByShard`) key on, so an export resource
  cannot be mistaken for the shard's own. A test pins this.
- `Role=export` matters: the AMI's `startup.sh` starts `weft-$Role`.
- The export host launches from the coordinator's own AMI, so it has the
  `export` subcommand, and with
  `InstanceInitiatedShutdownBehavior=terminate`.
- `Cleanup(shardID)` terminates live export instances and deletes export
  snapshots for the shard across all attempts; refuses an empty id (which
  would match everything).

Instance side (`deploy/`):
- `weft-export.service` (oneshot, runs `weft-shard export`) and
  `export-role.sh`, sourced by `startup.sh` when `Role=export`. It reads
  the job from the instance tags, validates it (the values reach a systemd
  EnvironmentFile), mounts the restored volume at **`/mnt/export`** and
  **never formats it** (a blank volume is a bad restore; formatting would
  turn it into an empty "successful" archive).
- Mounted at `/mnt/export`, not `/opt/app/data`, because weftd reads
  `shard.json` there for its `node` dimension and the restored volume
  carries the live shard's; the export host's metrics would be attributed
  to the shard.
- Shutdown is guaranteed three ways: an EXIT trap, a `shutdown -h +180`
  backstop armed first thing, and the unit's own `--timeout`. weftd starts
  before the export so its log (including the Phase 0 progress lines) ships
  to weft; `export_finish` waits for the unit, then 30s for the flush.
- Tested: `deploy/export-role_test.sh` (`make test-deploy`) covers input
  validation. Not tested: mount and shutdown paths, which need a real host.

**Infrastructure this needs, in `infrastructure-catalog` / `-live` (not
done; separate repo, and Terraform changes need your review):**
1. A dedicated **export launch template** (a new `shard-fleet`-like
   module). Do not reuse a shard template: the template's `Role=shard`
   tag may or may not be overridden by the request's tags, and if it is
   not, the export host boots as a blank shard. It needs `Role=export`,
   `instance_metadata_tags = enabled`, the shared user-data, a security
   group, and an instance profile that can write the archive prefix.
2. **Coordinator IAM:** `ec2:CreateSnapshot`, `DescribeSnapshots`,
   `DeleteSnapshot`, `RunInstances` including block-device-mapping from a
   snapshot, `CreateTags` on snapshots and volumes at creation, and
   `iam:PassRole` for the export instance profile.
3. **Coordinator config:** an export launch template id, per-AZ export
   subnets (the tier's shard subnets work), and the export instance
   profile. Per your rule, these belong in Terraform, not ad-hoc SSM.
4. **AMI:** picks up `export-role.sh` and `weft-export.service` via the
   `packer/weft.pkr.hcl` change on the next `kevcons build`.

Assumption to verify on the first real launch: `ebsnvme-id` maps the
snapshot-restored volume to `/dev/sdf` the way it does for a shard's
volume (the block device mapping asks for `/dev/sdf`).

**Phase 3 — FSM wiring. DONE (code; off until configured).** In
`logdb/shard_export.go`, with the hooks in `evaluateArchiving`:
- **Facts:** migration 080 adds `export_snapshot_id`, `export_instance_id`,
  `export_instance_type`, `export_started_at` and a monotonic
  `export_attempt` to `shard_records`. The shard row type is the gorm
  model, so they are fields on `Shard`. Narrow writers on `ShardStore`;
  `BeginExportAttempt` is a compare-and-set on the counter.
- **Trap found and pinned:** `SaveShard` upserts the whole row from
  `persistedCopy()`, which lists fields by hand. Without the new fields
  every ordinary persist would have written the export facts back empty.
  `RefreshFromStore` likewise copies by hand, and a new leader needs it to
  see the old leader's in-flight attempt. Two tests fail without these.
- **Phases, derived each tick, chained while nothing needs to wait:** claim
  attempt (size from the shard's latest heartbeat `disk_bytes`) → snapshot
  → wait → launch (held by the concurrency cap) → wait for manifest →
  record archive → clean up → retire the shard's own volume and instance.
- **Failure:** snapshot error, instance ended with no manifest (re-checking
  the manifest once, since the host can write it and power off between two
  polls), or the 3h attempt deadline fails the attempt: clean up, clear
  facts, keep the counter. The next attempt steps up one instance class.
  A stale manifest (earlier attempt) is ignored. An instance EC2 does not
  know yet is *not* treated as ended; a host that truly vanished is caught
  by the deadline. After 3 attempts the shard stays ARCHIVING with its
  volume and a log line per tick; set `export_attempt = 0` to re-arm.
- **Retire is held until the archive is recorded and the export resources
  are gone**, so a failed cleanup keeps the shard's volume. The export does
  not use the live handle, so a restarted coordinator still drives it.
- `AdminDelete` mid-export cleans up the shard's export resources.
- **Wiring:** `EXPORT_LAUNCH_TEMPLATE` and `EXPORT_MAX_CONCURRENT` from the
  environment; enabled only with the template, shard subnets and an
  `s3://` `PARQUET_DIR`. Anything missing logs why and leaves shards
  archiving in process (the old behavior); it never fails boot.
- **Dropped:** the pre-snapshot `CHECKPOINT`. It would need a new gRPC
  method, and Phase 1 showed a crash-consistent copy replays its WAL fully.
- **Multi-coordinator:** only the `shard-fsm-leader` lease holder runs
  `Evaluate`, so a double start needs a leader handover; the AWS calls are
  idempotent by tag and the attempt claim is a compare-and-set.
- **Seed values:** the instance-size table (`defaultExportSizing`) is a
  placeholder until Phase 0 measurements replace it.
- Not covered by tests: anything against real AWS.

**Phase 4 — sweep + alerts. DONE (code).** `logdb/export_sweep.go`, an
`ArchiveSweeper` loop (every 15 min, leader-gated) started by `startProd`
whether or not the export is enabled:
- **Orphan removal.** `ExportResources.ListExportResources` finds every
  live export instance and every export snapshot by the `export_shard_id`
  tag; the sweep removes what nothing references. A resource is an orphan
  when it is older than 15 min **and** its shard row is gone, its shard is
  ARCHIVED or GONE, or it is not the snapshot or instance the shard's
  current attempt records (a superseded attempt). The 15-minute grace lets
  the FSM adopt a resource it created but had not yet recorded. Anything
  older than 4h goes regardless (the FSM fails an attempt at 3h, so this
  only catches what that missed). A resource with no creation time is never
  swept. Deletes treat "already gone" as success, and one failure does not
  stop the rest.
- **Stuck-archive alert.** A shard ARCHIVING for longer than 6h (twice the
  attempt deadline) logs
  `ALERT archiving-stuck shard=<id> age=<d> export_attempts=<n>` every
  pass. It runs without the export too, since an in-process archive that
  cannot finish (4a672d0d) is stuck the same way. Entry time comes from
  `shard_operations`, which the row trigger writes on every state change.
- **The alert is a log line, not a built-in notification.** A cron or
  monitor on that line is the operating account's configuration and stays
  out of git. The attempt-cap line from Phase 3 is the other one to watch.
- Dev (`startDev`) does not start the sweeper.
- Not covered by tests: the sweep against real AWS, including whether the
  `tag-key` filter and tag-based `DescribeSnapshots` behave as the fake
  does, and listing past a single results page.

**Phase 5 — roll out.** See the checklist below. Re-evaluate `4a672d0d`
last: it should complete on its own once the coordinator runs the new
path. Only if it still cannot archive, consider `weft-admin force-remove`
(irreversible; needs explicit sign-off).

## Rollout checklist

Order matters. The feature ships **off** (no `EXPORT_LAUNCH_TEMPLATE`), is
proven by a manual drill on a scratch prefix, and only then enabled.
Nothing here has run against real AWS.

### 0. Blocker: stale files double-count rows on a retry

DuckDB's `OVERWRITE_OR_IGNORE` replaces files with the *same name* and
leaves every other file in the directory alone. Verified against real
DuckDB: after a second export into a directory holding a file from an
earlier partial attempt, the stale file survived and reading the archive
glob returned 40 rows from a shard holding 20.

This bites two ways: a retry attempt that splits files differently from the
failed one (a bigger instance means more threads), and the **in-process**
path, where `4a672d0d` has been writing partial output into
`<prefix>/<id>/` for days. Fix before enabling. Preferred design: each
attempt writes to its own directory (`<prefix>/<id>/attempt-N/`), the
manifest already records the path the coordinator stores, and the
coordinator deletes a failed attempt's output (it has an `ObjectDeleter`
for the janitor). Until fixed, do not enable, and list
`<prefix>/4a672d0d…/` in S3 before touching that shard.

### 1. Before merging

- [ ] Code review of the 9 commits on `worktree-ephemeral-archive-export`
      (`/code-review`).
- [ ] `make test`, `make test-deploy`; `go build ./...` clean.
- [ ] Renumber your untracked `078_org_id_bigint.sql`: the collapsed schema's
      max is 79 and mine is `080_shard_export_facts.sql`.
- [ ] Decide the fix for the blocker above and land it with this.

### 2. Infrastructure (`infrastructure-catalog` / `-live`; Terraform review)

- [ ] **Export launch template**: new module modeled on `shard-fleet`.
      `Role=export` (never `shard`), `instance_metadata_tags = enabled`, no
      `shard_tier` tag, the shared user-data with `instance_role = "export"`,
      an egress-only security group, the shard instance profile (it already
      writes the archive prefix and reads SSM/Secrets for `load-app-env`).
- [ ] **Coordinator IAM**: `ec2:CreateSnapshot`, `DescribeSnapshots`,
      `DeleteSnapshot`; `RunInstances` including on `snapshot/*` and
      `volume/*` resources; `CreateTags` on snapshots and volumes at creation;
      `iam:PassRole` for the export profile; `s3:GetObject` on
      `<archive prefix>/_manifests/*`.
- [ ] **EC2 vCPU quota** for the r7g family (On-Demand Standard) covers
      `EXPORT_MAX_CONCURRENT` × the largest class's vCPUs.
- [ ] Config goes in Terraform, not ad-hoc SSM. Adding `EXPORT_*` changes the
      app config blob's SSM parameter: **stop and warn on that plan line**.
      Leave `EXPORT_LAUNCH_TEMPLATE` unset on this first apply.
- [ ] Check whether this stack's apply also touches the cold-shard Lambda
      (it reverts to a placeholder; redeploy after if so).

### 3. Deploy (merge to `main`; CI builds and rolls the AMI)

- [ ] Migration 080 applied (coordinator boot). It is additive and old code
      tolerates the columns, so the rolling N=2 deploy is safe.
- [ ] Coordinator log shows `archive export disabled (EXPORT_LAUNCH_TEMPLATE
      is not set); shards archive in process` and `archive sweeper started`.
- [ ] No behavior change for in-process archives. Note: `roll-all` does not
      roll ARCHIVING shards, so **`4a672d0d` keeps its old binary** (no Phase 0
      logging). Phase 0 numbers must come from the drill below, not from it.
- [ ] After a healthy deploy, `make collapse` per the usual flow.

### 4. Drill: one export on a scratch prefix, feature still off

Use a `drill-` shard id and a scratch prefix, never the real archive prefix,
so nothing the coordinator or janitor reads is touched.

- [ ] Snapshot `4a672d0d`'s volume (a read-only operation on prod; `kevcons aws`).
- [ ] Launch one export host by hand from the export template, from the new
      AMI: `Role=export`, tags `export_shard_id=drill-<n>`, `export_attempt=1`,
      `export_prefix=s3://<archive bucket>/_export-drill`, the snapshot at
      `/dev/sdf`, shutdown behavior terminate. Start with `r7g.xlarge`.
- [ ] Verify the assumptions this design rests on:
  - [ ] `ebsnvme-id` maps the restored volume to `/dev/sdf`; it mounts at
        `/mnt/export`; `shard.duckdb` opens (WAL replay).
  - [ ] `weft-export.service` runs; `/opt/app/logs/export.log` fills; the log
        reaches weft (`/log/weft/export/…`) through weftd.
  - [ ] Parquet and `_manifests/drill-<n>.json` appear under the scratch
        prefix; the manifest's rows match the shard's `row_count`.
  - [ ] The host terminates itself and its restored volume is deleted.
  - [ ] The host did **not** register or heartbeat as a shard.
- [ ] **Capture the numbers**: wall time, peak RSS, PSI, spill bytes, partition
      count (the Phase 0 progress lines). Put them in the instance-size table
      (`defaultExportSizing` is a seed; there is no env override yet).
- [ ] Failure drills: terminate a host mid-export (no manifest, no partial
      "success"); export a copy with no `logs` table (must fail, not write an
      empty archive).
- [ ] Delete the drill snapshot, scratch prefix and any leftovers. If the
      sweeper is on, a `drill-` resource has no shard row and is removed
      after 15 minutes, so run the drill before enabling.

### 5. Enable, one shard

- [ ] Real-prefix pre-flight for `4a672d0d` (see the blocker): list and clear
      stale partial output under its prefix.
- [ ] Check the shard host is not still running an old in-process `COPY`
      (the old binary ignores its context): look for CPU and swap thrash.
- [ ] Terraform: set `EXPORT_LAUNCH_TEMPLATE` and `EXPORT_MAX_CONCURRENT=1`;
      watch for the SSM warning; restart coordinators one at a time.
- [ ] Log shows `archive export enabled (template=… max_concurrent=1)`.
- [ ] Watch `4a672d0d`: `export attempt 1/3 begun` → snapshot → launch →
      manifest → `archived to …` → volume deleted → ARCHIVED.
- [ ] Compare: archived row count equals the shard's `row_count`; a query over
      the archived shard returns what the live one did.
- [ ] Snapshot, export instance and restored volume are all gone.

### 6. Widen

- [ ] Raise `EXPORT_MAX_CONCURRENT` to 2. Watch the next real window rotation
      (a whole cohort reaches ARCHIVING together; the cap should hold launches).
- [ ] Re-evaluate the instance-size table against the real runs.

### 7. Alerts to configure (account config, not in git)

- [ ] `ALERT archiving-stuck` (the sweeper; 6h in ARCHIVING).
- [ ] `export attempt .* failed` and `export failed … times (cap 3)`.
- [ ] `archive-sweep: removed orphaned export` (non-zero means something leaked).
- [ ] EC2 spend and any lingering `export_shard_id` snapshots or instances.

### Rollback

Unset `EXPORT_LAUNCH_TEMPLATE` and restart coordinators: shards archive in
process again. Leftover facts on a row are inert while the export is off.
With the feature off the sweeper does not remove orphans, so clean up by
hand anything tagged `export_shard_id`. Migration 080 is additive and needs
no rollback.

## Open questions

1. **Early rotation by size.** Separate from this plan, but the same
   incident: nothing rotates on size (`MaxDiskBytes` was deleted,
   `disk_bytes` is collected and unused). With off-box export, size
   matters less for archive success but still bounds export cost and
   time. Decide whether to add a size-based trigger or alert-only; note
   `WindowEnd` is invariant (memory `feedback_window_end_invariant`).
2. **CHECKPOINT on a READ_ONLY shard.** Confirm the shard exposes (or
   can be given) a checkpoint step, and that a snapshot of a
   checkpointed READ_ONLY file is clean. A crash-consistent snapshot
   without it would still recover via WAL replay on open, but we'd
   rather not depend on that.
3. **Snapshot hydration speed.** Volumes created from snapshots load
   blocks lazily from S3. One sequential pass over a few GB should be
   tolerable; measure in Phase 0/1. Fast Snapshot Restore is the
   fallback, at extra cost.
4. **AZ for the export volume.** Pick the export instance's AZ freely
   (it need not match the shard's); prefer the AZ with the best spot/
   on-demand capacity for the chosen type.
5. **Spot vs on-demand** for the export instance. A spot interruption
   just fails the attempt and retries from the snapshot, so spot is safe,
   but keep on-demand as the fallback so a capacity drought can't block
   archiving.
6. **Concurrent-export cap.** A fixed number or derived from the EC2
   vCPU quota?
7. **Cost sanity.** Per shard per window: one snapshot (~3–30 GB for the
   duration of the export), one short volume, one short instance.
   Estimate against actual shard count before shipping.
