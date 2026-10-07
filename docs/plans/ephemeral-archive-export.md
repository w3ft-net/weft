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
8. **Global concurrency cap, enforced at the claim.** A bucket cohort
   rotates on one `WindowEnd`, so many shards hit ARCHIVING in the same
   tick and are evaluated concurrently. The cap bounds export *attempts in
   flight* (`export_started_at` set), cluster-wide, to stay under EC2 vCPU
   quotas. It is applied inside `BeginExportAttempt`, in the same
   transaction as the claim, under a transaction-scoped advisory lock. Two
   earlier designs were wrong and are worth remembering: counting running
   instances before launching is a check-then-act race (twelve concurrent
   claims all passed a cap of 3, reproduced by removing the lock from the
   test), and holding only the *launch* leaves a queued shard's attempt
   clock and counter running, so a cohort larger than the cap burned its
   retry budget waiting. A shard held by the cap has started nothing: no
   attempt number, no clock, no AWS call.
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
- Output location: `<prefix>/<shard-id>.attempt-N[.parquet]`, one per
  attempt (see the rollout checklist, item 0, for why).
- The `<prefix>` the export uses is `<PARQUET_DIR>/<label>` where the label is
  the shard's service name **sanitized** to `[A-Za-z0-9._-]` (other characters
  become `_`, at most 64 characters, never `.` or `..`): the prefix travels
  through an EC2 tag, the host's shell and a systemd environment file, the
  host rejects anything outside `[A-Za-z0-9._:/-]`, and a shard's sole
  service name is customer-controlled. Launch, manifest polling and output
  cleanup all derive the prefix from the same function (`exportS3Prefix`),
  so they cannot disagree. The in-process archive keeps the raw name.
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
  attempt (size from the shard's latest heartbeat `disk_bytes`; the
  concurrency cap is applied here, atomically) → snapshot → wait → launch →
  wait for manifest → record archive → clean up → retire the shard's own
  volume and instance.
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

### 0. Resolved: stale files double-counted rows on a retry

DuckDB's `OVERWRITE_OR_IGNORE` replaces files with the *same name* and
leaves every other file in the directory alone. Verified against real
DuckDB: after a second export into a directory holding a file from an
earlier partial attempt, the stale file survived and reading the archive
glob returned 40 rows from a shard holding 20. A retry that splits files
differently from the failed attempt (a bigger instance means more threads)
would have hit this.

**Fixed in the export.** Each attempt writes to its own location,
`<prefix>/<shard-id>.attempt-N` (hive directory) or
`<prefix>/<shard-id>.attempt-N.parquet` (single file), so a retry never
shares a directory with a failed attempt. The manifest already carries the
path and the coordinator records it, so readers are unchanged. The
coordinator also deletes a failed attempt's output (`ShardDeps.ExportOutputs`,
the janitor's `ObjectDeleter`): after the host is stopped, so a running host
cannot write after the delete; never on the success path; never once an
archive is recorded; and only exact `<id>.attempt-N` paths. A failed delete
keeps the facts so the next tick retries. A snapshot that vanished (what a
half-finished failure leaves) now fails the attempt again instead of being
polled forever. Correctness does not depend on the delete: it only reclaims
storage.

**Not fixed: the in-process path** (`Archive`, used when the export is off
and in dev). It still writes `<prefix>/<id>` and a retry after a partial
attempt can double-count. It is left as is on purpose: giving each in-process
attempt its own directory would make a stuck shard that retries every 10
minutes leak a new partial copy per attempt. See `docs/known-issues.md`.
Enabling the export removes `4a672d0d` from this path.

### 1. Before merging

- [ ] Code review of the 9 commits on `worktree-ephemeral-archive-export`
      (`/code-review`).
- [ ] `make test`, `make test-deploy`; `go build ./...` clean.
- [ ] Renumber your untracked `078_org_id_bigint.sql`: the collapsed schema's
      max is 79 and mine is `080_shard_export_facts.sql`.
- [ ] Coordinator role can `s3:DeleteObject` and `s3:ListBucket` on the archive
      prefix (the janitor's deleter needs the same; confirm it is granted).

### 2. Infrastructure (`infrastructure-catalog` / `-live`; Terraform review)

Written, validated and formatted, **not planned or applied**, and not
committed (those repos are edited from outside this worktree). Two stages,
like the janitor: stage 1 creates the template and IAM with the feature off and
must not change `app_config_blob`; stage 3 flips `archive_export_enabled`.

What changed:
- `modules/export-fleet/` (new): launch template + egress-only security
  group. `Role=export` (never `shard`), no `shard_tier`, IMDS tags on,
  `instance_initiated_shutdown_behavior = terminate`, the shared user-data
  with `instance_role = "export"`. Deliberately not the `shard-fleet` module,
  so the shard launch templates cannot show a diff.
- `stacks/asg-weft-application`: `module.export_fleet` (reusing the shard
  fleet's instance profile, which already carries the SSM/Secrets reads and
  S3 write the host needs), `aws_iam_role_policy.coordinator_export_output_cleanup`,
  variables `archive_export_enabled` (default false),
  `archive_export_max_concurrent` (default 1) and
  `export_default_instance_type`, the two `EXPORT_*` blob keys behind
  `archive_export_enabled`, and outputs.
- `prod/weft`: the two variables (default off) and the pass-through. The
  tfvars is untouched.

Verified read-only against the live account (`kevcons aws`):
- [x] The shared `weft-instance-role` already **allows** `ec2:CreateSnapshot`,
      `DeleteSnapshot`, `DescribeSnapshots`, `RunInstances`,
      `TerminateInstances`, `CreateTags`, `DescribeInstances`, and
      `iam:PassRole` to itself. No new EC2 or PassRole grant is needed.
- [x] **EC2 vCPU quota** (Running On-Demand Standard): 256, ample for
      `EXPORT_MAX_CONCURRENT` × 8 vCPUs (r7g.2xlarge).
- [x] The S3 gateway endpoint is attached to the shard subnets' route table,
      so export traffic does not ride the coordinator NAT.
- [x] The role's inline policies total about 5.8 KB (with whitespace) against
      the 10.2 KB aggregate cap; the new policy adds about 200 bytes.
- [x] `s3:DeleteObject` on the cold bucket is **implicitly denied** today. The
      new grant allows it only on `*.attempt-*` keys, checked with the policy
      simulator: allowed for `<id>.attempt-N.parquet` and
      `<id>.attempt-N/<part>/data_0.parquet`; denied for the in-process layout
      `<id>/…`, `<id>.parquet` and `_manifests/…`.

Still to do:
- [x] `terraform plan` in `prod/weft` (2026-10-06): my three resources are
      exactly as designed (`module.export_fleet.aws_launch_template.export`,
      `module.export_fleet.aws_security_group.export`,
      `aws_iam_role_policy.coordinator_export_output_cleanup`), `Role=export`,
      no `shard_tier`, the shared instance profile, **`app_config_blob`
      untouched**. But the full plan was **14 to add, 7 to change**, so 18
      changes are pre-existing and unrelated: three ASGs' launch-template
      version `198 → $Latest` (the module sets `instance_refresh`, so this
      would roll the coordinators, compute and the loom producer for no
      functional change), the GitHub deploy role (OIDC trust narrowed, and
      the `PackerIAMPassRole` statement removed from the packer policy, which
      may break CI AMI builds), 11 new `pgbouncer` resources, and the RDS
      parameter group and instance (log_statement and autovacuum tuning, one
      pending-reboot). **Do not apply the saved plan.** Apply only mine:
      re-plan with `-target=module.weft.module.export_fleet
      -target=module.weft.aws_iam_role_policy.coordinator_export_output_cleanup`,
      expect 3 to add, then apply that plan. Per the repo rule the apply is
      run by you, teeing to a log.
- [ ] Check whether this stack's apply also touches the cold-shard Lambda (it
      reverts to a placeholder; redeploy after if so).
- [ ] Stage 3 (after the drill): set `archive_export_enabled = true`; that
      plan should touch only `app_config_blob`.

**Separate finding, not fixed here: the retention janitor cannot delete from
S3.** `janitor_enabled = true` in prod, but the coordinator's role has no
`s3:DeleteObject` on the cold bucket (the same simulation: implicit deny); the
only `DeleteObject` in the stack is on the compactor Lambda's role. The
janitor's `S3ObjectDeleter` runs on the coordinator, so its tombstone sweep and
retirement deletes should fail with AccessDenied. The export's grant above is
deliberately narrower and does not fix this. Worth checking the coordinator
logs for `janitor:` errors; widening delete on customer data is your call.

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

**Run 2026-10-07 (AMI `ami-03aaf26cfc925ead3`, GitSHA `a89ca35`). Core path
passed; the weftd log and one failure drill are open (below).** Runbook:
`docs/runbooks/export-drill.md`.

- [x] Snapshot `4a672d0d`'s volume (a read-only operation on prod; `kevcons aws`).
      100 GiB, about 9 minutes to complete.
- [x] Launch one export host by hand from the export template, from the new
      AMI: `Role=export`, tags `export_shard_id=drill-<n>`, `export_attempt=1`,
      `export_prefix=s3://<archive bucket>/_export-drill`, the snapshot at
      `/dev/sdf`, shutdown behavior terminate. Start with `r7g.xlarge`.
      The SSM agent was online about 30 s after launch and the export began
      about 10 s after that.
- [ ] Verify the assumptions this design rests on:
  - [x] The restored volume mounts at `/mnt/export` (it appeared as
        `nvme1n1`; I did not run `ebsnvme-id` itself) and `shard.duckdb`
        opens (all 22,404,273 rows exported, so the WAL replay is fine).
  - [x] `weft-export.service` runs; `/opt/app/logs/export.log` fills.
  - [ ] The log reaches weft (`/log/weft/export/main.log`) through weftd.
        **Not met (2026-10-07 drill):** weftd ran and shipped
        `/log/weft/export/metrics.log` (rows to 30 s after the export ended),
        but `main.log` never appeared. See the weftd TODO below.
  - [x] Parquet and `_manifests/drill-<n>.json` appear under the scratch
        prefix; the manifest's rows (22,404,273) match the shard's `row_count`
        (checked by hand against `shard_records`).
  - [x] The host terminates itself and its restored volume is deleted
        (checked for all four hosts; the volume goes with the instance).
  - [x] The host did **not** register or heartbeat as a shard
        (`shard_heartbeats` had no rows for it, and the live shard's `addr`
        and `instance_id` were unchanged).
- [x] **Capture the numbers.** One shard, a 3.0 GB DuckDB (22.4 M rows, 2
      customers), `date_trunc('hour', ts)` partitions, 7 target files:

      | | r7g.xlarge (4 vCPU, 31 GB) | r7g.large (2 vCPU, 16 GB) |
      |---|---|---|
      | stats scan | 11.9 s | 23.1 s |
      | COPY | not captured | 4m35s |
      | export wall time | about 3m10s | 4m58s |
      | peak RSS | over 7.6 GB (still climbing when sampled) | 7.6 GiB |
      | swap / PSI full / spill | 0 / 0 / 0 | 0 / 0 / 0 |
      | output | 208 files, 2.5 GiB | 199 files, 2.47 GiB |

      Peak RSS is about 2.6x the DB size, so a 3x-the-DB memory rule of thumb
      fits both runs with room. `r7g.large` is comfortable for this shard;
      `r7g.medium` (8 GB) was not tried and looks marginal at that ratio.
      **The sizing estimate is about 2x low**: `est_parquet_bytes` was 1.2 GB
      against 2.5 GiB written (it only affects file count). Not yet copied
      into `defaultExportSizing`. Launch to export start was about 26 s, and
      launch to terminated about 6.5 min.
- [ ] Failure drills:
  - [x] Terminate a host mid-export (attempt 2, killed about 90 s into the
        COPY): no manifest was written (the manifest on S3 stayed attempt 1's),
        59 partial objects (713 MB) were left under `…attempt-2/`, and the
        restored volume was deleted with the instance.
  - [x] A host with no restored volume (attempt 3): the host powered itself
        off (`Client.InstanceInitiatedShutdown`) within about 2 minutes, with
        no output and no manifest. **Not seen:** the
        `restored volume never appeared at /dev/sdf` message itself (the
        console output was empty and the host's disk was gone).
  - [ ] Export a copy with no `logs` table (must fail, not write an empty
        archive). Not run.
- [x] Delete the drill snapshot, scratch prefix and any leftovers. Done
      2026-10-07 (466 objects, 6.0 GB; the bucket is unversioned, so they are
      gone). If the sweeper is on, a `drill-` resource has no shard row and
      is removed after 15 minutes, so run the drill before enabling.
- [ ] **TODO (important): weftd must read a new log from the start on boot.**
      An export host lives about 5 minutes, and `export.log` is the only
      record of its phases and failures (the host and its disk are gone
      afterwards, and the coordinator only sees the manifest). Today two things
      lose it, both visible in the drill (weftd shipped metrics but no
      `main.log`):
      1. `weftd.NewTailer` (`weftd/tailer.go`) seeks to the end of the file,
         so lines written before the tailer opens are dropped. `startup.sh`
         starts the export unit before weftd, and the `start`, `stats`,
         `sizing` and `copy start` lines are written in the first ~25 s,
         exactly the sizing numbers and the early failure reasons.
      2. `onboardAndRun` (`cmd/weftd/main.go`) blocks in `WaitForConfiguration`
         until the `export` source is configured on the coordinator, and does
         not start the tailer until then. A short-lived host may never get
         there. (Inferred from the code; the host's weftd journal was not
         captured, so not confirmed.)
      Fix both: a per-source "read from start" option (a weftd change, so it
      needs a version bump and a release per the auto-upgrade rules), and the
      `export` source configured once so a new host starts tailing at once.
      Then re-run one small export and confirm `main.log` has the `start`
      line. Not blocking the rollout (the manifest and the coordinator's
      attempt state are the completion signal); it blocks debugging the first
      real failure.

### 5. Enable, one shard

**Done 2026-10-07.** Enabled in prod (`EXPORT_MAX_CONCURRENT=1`); `4a672d0d`
archived through the export.

- [x] Optional cleanup for `4a672d0d`: its days of failed in-process attempts
      left partial files under `<prefix>/<id>/`. The export writes to
      `<id>.attempt-N` and the archive records that path, so the old files
      are never read; they only cost storage. Found 10 objects (58.7 MB)
      under `32-apps/shard-20260921T0000-4a672d0d/`. The coordinator role
      cannot delete them (no `s3:DeleteObject`, as expected), so this was a
      manual delete. Done 2026-10-07; the live `…attempt-1/` archive was
      left intact.
- [x] Check the shard host is not still running an old in-process `COPY`
      (the old binary ignores its context): look for CPU and swap thrash.
      Quiet: weft-shard at 85 MB RSS, load falling.
- [x] Terraform: set `EXPORT_LAUNCH_TEMPLATE` and `EXPORT_MAX_CONCURRENT=1`;
      watch for the SSM warning; restart coordinators one at a time.
      Both coordinators were replaced one at a time (not `kevcons config
      set`, which runs `load-app-env` alone and restarts every instance at
      once; see the note under "Gaps found" below).
- [x] Log shows `archive export enabled (template=… max_concurrent=1)`.
      Both new coordinators, plus `archive sweeper started`. Coordinator
      app logs are in `/opt/app/logs/coordinator.log`, not the journal.
- [x] Watch `4a672d0d`: `export attempt 1/3 begun` → snapshot → launch →
      manifest → `archived to …` → volume deleted → ARCHIVED.
      18:19:06 begun (`r7g.large`), 18:23:07 launched, 18:28:06 archived,
      18:28:10 ARCHIVED (UTC). About 5 minutes from launch.
- [x] Compare: archived row count equals the shard's `row_count`
      (22,404,273 both). 
- [ ] A query over the archived shard returns what the live one did. Not
      checked: the live shard was already retired when I looked.
- [x] Snapshot, export instance and restored volume are all gone (and the
      shard's own host and data volume are retired).

**Gaps found by the first real run**
- A volume-less ARCHIVING shard claims the single global slot and sits on it
  (`advanceExport` claims before it checks for a volume). `d1c1b361`, a
  zombie row (17.4M rows, dead since 2026-07-13, no volume, instance or
  S3 output), held the slot with `ARCHIVING export has no volume to
  snapshot` every minute until it was removed with
  `weft-admin force-remove`. **Fixed**: `advanceExport` now requires a volume
  before it claims a slot (`TestShardExport_ShardWithoutAVolumeDoesNotTakeASlot`);
  such a shard waits unclaimed and the 6h stuck-archive alert surfaces it. A
  query for other ARCHIVING rows with an empty `ebs_volume_id` found none
  (2026-10-07).
- `weft-admin` is not on `PATH` on the coordinator; use
  `sudo /opt/app/bin/weft-admin-run <cmd>`.
- `kevcons` has no config "apply without writing" command; `config set`
  would also drop `AWS_REGION`/`S3_REGION`/`APP_NAME` (those are appended by
  `fetch-config.sh`, not `load-app-env`). Suggested new command: `kevcons
  config apply`, rolling, one instance at a time, health-gated.

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
6. **Concurrent-export cap value.** It is a config knob
   (`EXPORT_MAX_CONCURRENT`, default 2 in code, 1 in the Terraform default).
   With a cohort of shards rotating together, the cap sets how long the last
   shard waits: roughly (cohort size ÷ cap) × the export time. Tune it once
   the drill gives a real export time. The EC2 vCPU quota (256) is not the
   constraint.
7. **Cost sanity.** Per shard per window: one snapshot (~3–30 GB for the
   duration of the export), one short volume, one short instance.
   Estimate against actual shard count before shipping.
