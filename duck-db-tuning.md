# DuckDB Shard Tuning & Disk Sizing Plan

Sizing and tuning guidance for the per-shard DuckDB databases that back weft's
sharded analytics workload. Written 2026-06-11.

## Workload constraints (the inputs that drive everything below)

- **Single table**, ~12 columns wide (narrow).
- Goals: **very fast inserts** and **fast queries**.
- Each shard runs on a **`t4g.micro` (1 GiB RAM)** instance.
- Shard data volumes are **gp3 EBS, currently 50 GiB** (4× 50 GiB data volumes +
  7× 8 GiB roots = 256 GiB total in the weft account).
- `memory_limit` on a 1 GiB box defaults to ~80% RAM ≈ **800 MB** — so any
  non-trivial full-table operation crosses into larger-than-memory and spills
  to disk. This is the single most important fact for sizing.

## TL;DR recommendation

- **Do not anchor on "100 GB/shard."** That number traces to a single,
  medium-confidence, undiagnosed report on a **200+ column wide table**
  ([#17210](https://github.com/duckdb/duckdb/issues/17210)) — the opposite of
  our narrow table. It is a "repartition before here" red line, not a target.
- **Target ~20–30 GB of `.duckdb` file per shard.** That keeps queries fast on
  1 GiB instances and fits comfortably (≤50%) inside a 50–60 GiB volume with
  co-located spill headroom.
- The real ceiling is set by **RAM and checkpoint cost, not a file-size wall.**
- For sustained fast inserts: **batch inserts, no unnecessary ART indexes/PK,**
  and manage checkpoint cadence.

## Key research findings (verified, with sources)

Deep-research pass: 15 primary/practitioner sources, 25 claims adversarially
verified (19 confirmed, 6 refuted).

### 1. There is no practical hard limit on `.duckdb` file size

- DuckDB officially has "no practical limit," is tested past **15 TB**, and ~1%
  of 2024 survey users run **2 TB+** files. (high confidence)
- Sources: [limits doc](https://duckdb.org/docs/current/operations_manual/limits),
  [huge-databases guide](https://duckdb.org/docs/current/guides/performance/working_with_huge_databases)
- **Implication:** the file size itself is not the constraint. Don't cap the
  file for the file's own sake.

### 2. What degrades at scale is operational, not capacity

- **Checkpointing slows** — ~5 s to checkpoint after adding a few rows to a
  265 GB DB. This is the one real link between file size and sustained insert
  speed. (high confidence,
  [huge-databases guide](https://duckdb.org/docs/current/guides/performance/working_with_huge_databases))
- Connect times grow on very large files; filesystem matters (XFS > ext4 on
  Linux).
- **Insert throughput degrading ~linearly toward ~100 GB** is **medium
  confidence** and from a **200+ column wide-table** case
  ([#17210](https://github.com/duckdb/duckdb/issues/17210)). **Likely does not
  apply to our 12-column table.** Treat as a yellow flag, not a law.

### 3. The dominant disk concern is spill/temp headroom, not the DB file

DuckDB's out-of-core engine spills larger-than-memory joins, sorts,
high-cardinality/holistic aggregations, exact `DISTINCT` counts, and windows to
a temp directory.

- **Default spill location is `<database_file>.tmp` — the SAME volume as the
  `.duckdb` file.** On a 50 GiB gp3, spill competes directly with the database
  file unless redirected.
  ([tuning guide](https://duckdb.org/docs/current/guides/performance/how_to_tune_workloads))
- **`max_temp_directory_size` defaults to 90% of REMAINING (free) disk.** The
  trap: if the `.duckdb` file fills most of the volume, free space shrinks, the
  spill ceiling shrinks with it, and large queries **OOM mid-flight**:
  `failed to offload data block ... This limit was set by the
  max_temp_directory_size setting`.
  ([memory-mgmt blog](https://duckdb.org/2024/07/09/memory-management),
  [#14142](https://github.com/duckdb/duckdb/issues/14142))
- **Spill volume is driven by per-query working set and `memory_limit`, NOT by
  DB size.** Less RAM per shard → more spilling → more headroom needed. On our
  1 GiB boxes this is the binding constraint.
- Spill is relocatable: `SET temp_directory='/mnt/spill'`. Caveat: some older
  versions/bindings ignored it ([#9401](https://github.com/duckdb/duckdb/issues/9401),
  [#14735](https://github.com/duckdb/duckdb/issues/14735)) — verify on the
  deployed version.

### 4. No official database-to-volume ratio exists

- DuckDB publishes **no** percentage-of-volume guidance. Any ratio is an
  engineering derivation from the spill mechanics above.
  ([limits doc](https://duckdb.org/docs/current/operations_manual/limits))

### Refuted claims (do NOT rely on these)

- `max_temp_directory_size` default is "unlimited" — **false**; the operative
  runtime default is 90% of free disk.
- DuckDB has no space-reclamation at all — **false**; reclamation exists
  ([reclaiming space](https://duckdb.org/docs/current/operations_manual/footprint_of_duckdb/reclaiming_space)).
- Spill "always" fills the whole disk — **false** (unsupported).
- S3-stored DB capped at 5 TB by S3 — **false** in this context.

## Tuning levers for OUR goals

### Very fast inserts

1. **Batch inserts — never row-by-row autocommit.** Use the Appender API,
   `COPY`, or multi-row `INSERT`. DuckDB is columnar; single-row commits are the
   anti-pattern and dominate throughput by orders of magnitude — independent of
   file size.
2. **Drop unnecessary ART indexes / PK / UNIQUE constraints.** They slow every
   insert and are memory-hungry. For an append-heavy single table, avoid them.
3. **Manage checkpoint cadence.** Checkpoint cost grows with file size — the one
   real file-size↔insert link. Smaller shards keep checkpoints cheap.

### Fast queries

- On `t4g.micro` (1 GiB), the binding constraint is **table-size-vs-RAM**, not
  file size. A full-table scan/aggregation over a multi-tens-of-GB table with
  ~800 MB `memory_limit` **spills and slows down** — this bites well below
  100 GB.
- Selective/filtered queries with min-max/zonemap pruning stay fast much longer.
  Keep data clustered/sorted on common filter columns to maximize pruning.

## Disk sizing math

Derived ratio rule (no vendor number exists):

| Design | Max `.duckdb` as % of volume |
|---|---|
| Co-located spill (default) | **≤ ~50%** (other half = spill + WAL/checkpoint + OS/FS reserve) |
| Spill redirected to separate volume | ≤ ~80–85% (data volume = file + checkpoint growth + ~15% reserve) |

Per-shard target band for "very fast both ways" on 1 GiB instances:

| If you want… | Per-shard `.duckdb` cap | Volume (co-located spill) |
|---|---|---|
| **Fast queries on t4g.micro as-is (recommended)** | **~20–30 GB** | **50–60 GiB** |
| Push larger, accept spill on heavy queries | ~40–50 GB | 100 GiB |
| Genuinely need ~100 GB/shard | bump instance RAM FIRST | 200 GiB + bigger instance |

**Important inconsistency to note:** "100 GB/shard" and "50 GiB volumes" cannot
coexist. Under the ≤50% co-located rule, a 50 GiB volume caps the `.duckdb` file
at **~24 GB today** — we are already effectively at a ~quarter of 100 GB.
Allowing 100 GB/shard would need ~200 GiB volumes (co-located) or ~120 GiB
(redirected spill) — 4–8× current EBS spend, the wrong direction.

### gp3 notes

- gp3 bills **capacity and IOPS/throughput separately** (3000 IOPS / 125 MB/s
  baseline free), so going to a smaller volume does **not** cost query
  performance.
- gp3 can grow online but **cannot shrink** — to go below 50 GiB, snapshot →
  restore to a smaller volume.

## Decisions still needed (open questions)

1. **Measure live:** actual `.duckdb` file size, row counts, and observed spill
   (`.tmp` dir) under peak load on a real shard. This is the missing input for a
   precise number — spill demand is workload-specific.
2. Confirm the insert pattern is batched (Appender/COPY), not row-by-row.
3. Confirm no ART/PK/UNIQUE indexes are silently in place.
4. Decide: co-located spill with ≤50% rule, OR redirect `temp_directory` to a
   dedicated small spill volume and size data volumes tighter.
5. Verify `temp_directory`/`max_temp_directory_size` behavior on the exact
   DuckDB version each shard runs.

## Action plan

1. SSM/kevcons into a shard; capture `.duckdb` size, row count, peak `.tmp`
   spill, DuckDB version, and whether any indexes exist.
2. Confirm insert path uses batched inserts and no unnecessary constraints.
3. If files are well under ~24 GB and spill is modest: **resize data volumes
   from 50 GiB → 30–40 GiB** (snapshot+restore, since gp3 can't shrink) to trim
   ~40% off the 4 data volumes (200 → ~120–160 GiB).
4. Set a disk-utilization alarm at **~65–70%** per shard as the early-warning
   line; treat ~50 GB/shard as the **repartition-now** red line.
5. If sustained spill is heavy, evaluate redirecting `temp_directory` to a
   dedicated volume vs. bumping instance RAM.

## Sources

- https://duckdb.org/docs/current/operations_manual/limits
- https://duckdb.org/docs/current/guides/performance/working_with_huge_databases
- https://duckdb.org/docs/current/guides/performance/how_to_tune_workloads
- https://duckdb.org/2024/07/09/memory-management
- https://duckdb.org/docs/lts/guides/troubleshooting/oom_errors
- https://duckdb.org/docs/current/operations_manual/footprint_of_duckdb/reclaiming_space
- https://github.com/duckdb/duckdb/issues/17210 (insert degradation — wide table, medium confidence)
- https://github.com/duckdb/duckdb/issues/14142 (temp-dir OOM error)
- https://github.com/duckdb/duckdb/pull/15057 (90%-of-free default)
- https://datamonkeysite.com/2025/10/19/running-duckdb-at-10-tb-scale/ (practitioner, spill redirect)
- https://motherduck.com/blog/separating-storage-compute-duckdb/
- https://aws.amazon.com/about-aws/whats-new/2025/09/amazon-ebs-size-provisioned-performance-gp3-volumes/
