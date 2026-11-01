# Per-service tables on shards

> **Status (2026-11-01): scoped, not started.** The evidence below was gathered on
> 2026-11-01 against production. Nothing here is built. The open questions at the end
> need answers before step 1.

## Start here (handoff for a fresh session)

**Goal.** A query that names one log file (`cat /log/weft/shard/stats.log`) reads
only that file's rows on each shard, not every row in the time range. A shard's
rows for one (customer, namespace, service, kind) live in their own DuckDB table,
and the planner names the table, so no view and no `UNION ALL` is involved.

**Why.** Selective queries are slow because the shard reads data for every service
in the time range, not because of how they are planned or how much they return. See
"What we measured".

**Left to do** (in rough order): answer the open questions; add the table-name function
and the shard's dual-layout support; ship the additive wire field; teach the planner to
send the table name; move facts, export and the janitor onto it; measure on the
2026-11-02 rotation shards. Details under "Design" and "Rollout".

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
row-level-security boundary. The column goes away in the conversion (see Rollout), so the
boundary becomes the table name, derived from the authenticated customer.

**Naming.** A pure function of the key, in a small package the planner and the shard both
import. Identifiers are customer-supplied and some contain slashes, so the table name is a
hash (at least 128 bits), always quoted, never interpolated text. Each shard keeps a catalog
table mapping key to table name for readable diagnostics and for migrations to iterate.

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
- `migrationSteps` assumes one table; loop it over the catalog and over the template.
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

Decided by Paul, 2026-11-01: take downtime and convert in one step. There is no period with
two layouts, so the deploy-order rule (old receivers stay correct) does not apply.

Maintenance window:

1. Stop ingest and queries. The coordinator, compute and shards all stop.
2. Rewrite every shard database that is not yet archived (INGESTING, READ_ONLY, and any in
   between). For each key in `logs`: create its table, `INSERT ... SELECT` ordered by `ts`,
   record it in `log_tables`. Then empty `logs` (it stays as the template) and drop
   `customer_id`, `namespace`, `service` and `kind` from the keyed tables. Ordering by `ts`
   also clusters each table by time, which the live layout never had.
3. Deploy the new coordinator, compute and shard together. The planner writes the table into
   the SQL (`FROM "l_..."`). The shard runs the SQL as written.
4. Bring everything up and check row counts per key against what step 2 started with.

**What this removes from the first design:** the `table` wire field, the optional
`TableQuerier` interface, layout detection (`initLayout`, `WithPerKeyTables`), the unhinted
union fallback, and the `FROM logs` rewrite (`resolveSource`). The shard has one layout. Kept
from steps 1 and 2: `logtable.Name`, the per-key ingest, `ensureTables`, `log_tables`, and
`logs` as the empty template.

**Dropped columns.** All four are constant within a table, and the table name carries them.
Consequences:

- Tenant isolation rests on the table name. The planner must derive it only from the
  authenticated customer, never from user input, and a test must show a query cannot name
  another tenant's table.
- Rows returned by a query still carry `namespace`, `service`, `kind` and `customer_id`; the
  shard fills them from `log_tables`.
- The planner stops emitting predicates on the four columns for live shards.
- `weft-admin shardcount` (`service = ? AND raw LIKE ?`) and the lambda shard
  (`cmd/lambda-shard/handler.go:60`) are rewritten to name tables.

**Archive.** Archived parquet keeps the old schema, four columns included. The export writes
them (from `log_tables`, per table) so new archives match. The cold shard rewrites `FROM
logs` textually (`cold_shard.go:150`), so a planner that names `"l_..."` needs a matching
rewrite there, and the cold path still needs the service predicates because parquet is not
split per table. Either the planner emits different SQL for archived shards, or the export
partitions parquet per table so the cold path can prune by path. This is not settled.

**Risks to check before the window.**

- Shards are `t4g.micro` with 1 GiB RAM. A sorted `INSERT ... SELECT` over a full shard may
  spill; time it on a restored snapshot first, and take an EBS snapshot of each shard before
  the rewrite as the rollback.
- Ingest during the window. Sources must buffer or retry (weftd's journal tailer, Kinesis
  retention, anything else that posts). Not yet verified.
- Window length is set by the slowest shard rewrite.

**Missing tables.** The planner cannot see each shard's catalog, so it names tables that a
shard may not have (a key with no rows yet). The shard must treat that as zero rows and not as
an error. Leading option: on a "table does not exist" error for a name in the `l_` form, rerun
against the empty template. Not settled.

**Tests.** About 306 `FROM logs` statements in 68 test files go through the shard and must name
the table instead (mechanical, with gopatch). Three raw `INSERT INTO logs` and four DDL
statements need hand edits.

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
2. Does the shard query proto take an additive table-name field cleanly?
3. What bound do we put on tables per shard, since services are customer-controlled? The
   ingest numbers above suggest tens, not thousands.
4. Delete the seven vestigial `log_groups` rows? They add no tables under lazy creation, but
   they inflate the registry.
5. Confirm the hypothesis first: `parquet_metadata()` on an archived shard's export, or
   `PRAGMA storage_info('logs')` on a restored snapshot. Read-only and does not touch the live
   shards.
6. Does the per-key table design also fit retention and the cold path as hoped (per-table
   export partitioned by key), or does it change the archive step?

## Known side issue

`trace --spans` clips its TOTAL column (`theme/span_rollup.go:596` prints a nanosecond
`Duration.String()`, and `PadLeftVisible` at `:461` keeps the tail of strings over 9
characters), so `17.023862ms` shows as `.023862ms`. A failing test is in
`theme/span_rollup_test.go`. Unrelated to this plan.
