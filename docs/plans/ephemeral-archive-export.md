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
  C. CreateVolume(from snapshot, export AZ)  → export_volume_id
  D. RunInstances(export type), attach export volume,
     boot weft-export (mode of cmd/shard)    → export_instance_id
  E. weft-export opens the file read-only, runs the existing
     exportArchive, writes Parquet + manifest to S3, self-terminates
  F. Coordinator sees the manifest: SetShardParquetPath,
     RecordArchiveStats, swap Live for ColdShard
  G. Cleanup: delete export volume, delete snapshot,
     then the existing retire-volume + terminate-instance steps
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
`export_volume_id`, `export_instance_id`, `export_instance_type`, plus
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
   no instance, and no `export_snapshot_id` / `export_volume_id` /
   `export_instance_id`. `AdminDelete` / `ForceRemove` also clean up
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

**Phase 2 — snapshot/volume/instance primitives.** Extend
`VolumeManager` with snapshot-create/wait/delete and create-from-
snapshot; extend `EC2Provisioner` with an export launch spec. Mock
`EC2VolumesAPI` tests like the existing volume tests.

**Phase 3 — FSM wiring.** Replace step 1 in `evaluateArchiving` with
A–E, add G cleanup, persist the new facts (migration at the next free
version above the collapsed schema's max). Tests: crash/resume at every
step boundary; retry after the export instance dies; manifest absent →
no completion.

**Phase 4 — sweep + alerts.** Orphan sweep for tagged export resources;
alert on ARCHIVING older than N× the expected export time.

**Phase 5 — roll out.** Ship, then re-evaluate `4a672d0d`: it should
complete on its own once the coordinator runs the new path. Only if it
still cannot archive, consider `weft-admin force-remove` (irreversible;
needs explicit sign-off).

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
