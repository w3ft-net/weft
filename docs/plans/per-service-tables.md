# Per-service tables on shards

> **Status (2026-11-02).** Two layouts are in play (next section). The archive layout is
> live: the exporter writes it, and all five existing archives were converted and swapped on
> 2026-11-02. The live-shard table layout is built and deployed but off (`--per-key-tables`
> defaults to false and no unit file sets it), so every shard that still holds DuckDB data is
> on the single `logs` table. The evidence below was gathered on 2026-11-01 against production.

## Start here (handoff for a fresh session)

**Goal.** A query that names one log file (`cat /log/weft/shard/stats.log`) reads
only that file's rows on each shard, not every row in the time range. A shard's
rows for one (customer, namespace, service, kind) live in their own DuckDB table,
and the planner names the table, so no view and no `UNION ALL` is involved.

**Why.** Selective queries are slow because the shard reads data for every service
in the time range, not because of how they are planned or how much they return. See
"What we measured".

**Two layouts, not one.** "Per-key" names two things that are easy to run together:

- **Per-key archive layout** (Parquet in S3, partitioned by customer, namespace, service, kind,
  bucket width and time). It is what the exporter writes with `--keyed`, and what the
  conversion script wrote for the five existing archives. An archived shard has no DuckDB
  table at all, so this is the only layout it can have. **Live and measured.**
- **Per-key tables** (one DuckDB table per key on a shard that is still ingesting or waiting
  to archive). Behind `--per-key-tables`, **off**; today's ingesting and read-only shards all
  hold the single `logs` table. Not measured on a prod instance type.

The first speeds up queries that reach archived data. The second is what the hot-shard
problem below needs, and it is not delivered yet.

**Left to do.** See "Next steps (2026-11-02)" for the dated list, and "Cleanup" for the code
that is deleted once both layouts are settled. Details of the design are under "Design" and
"Rollout".

## What we measured

All numbers are from 2026-11-01. Where something was inferred, not measured, it says so.

**The symptom.** The `filter-count-qmax` benchmark query
(`cat /log/weft/shard/stats.log | jq 'select(.q_max_ms > 10)' | wc -l`, range
2026-10-27..2026-10-29) returns about 20,000 lines and took 6 to 12 s in
`bench/fieldtypes`. The three recordings (`baseline`, `after`, `after-oct31`) have
identical output and no meaningful speedup between them.

**Where the time goes** (`trace --id`, trace `bd1a5c55…`, 11.8 s):

| Time | Event |
|---|---|
| 0 ms | `weft/api` receives `POST /api/shell/exec` |
| 36 ms | coordinator planned, SQL chosen |
| 37 ms | two shard queries start in parallel |
| 8,105 ms | first shard (`bfed2003`) finishes |
| 11,806 ms | second shard (`a70003d3`) finishes |
| 11,809 ms | compute, coordinator and API done |

About 40 ms is outside the shards. The shards' own `stats.log` records agree: DuckDB
reported `q_max_ms` of 8,066 and 11,768 for the two shards in that interval.

**Not the plan.** A local shard with the real schema and the exact SQL pushes every
filter (`ts`, namespace, service, kind, the JSON predicates) into the table scan, and
parameterized SQL plans the same as inlined literals. It ran in 17 to 30 ms over 1.5M
rows. That data was 0.14 GB and fully cached, so it rules out the plan, not the I/O.

**Not CPU, not the volume cap, not ingest writes.** The shards are `t4g.micro`
(1 GiB RAM) holding 5.4 and 5.9 GB databases on gp3 volumes (3,000 IOPS, 125 MB/s).
CloudWatch for the slower shard:

- The data volume read 978 MB in one minute, in 3,875 reads of about 252 KB. Over a 12 s
  query that is about 82 MB/s, under the 125 MB/s cap, with a queue length averaging about
  0.5 in flight.
- CPU credit usage and 5-minute CPU averages show about 0.4 of 2 vCPUs busy (an upper
  bound: other queries ran in the same window).
- Ingest writes to the volume are steady at about 20 to 37 MB a minute, about 8 write
  ops/s.

So the scan is latency-bound with little parallelism: it waits on one read at a time
while the CPU and the device are mostly idle. This is a hypothesis that fits all the
numbers, not a measurement of the scan itself. One caveat: the large read is in the
06:21 EST CloudWatch bucket while the query ran 06:20:37 to 06:20:49; the 5-minute
instance metric covers both.

**Why a selective query reads so much (hypothesis).** `logs` is one table with no
declared order. Rows from all services are interleaved by time, so the matching rows
sit in every row group and the row groups' min/max cannot skip any. The scan reads the
data for every service in the time range. That range holds about 11M rows per shard
(the two ingesting shards hold 35.9M and 33.2M rows over roughly six days).
`PRAGMA storage_info('logs')` or `parquet_metadata()` on an archived shard would confirm
it and has not been run.

**Read-only is not faster per row.** The same `cat … | wc -l` over a range served only by
the READ_ONLY window (Oct 20 to 22) took 0.5 s for `shard/stats.log` (18,922 lines),
against 7.6 s for Oct 27 to 29 (20,670 lines). The ingesting window holds roughly 12x more
rows per day, and time scaled about 15x, so per-row cost looks similar. The 2026-11-02
rotation turns the current shards read-only, which gives a cleaner comparison: rerun the
`after-oct31` queries then.

## What we ruled out

- **Cluster rows when a shard goes read-only.** There is no downtime between ingesting and
  read-only, and it helps only read-only shards.
- **Sort each ingest batch.** DuckDB stores row groups of 122,880 rows and appends batches
  into the open row group, so a sorted batch still gives every row group nearly the full
  range of service names. Clustering needs a sort far larger than a row group, which is
  compaction.
- **Route a service to one shard.** It loses the redundancy of spreading writes across
  shards (a degraded shard would block its services) and the parallel reads to compute.
- **More gp3 throughput.** The query is not at the cap.
- **A view over per-service tables.** It costs about 70 us per table on every query that
  goes through it (4 ms at 30 tables, 21 ms at 300, 111 ms at 1,500, measured locally), and
  it must be recreated when a service appears. The planner can name the table, so no view
  is needed.

## Design

**The key.** (customer_id, namespace, service, kind). `env` stays a column: reader tokens
span envs (a 3-segment path reads every env), so an env in the key would turn that path
into a union over envs the planner cannot list. Customer is in the key because it is the
row-level-security boundary; the `customer_id` predicate stays on every query as defense in
depth until the final phase drops the column (see Rollout).

**Naming.** A pure function of the key, in a small package the planner and the shard both
import. Identifiers are customer-supplied and some contain slashes, and DuckDB compares
identifiers without regard to case even when quoted (`"Api"` and `"api"` are one table), so
a name built from the key would join keys. The name is `l_`, a readable form of the key
(lowercased, non-alphanumerics as `_`, cut to 48 characters), `_`, and 128 bits of a hash of
the exact key. The readable part is for people looking at a shard; the hash keeps keys apart.
The shard finds its log tables by that shape (`logtable.IsName`) and keeps no table of them.
When the key columns are dropped (below), the key moves into a `COMMENT ON TABLE`, created in
the same statement as the table.

**Planner.** `From{Table: "logs"}` is set in one place, `newSQLBuilder()`
(`planner/sql.go:197`), before the source is known. The table moves to where the source
fields are applied (`addSourcePredicates`, `planner/planner.go`). A single-file source gives
one table. The namespace, service and kind predicates can stay; they become redundant.

**Shard.**

- Create a table lazily from a template on the first row of a key. A key with no rows never
  gets one.
- Split each ingest batch by key and append to each table (`shard.go:471`).
- A query naming a table the shard does not have returns zero rows, not an error. Other
  errors still surface.
- `migrationSteps` assumes one table; loop it over the log tables and over the template.
- Field facts (`shard.go:800`), the row count and min/max (`shard.go:895`, `shard.go:908`),
  the export (`shard.go:932`, `export.go:102`), the janitor and `weft-admin shardcount`
  work per table.
- Exporting per table, partitioned by key, would let the cold path prune by service too.
  The cold shard rewrites `FROM logs` textually today (`cold_shard.go:150`).

**Multiple sources.** `cat` takes several files and a glob expands to several
(`extractServicesEnv`, `planner/parse.go:305`), and `trace` takes a list of namespaces. The
planner emits `IN` predicates for these today. Each becomes a list of tables, run as
sub-queries merged by the coordinator, or as an explicit `UNION ALL` over the named tables.
Which one depends on how `cat a b` orders rows (by timestamp or file by file); that has not
been checked.

## Rollout

Existing shards keep one `logs` table. A shard detects its layout when it opens: an old
shard keeps `logs`, and a shard created after the change uses per-key tables. Old shards age
out into archive.

The deploy-order rule is that old receivers stay correct. If the planner changed the SQL text
to name a table, an old shard would fail on it. Instead, add an optional field to the shard
query carrying the table name. A new shard uses it and rewrites `FROM logs`; an old shard
ignores it and runs the unchanged SQL. This depends on the shard query proto (not read yet)
taking an additive field.

**Final phase: drop `service` and `kind` (decided by Paul, 2026-11-01).** Once every live
shard is per-key, a table's name already says which service and kind it holds, so the two
columns repeat the same value on every row. Drop them from the keyed tables. This must come
last, because:

- Legacy shards still need the columns until they have aged out into archive, and a query
  sent to one still filters on them. The planner must stop emitting `service`/`kind`
  predicates only for hinted queries on per-key shards.
- An unhinted query over the union can no longer tell the services apart. Every remaining
  caller that relies on that (`weft-admin shardcount`, `service = ? AND raw LIKE ?`) must be
  moved to a hint first.
- Rows come back from a query with `service` and `kind` set, so the shard must fill them
  from the table's comment when it builds the result.
- The parquet export and the archive read the columns. The export must write them, taken
  from the table comments, so archived data keeps the schema that archive queries
  expect. Parquet archived before the change already has them.
- `namespace` is constant per table too, so it drops with the other two.
- `customer_id` drops too (Paul, 2026-11-01). Tenant isolation then rests on the table
  name, which `logtable.Name` derives from the customer, so the restriction moves to the
  layer that derives the name. That has consequences:
  - The coordinator must derive the table only from the authenticated customer, never from
    user input, and a test must show a query cannot name another tenant's table.
  - The unhinted fallback (union of every table) is a cross-tenant read once the column is
    gone. It exists only so un-migrated callers keep working during rollout. In this phase
    an unhinted query on a per-key shard must return an error.
  - The archive export keeps writing `customer_id` (from the table comments), because archive
    queries and the hive layout depend on it.

**Tests.** About 306 `FROM logs` statements in 68 test files, nearly all of which go through
the shard, so a shim that resolves `logs` to the hinted table should cover them. Three raw
`INSERT INTO logs` and four DDL statements need hand edits.

## Archive layout (Paul, 2026-11-01: convert archives first)

Archived shards can be rewritten to a layout that prunes by key, with a switch per archive,
without touching live shards. It also settles the archive/cold question below.

**Today.** An archive is `<prefix>/<shard>.attempt-N/m<minutes>/_part=<bucket start>/data_0.parquet`,
or one file for a small shard (`shard/partition.go`). Time is the only partition. Services
are interleaved inside every file, so the footer statistics can't prune them. Measured on a
real file (a 20 MiB day bucket of the dogfood archive, 262,209 rows, 3 row groups): the
statistics say every one of 10 services may be in all 3 row groups, and 9 of 10 really are.
`shard/stats` is 2.4% of that file's rows, `coordinator/main` 34%, `api/main` 27%, and a
query for any of them reads the whole file. The prod archive bucket holds 5.2 GiB in
`40-apps` (two shards, daily files near 500 MiB), small enough to convert in one pass.

**Layout.** Add the key to the hive partitions (the final form, with each key's own bucket
width in `_w`, is under "Sizing" below; this is the first probe, with one width):
`PARTITION_BY (customer_id, namespace, service, kind, _part)`, key directories first and
`_part` last, so `PruneColdPartitions` is unchanged. Probed with DuckDB 1.5.2 on a local
copy: 32 files, and a query with the planner's bound parameters read 1 of 32
(`File Filters: service, kind, namespace, customer_id`), with the right rows. So the
planner's existing predicates prune; the cold path needs no hint, no Lambda request change
and no compute change.

- Values with `/`, spaces, `'` and `=` are percent-encoded in the directory name
  (`service=a%2Fb%20c`) and read back whole.
- The partition columns must be read as `hive_types` (BIGINT, VARCHAR, VARCHAR, VARCHAR). A
  service named `123` is otherwise typed as a number when every value in the column looks
  like one. The archive reader is `parquetReadExpr` in `shard/cold_shard.go`, copied in
  `cmd/lambda-shard/handler.go`; both change, and the Lambda is a separate deploy.
- A case-insensitive filesystem merges `api` and `Api` into one directory and corrupted a
  file in the probe. S3 keys are case-sensitive, so prod is unaffected; a dev archive on
  macOS would not be. If that matters, name the directories with the table name
  (`logtable.Name`) instead of the raw values.
**Sizing (decided 2026-11-02).** The existing width comes from `choosePartitionMinutes`: the
shard's whole Parquet size over 16 MiB gives a file count, and the time span over that count
gives one width. Applied to keys that fits none of them. For a 3 GiB shard over 7 days it is
59 minutes: about 3,900 partitions, `weft/api/main` (half the bytes) in files of about 9 MiB,
`shard/stats` (2%) in files of 0.4 MiB, the smallest keys in a few KB. A fixed day bucket
(the first version's) is the opposite: about 214 MiB for the biggest key. So each key picks
its own width, in the `sized` layout:

`<shard>.attempt-N/sized/customer_id=…/namespace=…/service=…/kind=…/_w=<minutes>/_part=<bucket>/…`

- **Width.** `pickWidth(estParquetBytes, span)`: the key's share of the shard's Parquet bytes
  (its share of the text, plus 64 bytes a row) over its own first-to-last span gives a file
  count at 16 MiB, and the width is the narrowest of 1h, 2h, 6h, 12h, 1d, 2d, 7d that is not
  narrower than span over files. A key under one file gets the widest. Widths are whole
  hours dividing a week or whole days, so buckets line up with the calendar.
- **Reader.** `_w` is a partition column, so the low time bound is `CAST(_part AS TIMESTAMP) +
  to_minutes(CAST(_w AS INTEGER)) > F`: a row at or after F is in a bucket that ends after F,
  whatever the key's width. Probed with DuckDB 1.5.2: 3 of 175 files read for a two-hour
  window on the key with hourly buckets, same rows as the unpruned query. No list of widths
  is part of the contract, so a later writer may use others without a reader change.
- **Writers.** The shard export and the compactor's rewrite share `createKeyWidths` (one temp
  table of widths, joined to the rows) and `sizedSelect`. The compactor does not know the
  Parquet size of what it rewrites and estimates it from the text (`parquetPerRawByte`, 0.3),
  which errs towards narrower buckets.
- **Files per partition.** Left alone, a partitioned write splits partitions into many small
  files: DuckDB keeps at most 100 partition files open and starts a new file each time it
  reopens one (1,905 files for 308 partitions in a 6M-row trial). Sorting the input fixed
  that (326 files) but cannot be used on a whole archive: a real 2.5 GiB archive needed more
  than 6.5 GiB of spill under a 2 GiB function's settings and failed. Raising
  `partitioned_write_max_open_files` fixes it with no sort: the same trial gave exactly 308
  files for 308 partitions, in 5.9 s against 10, peak 1.29 GB under a 1,228 MB limit. A
  Lambda allows 1,024 open files, so there the cap is 900 and partitions past it fall back
  to extra files, not to a failure. A few extra files remain where DuckDB's writer threads
  meet, so tests bound the count rather than expect one file per partition.
- **Compatibility.** `k<minutes>` archives (one width for every key; written only between
  2026-11-01 23:30Z and this change, probably none) are still read. A retention rewrite of one
  produces a `sized` archive. Nothing writes `k` any more.
- **Not done: time-varying density.** A width is one number per key. A key that is quiet for
  six days and busy for one gets one width. Splitting per key and day is the next step if the
  archives show it matters.

**The switch.** A path segment records the layout, as `m<minutes>` records the bucket width.
An archive without it reads as it does now. Converting is one archive at a time, and rolling
one back is repointing `parquet_path` at the old object while it is still there.

**Converting existing archives (decided 2026-11-02: a one-off, not the janitor).** The sized
layout is what the export writes from now on, so the five archives that already existed were
converted once, by hand, from a workstation with the disk and memory for it: each was
downloaded, copied to the sized layout, verified against its export manifest and against the
original key by key, uploaded beside the original (`<archive>.r<ns>/sized/`), and swapped in
with an admin RPC that tombstoned the old objects for a week. The code for it (`cmd/convert-archive`,
`scripts/convert_archives.py`, `weft-admin swap-archive`, the `SwapShardArchive` RPC and its
coordinator side) is deleted, as are `CompareArchives`, `VerifyAgainstManifest` and
`CopyArchive`. The old objects were collected by the tombstone sweep, which now belongs to the
hosted retention package.

**Result (2026-11-02).** All five archives (17.0M to 21.2M rows each, 31 to 46 keys, 155 to
168 files of about 15 to 32 MiB) were converted, verified key by key, uploaded and swapped
between 12:36Z and 12:42Z. Each convert took 30 to 40 seconds on a workstation after a 1 to 2
minute download. Measured through the cold-shard Lambda on `a85dbee1`, the same SQL and a
one-hour window for one key (the Lambda invoked directly, warm, median of three, not
through the coordinator):

| Key | Rows in archive | Old layout (Lambda) | Sized layout (Lambda) |
|---|---|---|---|
| `loom/api/main` | 10.8M | 3.8 s | 0.77 s |
| `weft/coordinator/main` | 737k | 3.6 s | 0.70 s |
| `weft/shard/main` | 84k | 3.9 s | 0.70 s |
| `weft/compute/runtime` | 8k | 3.7 s | 0.63 s |

The old layout costs the same whatever the key's size, because it prunes only by time. The
sized layout opens only the key's directories. Through the CLI a full day of one service
(233,041 rows) and a week (1.5M rows) take 1.0 s and 1.6 s. Not measured: wider windows
against the old layout, or queries that name several keys.

**The janitor keeps the layout.** `RewriteRetention` writes a sized archive when its
destination is one, and `rewriteDestPath` gives a sized or keyed source a sized destination.
Otherwise a retention pass would collapse the archive into one file. Other archives are
rewritten as one file, as they always were.

**New archives.** A per-key live shard exports each table straight into its key's
directories (`exportParquet` reads `source()`, the union, today). That avoids building the
union, which was a memory worry on the 1 GiB shards.

**Risks.** A retention rewrite of a sized archive runs in the compactor Lambda (2 GiB, 300
seconds, 3 GiB of `/tmp` since 2026-11-02); it sets DuckDB's memory limit, threads and spill
directory from the function's size (`lambdaSettings`). Measured on `a85dbee1` (18.7M rows)
under those settings: 32 seconds, 1,224 MiB of spill, 1,413 MiB of process memory. The
Lambda's default 512 MiB of `/tmp` would not have held the spill, and that run read and wrote
local files, so S3 buffering is unmeasured. A Terraform apply reverts the cold Lambda to a
placeholder, so redeploy after (see the cold-shard deploy notes). The gain applies only to
queries that reach into archives (see "Result" above).

## Next steps (2026-11-02)

Decision (Paul, 2026-11-02): wait a week for the natural rotation. Nothing below needs doing
before Nov 8. All times are UTC and assume the bucket's `WindowDuration` and retention are 7
days, which I have not confirmed on the `buckets` row.

**What happens on its own, and what to check**

| When | What | Check |
|---|---|---|
| Nov 8 about 23:50 | Rotation: the Nov 2 shards (`68a0efb7`, `58f6c85c`) are within `PrewarmLead` of their `WindowEnd`. The Nov 9 cohort is minted, becomes `INGESTING`, then the Nov 2 shards go `READ_ONLY`. | Both new shards `INGESTING`, the old two `READ_ONLY`, `weft-admin audit` healthy, ingestion_errors still 0. |
| Nov 9 00:00 and the next bucket tick | Retention sweep archives the two `READ_ONLY` Oct 26 shards (`a70003d3`, `bfed2003`): `now - WindowEnd` passes 7 days. **First production run of the sized exporter**: the export host (`r7g.large`, coordinator image) writes `--keyed`. | A manifest per shard under `s3://weft-cold-220664822695/<prefix>/_manifests/` and a `parquet_path` ending `/sized/**/*.parquet`. `python3 scripts/convert_archives.py --stage plan` must report nothing to convert. Row and customer counts match the manifest (`VerifyAgainstManifest`). A CLI query of one key in their window is fast and returns rows. The shard's instance and volume are reaped. |
| Nov 9 about 12:36 to 12:42 | Tombstones on the five swapped archives pass 7 days (`ArchiveSwapGCDelay`); the janitor's `SweepTombstones` deletes the old objects. **Rollback by swapping back ends here.** | The old `attempt-1/` objects of the five shards are gone, the new `.r<ns>/sized/` ones remain, and queries still return. I have not seen a tombstone sweep delete anything yet. |

**Decisions and work before Nov 8 23:50 (or wait another week)**

1. **Per-key tables on live shards.** `--per-key-tables` goes in `deploy/weft-shard.service` as
   its own commit. It takes effect only on a new, empty shard database, so the Nov 9 cohort is
   the first that can be per-key; if the flag is not in a shard image by Nov 8 23:50, the next
   chance is Nov 16. First measure ingest and a single-key query on a `t4g.micro` (the prod
   shard type), since the cost per table there is unmeasured. With the flag on for the Nov 9
   cohort, the last legacy shard (Nov 2) archives on Nov 16.
2. **Terraform housekeeping.** The four catalog commits (`88bfe4c`, `c92d97f`, `bf38283`,
   `39ed47c`) are local and unpushed. The prod config blob still holds `KEYED_ARCHIVES=1`
   (set by hand on 2026-11-01, unread since the janitor conversion was removed); the next apply
   of `app_config_blob` drops it. That is an SSM change: read the plan for it before applying.
   `janitor_enabled` and `JANITOR_ENABLED` are also unread now that the flag is gone from the
   code, and can be deleted.
3. **Push** the local doc commit(s) with the implementation PR.

**Why the cascade cannot be started early.** Windows are `now.Truncate(WindowDuration)`, so
they are calendar-aligned, and the next window's shards cover future time. Scaling the target
down demotes the youngest shards, not the oldest. Raising `window_duration` mid-flight
re-aligns the window (14 days puts today's window at Oct 26 to Nov 9, while the live shards
cover Nov 2 to Nov 9), so the next rotation looks for shards in a window that has none and
mints into one about to end; it also pushes retention, and so the Oct 26 archive, back a week.
`POST /api/admin/shards/{id}/archive` does work on a `READ_ONLY` shard, with no retention
check (that check guards only deleting an archived shard), but it runs the export inline and
does not start a rotation. A real "rotate now" would need an explicit window anchor and a
fake-clock test of the bucket FSM (`b.clock()`); it is not designed.

## Cleanup: the migration code goes away

Everything that lets a shard be either layout, and everything that lets the SQL not name
its table, exists only for the migration. When it is over, the per-key layout is the only
path. Remove:

- `WithPerKeyTables`, the `--per-key-tables` flag, and the `shardOpts` plumbing through
  `NewBlankServer`, `NewServerFromIdentity` and `NewSpareServer`. `NewDuckDBShard` always
  uses the per-key layout.
- `initLayout`, the `perKey` field and the legacy ingest path (`ingestTable` into `logs`).
- The hint: `table` on `ExecuteQueryRequest` and `QueryRequest` (mark both numbers
  `reserved`, never reuse them), `TableQuerier`, `QueryContextTable`. The planner writes
  `FROM "l_..."` into the SQL.
- `resolveSource`, the `FROM logs` regex, and the unhinted union fallback. A query that
  names no table is an error: with `customer_id` dropped it would read every tenant.

Keep `logtable.Name` and `IsName`, per-key ingest, and `logs` as the empty template new
tables are made from.

Do this only when all three hold:

1. No legacy shard remains. A legacy shard lives until it archives (its window plus the
   retention period), so this is weeks after the flag goes on, not days. Check the shard
   registry, not the calendar.
2. The archive and cold path is settled. Parquet archived from legacy shards keeps the old
   schema, and the cold shard rewrites `FROM logs` (`cold_shard.go:150`), so a planner that
   names `"l_..."` needs either different SQL for live and cold shards or an archive split
   per table. See "Archive layout", which takes the second route.
3. Nothing that reads shards directly sends unnamed SQL: `weft-admin shard-count` and the
   lambda shard (`cmd/lambda-shard/handler.go:60`) must name tables first.

The column drop (see Rollout) goes with this: it needs the same conditions plus the table
comment holding the key.

### Archive side: the conversion and old-layout code

Separate from the live-shard list above, and earlier: it depends only on the archives, not
on the flag. Do it when both hold. Check `shard_records.parquet_path` of every `ARCHIVED`
shard, not only the manifests: every path ends `/sized/**/*.parquet` (or is a small-shard
single file), and the five swapped archives' old objects are gone (after Nov 9, see "Next
steps"). Then remove:

- **Reading the old layouts.** The `m<minutes>` and `k<minutes>` branches of
  `partitionSegment`, `PartitionMinutes` and `IsKeyed` (`shard/partition.go`), the matching
  branches of `ParquetReadExpr` and `keyedHiveTypes` (`shard/cold_shard.go`), and the
  width-less and `m`/`k` handling in `PruneColdPartitions` and `partBound`
  (`shard/cold_prune.go`, including `coarsestLegacyPartition`). Keep what the sized reader
  uses; the compiler and the cold-shard tests will say which that is.
- **Writing the old layouts.** `exportParquet` (the time-only export), the `keyed bool`
  carried through `archive`, `exportArchive` and `exportPartitioned` (`shard/shard.go`),
  `ExportConfig.Keyed` and the `--keyed` flag (`cmd/shard/export.go`,
  `deploy/weft-export.service`): the exporter then always writes the sized layout. The
  single-file path for a small shard in `exportArchive` runs only when S3 is not configured
  (local and dev); keep or drop it with that in mind.
- **The janitor's other branch.** The single-file destination in `rewriteDestPath`
  (`logdb/janitor.go`) and the plain `COPY ... TO file` in `RewriteRetention`, plus its error
  for a non-sized keyed destination (`shard/compact.go`).
- **The one-off conversion (done).** `cmd/convert-archive`, `scripts/convert_archives.py` and its
  tests, `CompareArchives`, `LocalPathOf` and `VerifyAgainstManifest` (`shard/archive_compare.go`;
  keep `VerifyAgainstManifest` only while the first production exports are being checked),
  and `SizedArchivePath` once `rewriteDestPath` has one branch. The compactor and exporter
  keep working without any of them.
- **The swap (done).** After the 7-day rollback window of the five swapped archives: the
  `SwapShardArchive` RPC and its messages (mark the field numbers `reserved`),
  `Provisioner.SwapArchive`, `ShardStore.SwapArchivePath`, `ErrArchivePathChanged`, the
  `swap` tombstone reason, and `weft-admin swap-archive` (`cmd/weft-admin/swaparchive.go`).
  Keep `ArchiveSwapGCDelay` and the tombstone sweep only if something else uses them.
- **Docs.** Shrink "Converting existing archives" to a short history, and update the runbook's
  "Sized archives" section (`docs/runbooks/retention-janitor.md`).

`weft-admin shard-count` still names no tables; it is on the live-shard list (condition 3).

## Expected gain, and what it will not fix

The `stats.log` query would scan its own table, about 20,000 rows, not about 11M. A large
service such as `weft/api/main` is about half of all rows and gains little; those queries
stay at several seconds. Two ideas for them, neither tested: a higher DuckDB `threads`
setting to keep more reads in flight on a 2-vCPU box (prod appears to run the default, not
confirmed), and a larger instance for caching.

## Measurements behind the design

**Key count.** `tree /log` over Oct 1 to now: 20 leaves for `graybeard` (14 under `weft`: api
1, compute 1, coordinator 6, shard 6; 4 under `vimgym`; 2 under `render-test`) and 6 for
`loom` (`prod` and `sand` x `api`, `edge`, `worker`, kind `main`), which is 3 keys with `env`
as a column. About 23 tables per shard.

Do not size from the shard registry: `shard_services` showed 39 (namespace, service) pairs
per shard and no kind. Seven of those are vestigial `log_groups` rows (`weft/shard/{heartbeats,
main,metrics,runtime}`, created 2026-05-29, and `loom/{api,edge,worker}`, created 2026-09-03):
no source exists under those names, and the shard's startup migration strips anything after a
`/` from stored service names. `weft/shard` has six kinds (disk, heartbeats, host, main,
runtime, stats), more than the four first assumed.

**Ingest cost** (local M2 with an SSD, DuckDB at 2 threads and 800 MB, 600,000 rows, one
Appender per table per 20k-row batch, rows spread evenly):

| Tables | Rows/s | 20k batch avg / worst | Final CHECKPOINT |
|---|---|---|---|
| 1 | 411,000 | 49 / 107 ms | about 0 |
| 30 | 199,000 | 101 / 224 ms | 130 ms |
| 300 | 28,000 | 712 ms / 1.5 s | 980 ms |
| 1,500 | 1,800 | 10.9 / 19 s | 5.5 s |

Cost per table per batch grows with the table count. At about 23 tables and about 130 rows/s
of real load there is a wide margin. Keeping Appenders open across batches would help beyond
that and was not tested. Absolute numbers on a `t4g.micro` with EBS will be worse.

## Open questions

1. How does `cat a b` order rows, by timestamp or file by file? It decides sub-queries versus
   `UNION ALL`.
2. ~~Does the shard query proto take an additive table-name field cleanly?~~ Yes: `table`
   (`ExecuteQueryRequest` 16, `QueryRequest` 4) is deployed and old receivers ignore it.
3. What bound do we put on tables per shard, since services are customer-controlled? The
   ingest numbers above suggest tens, not thousands.
4. Delete the seven vestigial `log_groups` rows? They add no tables under lazy creation, but
   they inflate the registry.
5. Confirm the hypothesis first: `parquet_metadata()` on an archived shard's export, or
   `PRAGMA storage_info('logs')` on a restored snapshot. Read-only and does not touch the live
   shards.
6. ~~Does the per-key table design also fit retention and the cold path as hoped, or does it
   change the archive step?~~ The archive step changed as hoped: archives are partitioned by
   key with a width per key (see "Archive layout"), live and measured. What is still open is
   the live-shard side: the exporter reading per-key tables rather than the union
   (`exportKeyedParquet` reads `source()`, the union, today).
7. Is the bucket's `WindowDuration` and retention really 7 days? "Next steps" assumes so and
   it has not been read from the `buckets` row.

## Known side issue

`trace --spans` clips its TOTAL column (`theme/span_rollup.go:596` prints a nanosecond
`Duration.String()`, and `PadLeftVisible` at `:461` keeps the tail of strings over 9
characters), so `17.023862ms` shows as `.023862ms`. A failing test is in
`theme/span_rollup_test.go`. Unrelated to this plan.
