# Field row spec: sending a compute stage only the fields it reads

Status: 2026-10-31. Everything below is on `origin/main`. CI is green on the last commit
(`8cb6355c`) and its Deploy workflow reported success. The deploy of `7e6ca66b` (the first with
the keyed aggregates) is followed by the bot's migrations-collapse commit (`37aad2da`, "through the
just-deployed AMI"), which says that AMI was deployed; the instances and their versions were not
checked (see "Still to verify").

| | State |
|---|---|
| Deploy 1: add (row spec, `field_sql`, typed exact plan, `-F`/`-v` flag handling) | on `origin/main` (`2c6bd346` through `c8c26e38`) |
| Deploy 2: remove (text record, regexp, `fieldSelect`; `LoweredPlan` carries the field form) | on `origin/main` (`39d6ac19`), pushed after the migrations collapse. The gate below was **not** checked by this plan's author: see "Still to verify". |
| Compute performance (buffered pipes, meta-variable narrowing, jq per-input variables, awk per-record binding) | on `origin/main`, with warp v0.2.57 to v0.2.60 |
| Deploy 3: shard-side keyed aggregates (sum, max, min) and `sort -k`/`head` folded around them | on `origin/main` (`08193a7d`, `0cf4a53e`, `7e6ca66b`), in the **definitive** form: the plan is chosen once from the field facts, with no run-time check and no retry. See "Deploy 3" below, `field-types.md` "Keyed sum/min/max: built", and `shard-plan-variants.md`. Rolls coordinator, compute, shards. |

It follows `field-types.md`, whose exact plan (`QueryPlan.Typed.Exact`) already lowers an awk
filter-and-print over typed fields to SQL alone.

## Problem

An awk stage that stays in compute and reads only a few fields still receives every whole
line from the shard. `projectFields` (`planner/fieldproj.go`) shrinks that today by having
the shard build a *synthetic text record* (`a _ c` for a program that reads `$1` and `$3`).
It has three faults:

- the SQL is built as a string in the backend (`c.fieldSelect`), after the IR passes, and
  re-parses the compute stage's own text, which the IR contract forbids;
- it tokenizes `raw` with `regexp_extract_all` at every position, although the row's
  `fields` column already holds the split;
- compute then splits the text it was just handed.

## Design

The request carries a **row spec**: the positions the stage reads (`$1`, `$3`) and whether
it reads `NF` or `$NF`. The shard selects `list_extract(fields, N)` as plain columns, plus
`len(fields)` when `NF` is read, and returns them as a repeated field on the result row.
Compute rebuilds the record the stage reads: placeholders for skipped positions, the real
count for `NF`, the real last field for `$NF`. (A warp API that sets an interpreter's fields
directly was considered to skip `setRecord`; profiling compute showed the interpreter is not
where the time goes, see "Compute performance", so it was dropped.)

The spec is an IR node on the awk stage (produced by the frontend, gated by a pass,
rendered by the backend), so `explain` shows it and nothing re-reads a compute string.

## Which cases use it

Only awk stages that remain in compute and read a few fields: multiple rules, `printf` with
arithmetic, an `END` that uses arrays, and the keyed sum or max/min where the shards lack the
facts for them. A program whose pattern and print both lower (`awk '$3 > 100 {print $1, $3}'`
on a typed shard) has no stage and no record. Shard-side keyed aggregates (Deploy 3) took over
most of the remaining cases wherever every shard in the query has the facts, which made this work
less valuable than it looked when it was planned; it still shrinks what compute receives
everywhere else.

## Deploy rules

Every deploy rolls **coordinator, then compute, then shards**, so at every moment a request
sender is the same age as its receiver or newer, and a row sender (the shard) is older.

- A newer sender may add fields to a request. An older receiver ignores them and does what
  it did before, which must still be correct.
- A newer receiver of rows must accept the old row form and the new one.
- A shard launched mid-rollout runs the new binary against a compute that may be old, so a
  shard may only change behavior when the request it receives asks for it.

This is the pattern `ShardDescriptor.sql` and the typed variant already use.

## Deploy 1: add

Ships together with the local commits that are not yet deployed (typed filters, exact
plan, strict-decimal classifier, warp bump, benchmarks).

- **No warp release.** Compute rebuilds the text record from the fields (`compute.RebuildRecord`);
  a warp `SetFields` API that skips `setRecord` is a later optimization.
- **Coordinator:** the planner emits the row-spec IR node and a second SQL, `field_sql`,
  that selects the field columns (no regexp). `ExecuteQueryRequest` gains `field_spec` and
  `field_sql`. The request's `sql` stays the current text-record SQL.
- **Compute:** forwards `field_spec` and `field_sql` on the shard Query call. Accepts a
  `ResultRow` with `fields` filled (rebuilds the record) or empty (uses `raw`, as now).
- **Shard:** a Query that carries `field_sql` runs it and fills `fields`; otherwise it runs
  `sql` as now.

Mixed states during the roll, each correct:

| Coordinator | Compute | Shard | Result |
|---|---|---|---|
| new | old | old | compute ignores the new fields; today's plan runs |
| new | new | old | shard ignores `field_sql`; returns text records; compute reads them as `raw` |
| new | new | new | shard runs `field_sql`; compute rebuilds the record |
| new | old | new (launched mid-roll) | the Query has no `field_sql`; shard runs `sql` |

Verification before moving on: the difftest corpus runs the stage over all three row forms
and compares with the reference stage; `BenchmarkAwkFilterPrint`-style benchmark for a
stage that stays in compute, with the shard SQL, the wire bytes and the whole query.

Rollback: revert the commit and redeploy. Nothing persisted changes.

## Gate between Deploy 1 and Deploy 2

Every shard must run Deploy 1's binary, including any left on an older AMI (last time two
did). Check `binary_sha` across `shard_records` for every non-archived shard. This is for
speed, not correctness: a shard that is still old would return whole lines, which compute
accepts.

## Deploy 2: remove

Coordinator only in effect. Built in the commit after Deploy 1's; do not push it until the
gate passes.

- Deleted: `syntheticRecordSQL`, `compiled.fieldSelect`, the `Lines` unnest the text record
  needed, and the regexp. `projectFields` stays, as the pass that decides the field spec.
- `DirectShardSQL` is now the plain query (whole lines); `FieldSQL` is the only projected
  form. A shard without the field form (a cold shard, one from before Deploy 1) sends whole
  lines, which compute takes as `raw`.
- `LoweredPlan` carries `field_sql` and `field_spec`, so a cron or dashboard fire sends the
  field form as an interactive query does. Without them a cron's awk stage would have lost the
  projection it had when the text record was baked into `DirectShardSQL`. Plans lowered
  before this still work: their `DirectShardSQL` holds the text record.
- `TestFieldProjectionShrinksTheWire` measures the field form.
  `TestDifferentialFieldProjectionLines` now stores events as `SplitLines` does and checks
  the field form against the reference. (Its rows must not carry a JSON payload, which
  `SplitLines` never splits; a multi-line row in the shard is the residue under open point 2.)

What it costs where the field form isn't available, on 200k lines and a stage reading one
field: the whole line is 11.0 MB of values against 0.5 MB in the field form, where the text
record sent 1.3 MB. In-process, where bytes are free, the whole query takes the same time
either way (707 ms whole lines, 727 ms field form), so the saving is wire bytes only.

Rollback: revert and redeploy; Deploy 1's compute and shard still accept both forms.

## Deploy 3: shard-side aggregates

Keyed sum, max and min as a shard `GROUP BY`, built in the form agreed with Paul after two
other forms were tried and removed (`shard-plan-variants.md`):

- **Definitive, not checked.** The coordinator picks the plan once, from what each shard has
  said about its data, and nothing is verified afterwards: no run-time exactness test, no retry, no
  fallback plan. If every shard in the query has the facts the aggregate needs, they all run the
  `GROUP BY`; if any lacks them, the base plan runs for the whole query (`exactPlan`). A shard
  that fails fails the query.
- **A sum is exact integer arithmetic.** It needs a new fact, `$N#int` (a decimal integer of at
  most 15 digits); the shard adds `BIGINT`s (128 bits in DuckDB), compute adds the totals in
  arbitrary precision, and there is no bound to check. It differs from awk's float sum only beyond
  2^53.
- **max and min** follow warp's awk: a key whose values never beat 0 is absent, the printed text is
  the first row holding the best number, and ties across shards go by time.
- **`sort -k` and `head`** around the aggregate: a sort before a count or sum is dropped, and
  `sort -k2 -n` (and `-rn`, `-k2,2nr`, the other plain forms) with a `head -n N` after it folds into
  the group, applied to the printed lines with warp's own comparison.

200k lines and about 5,000 keys, in process (`BenchmarkKeyedAggregate`): a keyed sum 172 ms to
23 ms and a keyed max 156 ms to 33 ms through compute, with 200,000 rows from the shard down to
5,000.

Evidence on how much the row spec is used, from the coordinator's audit log
(`/log/weft/coordinator/queries.log`, dogfood org only, 10-18 to 10-31): no query has reached
the field projection since it shipped (checked with a control query that did), and of about
13,600 awk stages that stayed in compute, 99.9% were one monitoring query
(`awk 'LEVEL == "ERROR"' | wc -l`) that the current planner lowers to a `count(*)`. About five
ad-hoc queries over two weeks would have used the row spec, all keyed aggregates. That is weak
evidence about customers, who do not exist yet. The decision was to build the row spec
regardless; Deploy 3 has since removed most of what was left for it.

## Decisions for Deploy 1 (settled)

### 1. How the shard maps `field_sql` onto `ResultRow.fields`

`shard.ScanResultRows` (`shard/scan.go`) is positional and string-typed: column 0 is `raw`
(a non-null `string`), column 1 is `ts`, and every further column is scanned into a
`sql.NullString` and routed by name: `event_id`, `line_no`, anything else into
`ResultRow.MetaVars`. A DuckDB `LIST` column would not scan into a `NullString`, so the
fields cannot ride as one array column without changing the scan.

Proposal: keep the scan string-based and add two name conventions to its switch, so the
SQL stays plain and legible:

    SELECT '' AS raw, ts, event_id, line_no,
           list_extract(fields, 1) AS field_1,
           list_extract(fields, 3) AS field_3,
           len(fields)             AS nf
    FROM logs WHERE ...

- `field_<N>` appends to `ResultRow.fields` in column order. The request's `field_spec`
  says which positions those are, so the row carries values only. A NULL (the row has
  fewer fields) is an empty entry.
- `nf` sets `ResultRow.field_count`, and is always selected when a spec is present. Its
  presence is what tells compute "this row is in field form", so a row with no fields
  (`nf = 0`) is distinguishable from a row in raw form. That needs presence on the wire:
  `optional int32 field_count`.
- `raw` is `''` in field form, not NULL, because column 0 scans into a plain `string`.
- Ordering and the merge key are untouched: `ts`, `event_id` and `line_no` are the same
  columns as today.

New proto fields: `ResultRow.fields` (repeated string), `ResultRow.field_count`
(optional int32), `ExecuteQueryRequest.field_spec` and `.field_sql`, and the matching
fields on the shard Query request. All are additions, so older receivers skip them.

### 2. Multi-line rows

Ingest stores a multi-line text event as one row per line (`ingest.SplitLines`, called by
`ingestBatch` since 2026-10-14; `docs/query-execution.md`, "Rows are lines"). So a stored
text row is a line, `fields` is that line's split, and `list_extract(fields, N)` is awk's
`$N` for it. `TestDifferentialMultiLineEventsLoweredFields` and
`...ExactPlan` pin this: events of several lines, with blank lines, a trailing newline and
CRLF, go through `SplitLines` into the shards, and the lowered `$N` pipelines and the exact
plan match awk.

The row spec therefore needs no multi-line case. The field form is unconditional, and the
unnest (`unnest(string_split(raw, chr(10)))`) and `contains(raw, chr(10))` that an earlier
draft carried are not needed.

Rows that can still hold a newline are the residue the docs already name:

- rows ingested before 2026-10-14, whose `fields` is also the older schema-dependent split
  (`awkFields`, `docs/query-execution.md`), and so already disagree with awk on `$N`;
- a JSON line that is its own payload, which is never split (jq reads the payload once per
  event), if its `raw` contains a newline.

For these the field form is no worse than the lowered `$N` pipelines are today. It is
wrong where they are wrong, until the pre-2026-10-14 rows age out of retention. The old
projection handled them (it unnested), so the removal in Deploy 2 gives that up; check
the retention horizon before Deploy 2 and say so in its commit.

## Compute performance

Found by profiling a stage that stays in compute (`BenchmarkFieldRowSpec`, then
`BenchmarkMetaVarStages` and `compute.BenchmarkStages`). The row shape was not the cost.
Compute's own work for 200k rows is small (merge about 45 ms, awk about 30 ms); the rest was
goroutine handoff and per-row work for meta-variables that mostly no stage reads. All of it is
compute and coordinator code with no new wire fields, so the coordinator, compute, shard order
holds and an older compute simply runs slower.

1. **Buffered pipes** (`compute/pipe.go`). The stage's stdin and stdout were `io.Pipe`s, so
   each row cost two blocking handoffs on the way in and one per output line on the way out.
   `chunkPipe` buffers like an OS pipe (writes return once buffered, reads take what is
   there, the writer waits only when 1 MB is full). It replaced an interim `bufio` writer
   that needed flush rules and deadlocked once against the meta-variable queue.
2. **Meta-variables only for stages that read them.** The awk meta-variable queue, its
   per-row maps and `FormatTS` ran for every awk first stage; the jq `JqPerInput` hook ran for
   every jq stage and unwrapped any payload shaped like the old envelope (a bug).
3. **Only the meta-variables a stage names.** `planner.MetaRead` says which columns a stage
   reads (or all, for `awk -f` or a jq that doesn't parse). The shard projects only those
   (`Query.metaCols`, replacing an all-or-nothing flag), compute builds and binds only those
   (`awkBinder`, `JqBinder`), and the planner takes the union over the row stages and the
   first compute stage only, since a later stage reads what the one before wrote. The live
   filter uses the same.
4. **jq variables per input** (warp v0.2.57, `jq.Options.InputVars`). The row's payload now
   reaches jq as it is; compute queues each row's variables and the hook takes them in
   order. This replaced an envelope that was marshalled, wrapped, parsed and unwrapped per
   row. A positional scheme has a hazard (a payload of two JSON values shifts every later
   row's variables), so the writer checks each payload is exactly one valid value and fails
   the run otherwise, as an invalid payload always has; empty payloads queue nothing; and a
   stop channel releases a writer waiting on a stage that never reads (a buffered pipe lets
   the writer get ahead of one).

Whole query through the difftest harness, 200k rows, before this work and now:

| Stage | Before | Now |
|---|---|---|
| awk reading `LEVEL` | 1,626 ms | 290 ms |
| awk reading `LEVEL` and `HOST` | 1,546 ms | 332 ms |
| jq `. + {l: $level}` | 7,811 ms | 857 ms |
| awk printing 200k lines | 326 ms | 159 ms |
| awk stage reading one field (whole lines) | 752 ms | 156 ms |
| `awk '$3 > 100 {print $1, $3}'`, base plan | 1,062 ms | about 190 ms |

Compute alone (`BenchmarkStages`): a jq stage reading `$level` is 672 ms against 701 ms for
one reading nothing, so the meta cost is gone; the rest is warp's jq at about 3.4 us an input.

An earlier figure in `field-types.md` for the exact plan was overstated by the harness
pre-running each request's SQL; `harness.unchecked` fixes it for benchmarks.

Tests that pin it: `compute/pipe_test.go` (ordering, no wait for the reader, immediate
visibility, backpressure, reader close, concurrent, run under `-race`);
`compute/stagewriter_test.go` (meta-variables stay with their row over 5,000 rows, a slow
producer's first row isn't held back, `head` doesn't strand the writer, two-value payloads
fail instead of misbinding, a non-reading stage doesn't strand the writer, only the columns a
stage reads are built); `planner/meta_test.go` (`MetaRead` for 13 stage shapes, the projected
columns). Each safeguard was checked by removing it and watching its test fail.

## Bugs found on the way

- `awk -F, '{ s += $2 }'` was cut down to whitespace-split fields, changing the answer (the
  projection from `497fb9e0`). Fixed by moving the analysis into the frontend; `explain` now
  names the flag (`-F cannot be optimized`). `-v name=x` stages are projected again; `-v FS`,
  `-v RS` and `-v NF` are not, since they are `-F` by another route.
- A jq stage unwrapped any payload that looked like the old envelope.
- The integer fact (`$N#int`) broke two `cmd/shard` tests that count the facts a shard reports
  (`TestFieldFacts_ShardToCoordinatorToPostgres`, `...ALostAckIsResentAndTheDuplicateIsHarmless`),
  which failed two CI runs and kept both from deploying. They now expect the third fact, and
  `TestFieldFacts_KeyedAggregatePlansAskForTheKeysTheShardReports` checks a sum plan's keys reach
  the registry (`7e6ca66b`). The local runs behind the earlier "all green" used a hand-picked package
  list that left out `cmd/*`; the check is `go test -tags test ./...`, which is what CI runs.
- The pre-existing exact `$N` lowering is correct for stored text events (ingest splits them
  into lines); a test that fed multi-line rows straight to a shard looked like a bug and was
  not real input.

## Still to verify

- **This deploy.** The evidence that it rolled out is indirect: CI and Deploy green, and a
  migrations-collapse commit that names the just-deployed AMI. Nobody has looked at the running
  coordinator, compute and shards to see which build they run. Until compute has rolled, a count
  with a `sort`/`head` folded into it errors ("unknown group order") on a coordinator that is
  already new; nothing else is affected, because no exact plan can be chosen before shards report
  facts.
- **Deploy 2's gate** (not checked by me): every non-archived shard, cold shards included, on
  Deploy 1's binary (`binary_sha` in `shard_records`); no stored row with both a newline and a
  JSON payload (the field form reads such a row wrongly); the retention horizon for rows
  ingested before 2026-10-14, whose `fields` is the older split. A shard without the field form
  now sends whole lines: 11.0 MB against 0.5 MB for one field of 200k.
- **Facts in prod.** None exist until the Nov 2 rotation creates new-window shards. Then:
  confirm shards report facts (`shard_field_facts`), now including `$N#int` from the new shard
  binary, and that a keyed aggregate over a typed range is routed to the `GROUP BY`; record a
  baseline on a range inside the new window with `bench/fieldtypes` and run `after`. The keyed
  aggregates have run only in process so far, and `bench/fieldtypes` has no source with numeric
  awk columns for them (its own sources are JSON): they need a text source such as an access log.
  A shard created mid-window since the facts deploy would carry facts under the old classifier
  grammar (`inf`, `nan` and hex floats counted as numbers); none are expected.

## Next

- ~~Single-pass JSON parse in warp's jq.~~ Done in warp v0.2.58 (pushed): jsoniter's
  iterator builds each `Value` as it scans, behind a wrapper that closes four leniencies
  (number at end of stream, truncated input, number grammar such as `01`, invalid UTF-8). A
  hand-written reader was no faster than jsoniter (1.29 ms against 1.15 ms for 1,000 objects;
  fastjson 1.17 ms, sonic 3.2 ms), so the cost left is the `Value` tree (about 26 allocations
  a row), not the parser. A jq stage in compute: 672 ms to 356 ms reading `$level`, 666 ms to
  254 ms reading nothing.
- ~~Deploy 3~~ built (`field-types.md`): 200k lines, about 5,000 keys, in-process
  (`BenchmarkKeyedAggregate`): a keyed sum 172 ms to 23 ms and a keyed max 156 ms to 33 ms through
  compute, with 200,000 rows from the shard down to 5,000. It takes over the cases the row spec
  existed for (keyed sum and max, which the section above called most of what is left), but only
  where the shards report the facts; elsewhere the row spec still shrinks what compute receives.
- ~~A stage that reads meta-variables pays per row for two maps.~~ Done in warp v0.2.59
  (pushed): `PerRecordBind` takes a slice the caller reuses, and compute builds it
  from the row (`awkBinder.bind`) and recycles the slices through a free list. The rest of
  the cost was `strconv.ParseFloat` allocating an error for every text value tested for being a
  number (a level, a timestamp, a field compared with a string): `SetVar`, `Number()` and
  comparisons now reject text without calling it (`parseFloatQuiet`, checked against
  `ParseFloat` on 200,000 random strings). 200k rows, an awk stage reading `LEVEL`: 127 ms to
  88 ms, 2.8M allocations to 1.07M, against 52 ms for a stage that reads none. The live
  filter (`RowStages.Keep`) still uses the map hook; its meta map comes from the coordinator.
- ~~A race in `planner/difftest` under `-race`.~~ Fixed in warp v0.2.59. The stages of a
  pipeline run concurrently and share the one stderr the embedder gave (`stderrBuf` in
  `Job.consume`, a `strings.Builder`), so two stages writing errors raced. It showed on
  `head -n 50 ... | wc -l | jq 'select(.status == 500)' | cut`, and predates this work
  (it reproduces with warp v0.2.57). `execPipelineMulti` now gives its stages one locked
  stderr, so no embedder needs a thread-safe writer. Pinned by
  `TestJob_ConcurrentStageErrorsDoNotRaceOnStderr` and warp's
  `TestPipelineStagesShareStderrSafely`, both failing before.
- ~~A race in warp's parallel pipeline path.~~ Fixed in warp `a93fdda` (pushed, `v0.2.60`).
  `TestParallelGrepSort` failed under `-race`: the chunks of a parallel segment run at once and
  each wrote the shared interpreter's exit status while another forked from it. Writing it
  concurrently also left the pipeline with no defined status (whichever chunk finished last), and a
  segment fed no input never ran, so the status of the segment before it leaked through
  (`grep NOPE | sort` exited 1 in parallel and 0 serially, on every run). A pipeline now ends
  with the status serial gives it: success if any chunk succeeded, and a segment with no input
  runs once for its own status. `TestParallelPipelineStatusMatchesSerial` pins it. Nothing in weft
  sets `Parallelism`, so no weft path reached it.
- `bench/fieldtypes/results/after.json` is the pre-rotation run and is still untracked.
- ~~A race in `explain`'s line count.~~ Fixed. `TestShellExec_ExplainSurvivesACountThatFails/too_slow`
  failed once in CI (it had passed on 20+ earlier runs) and passed on rerun. `countRetrieved`
  (`api/explain.go`) reported "took longer than 50ms" only if its own context had expired, but the
  coordinator enforces the deadline it was sent with a timer of its own, and when that fires first
  the stream ends with `Canceled` (`RST_STREAM CANCEL`) or `DeadlineExceeded` while the client's
  context is still alive. `endedByDeadline` now waits up to 100ms for the client's deadline when the
  stream ends with either code and reports the timeout if it passes; a cancellation with the deadline
  far off keeps its own message and is not waited on. Two new cases reproduce it deterministically
  (they failed before the fix) and a third covers a cancellation that is not the deadline.
- **Open: per-shard plans.** A plan keyed to arrays of shards on the wire (a stage list per group
  of shards) is not built. The keyed aggregates did not need it (a shard without the facts
  cannot join an exact merge, so the whole query takes the base plan). The one candidate is the
  numeric-filter exact plan, where shards with the facts need no awk stage and the rest do.
- **Open: remaining jq cost** is the `Value` tree, about 26 allocations a row, not the parser.
