# Field types: gradual typing of what the shards ingest

> **Status (2026-10-31): the exact numeric filters and the keyed sum/max/min are built; see
> their sections below.** Decided in conversation
> with Paul; the build order is under "Rollout". Related: `query-limits.md` (the
> keyed sum/min/max and exact numeric filters wait on this), `planner-ir.md`.

## Problem

Several lowerings are exact only if a field's values are all of one kind:

- awk compares a number-looking field as a number and anything else as a string,
  so `$5 > 500` can only be a prefilter and `m[$3] = $5` max/min cannot lower.
- awk reads `"12abc"` as 12 where SQL's cast reads NULL, so a keyed sum cannot lower.
- jq's comparisons and arithmetic depend on the JSON type of the value.

We control ingest, the shard, awk and jq, so we can learn what each source has
actually held and lower when the answer is "only numbers", falling back when it
isn't.

## Decisions (Paul, 2026-10-30)

- **Ingest never waits.** A classifier goroutine in the shard process is handed a
  reference to each batch at the same time as the append. It adds no work to the
  ingest path.
- **Tolerate a short wrong window.** A fact is a guess promoted by evidence: after
  N conforming rows a locator counts as typed (case 1: until then queries are just
  less optimal). A violating row marks it dirty (case 2). A query that ran in the
  few milliseconds between the row landing and the flip may be slightly wrong, and
  that is accepted, as an artifact, not repaired. **No undo and no recompute** of
  cron history, alerts or materialized views.
- **Facts are per (shard, source, locator), and dirty is permanent for that
  shard.** A shard is a time window, so a stray row costs one window, and cold or
  never-classified shards are simply unknown. No coverage timestamps or day buckets.
- **New shards start unknown**, and are typed only by their own first N rows. No
  seeding from the previous shard.
- **The planner stays pure and emits both variants.** The coordinator picks per
  shard at dispatch from its registry. A fact flipping never forces a relowering:
  persisted cron plans just work.

## The fact

Key: `(customer_id, namespace, service, kind, env)` plus a locator, the awk field
`N` or a top-level jq key. Customer is part of the key because shards are shared
across customers. Env is part of it so a dirty row in staging doesn't downgrade
production.

Value, per shard: `classes` (the set seen: missing/empty, integer within 2^53,
other number, non-numeric text; for jq also null, bool, array, object), `state`
(`UNKNOWN`, `TYPED`, `DIRTY`), an `epoch` that only increases, and a count of
conforming rows. For a sum to be provably exact the shard also keeps the largest
absolute value seen.

The classifier is warp's own predicate for "looks numeric" (awk) and for jq's
number types, used by both the interpreter and ingest, so the fact and the runtime
cannot disagree. warp exports only that predicate, so its API stays weft-agnostic.

N is 1,000 conforming rows: a locator is typed once the classifier has seen 1,000
rows of that (shard, source, locator), all present and of an allowed class, and no
violation. A missing or empty value is its own class and keeps a locator from being
typed numeric.

Tracked locators are capped at 50 per source, awk fields and top-level jq keys
together, first seen first kept. Beyond the cap a locator is simply unknown.

## Reporting (shard to coordinator)

- `ShardHeartbeatRequest` gains `repeated FieldFactDelta`, transitions only (a
  locator became typed, or went dirty), each with its epoch.
- The coordinator folds them into the shard's registry entry and writes them
  through to Postgres, so a restarted or second coordinator loads them at boot
  with the shard registry. Heartbeats are not responsible for bootstrap.
- A lost "dirty" is the one dangerous loss, so the shard resends a changed fact
  until `HeartbeatResponse` echoes its epoch (or sends its full fact snapshot
  periodically).
- Old shards send nothing and old coordinators drop the field. Both mean
  "unknown", which is today's behavior, so the facts have no rollout ordering.
- Live shards keep facts in memory only and rebuild them with a background scan on
  open. Sealed shards persist their finished facts so a reopen doesn't rescan.
  Backfill of cold shards is an async scan, exact because sealed data is immutable.
- A facts entry leaves with its shard's registry entry.
- Every flip is logged with `weftsdk.Infof` (locator, shard, the violating row),
  so a strange artifact can be explained afterwards.

## Dispatch (coordinator to shards)

`PlanScoped` takes no facts and returns a base plan plus, where one applies, an
optimistic variant with the requirement that gates it (source and locator). In
`executeQuery`, which already holds the shard descriptors, each shard gets the
optimistic variant if the registry says that shard is typed for the requirement,
else the base plan. `ExecuteQueryRequest` carries per-shard variants.

## What compute merges

| Operation | Mixed shards | Merge |
|---|---|---|
| Numeric filter (`$5 > 500`) | easy | Typed shards pre-filter; compute reruns the original filter over everything, which is idempotent on survivors. |
| Count or sum per key | fine | For base-plan shards compute runs the original stage per shard, producing `key sum` partials that add to the typed shards' partials. Float sums may differ in the last digits. |
| `wc -c`, distinct count | fine | Partial-and-add. |
| Min/max per key | hard | awk compares strnum values numerically and the rest as strings, so a typed partial and a dirty partial have no clean merge. All shards typed gets the lowering; otherwise the whole query takes the base plan. |

## Rollout

1. **Dispatch plumbing.** *(Done 2026-10-30.)* Per-shard variants in the
   request; compute accepts both forms. Compute first: an old compute ignores the new
   field, so this is safe alone. A variant is `ShardDescriptor.sql` + `args`, which
   replace the request's for that shard. Per-shard stages or group specs (the
   sum/count merge, step 3) are not on the wire yet; they land with the planner
   variant that needs them.
2. **The classifier**, the heartbeat deltas, the registry and the Postgres
   write-through. *(Done 2026-10-30.)* `Ingest` is the only writer of row
   values (checked 2026-10-30; the other writes to `logs` are migrations of source
   columns), so that is the only hook, and `TestIngestIsTheOnlyWriterOfRowValues`
   fails if anything else writes `fields` or `payload`. Built:
   - `shard/fieldfacts.go`: the classifier goroutine (queue of 1,024 batches; a batch
     that doesn't fit turns the classifier off for good, never a guess). Typed after
     1,000 numeric rows, dirty on the first non-numeric or missing value. A locator
     first seen after earlier rows is dirty from the start.
   - **No rescan, ever.** Rows are append-only, so the classification results are all
     there is to keep. They live in a one-row table in the shard's own DuckDB file
     (`field_facts_state`), which travels with the volume through a roll. The classifier
     writes it itself, **only when a result changes**: a source or locator starts being
     tracked, or one becomes typed or goes dirty. Classifying another conforming row is
     not a change, so a steady source writes nothing. The next instance restores it at
     open. `Pause` just waits for the classifier to finish before the checkpoint.
   - What is kept is results, not counts: a locator part-way to 1,000 rows starts
     counting again after a roll (slower, safe; a source slow enough to be hurt by that
     does not need typing), and a restored source counts as having
     had rows, so a locator it didn't save stays dirty. A shard with rows and no saved
     state (one from before this) opens with its facts off and stays off for its life;
     it is never scanned, so for the shards that exist at the first deploy that means no
     facts until their window rotates. (An earlier version rescanned the table on open.
     On a ~1 GB host holding several GB it left a rolled shard stuck in LAUNCHING, so it
     was removed.)
   - **Accepted gap:** if a shard is killed (not rolled) between a row committing and the
     classifier seeing it, that row is never classified, and a violation in it could leave
     a locator typed that shouldn't be. This is the plan's tolerated window made
     permanent for that shard (Paul accepted it, 2026-10-30). Closing it needs a "shut
     down cleanly" marker restored only when set (costing the facts after every crash).
     A classifier that stops *knowingly* (its queue overflowed) is not part of the gap: it
     deletes the saved results and blocks any later save, so the next open finds rows and
     nothing saved, and leaves the facts off.
   - **No reset for an existing shard.** A shard with rows and no saved state has facts off
     for its whole life, through any number of rolls: there is no sound way to start
     counting from now, because rows already there might violate (the plan has no
     per-time coverage). It gets facts when its window rotates and a new shard opens.
     Anything that needs facts on today's data has to use a range inside a window that
     opened after the first deploy, and record its baseline there.
   - `ShardHeartbeatRequest.field_facts` and `HeartbeatResponse.field_facts_acked`. Only
     typed facts and retractions of typed ones are sent; absence is unknown.
   - `logdb.Shard.FieldTyped(key)`, `ShardRegistry.FoldFieldFacts`, and migration 110
     (`shard_field_facts`, cascading with the shard record), loaded at boot and on
     refresh. The ack is withheld if the store write fails.
   - Locators are awk fields (`$N`) and top-level payload keys (`.key`). A key is
     numeric only if its JSON value is a number: a string that reads as a number,
     null, a bool, an array, an object, a missing key or a payload that is not an
     object all count against it, because jq compares them differently. Awk fields
     and keys are capped at 50 *each* per source, not 50 together: a JSON row's line
     also splits into awk fields, which would otherwise use up the slots its keys
     need. Classifying a stats-shaped row costs about 2.5 us on the classifier
     goroutine (`BenchmarkClassifyJSONRow`).
   - A fact is just typed or not: the class set and the largest absolute value (for
     an exact sum) wait for the sum lowering that needs them. Nested jq paths are
     still out.
   - `awk.LooksNumeric` shipped in warp v0.2.55 (weft's `go.mod` pins it).
3. **Planner variants**, in this order: numeric filters, then sum/count per key.
   Min/max with its all-typed-or-nothing rule last.

   > **Finding (2026-10-30, reading `planner/jq.go` and `planner/awk.go`):** the first
   > item buys less than the plan assumed, and the order should change.
   >
   > - **jq numeric filters are already exact.** `jqPathComparisonToSQL` lowers
   >   `select(.q_max_ms > 10)` as jq evaluates it, branching on the value's JSON type
   >   (numbers sort below strings, a missing key is null). A typed key only lets that SQL
   >   drop the type branches: `CAST(json_extract(payload, '$.q_max_ms') AS DOUBLE) > 10`.
   >   That is cheaper per row on the shard, but it ships no fewer rows and changes no
   >   plan, so none of the benchmark's filter queries will move much.
   > - **awk `$N` comparisons are the real filter win.** Today `$5 > 500` is a prefilter
   >   (`awkPushdown.why = reasonAwkStrnum`): it ORs a numeric test with a *string*
   >   compare and an inf/nan/hex escape, so it ships every row whose text sorts above
   >   "500" ("60", "7"). A typed `$5` is exactly `TRY_CAST(fields[5] AS DOUBLE) > 500`,
   >   which ships only the matches. No weft-owned source has numeric awk columns, so
   >   this needs a different benchmark source.
   > - **The keyed aggregates are where the benchmark moves.** `keyed-max-qmax`,
   >   `keyed-sum-qcount`, `keyed-sum-qrows` and `sortn-max-qmax` ship every row to
   >   compute today. Lowered to a per-shard GROUP BY they ship one row per key per
   >   shard. That is the keyed-aggregate IR node `query-limits.md` already names as
   >   next, and the sum/count merge in the table above. It needs per-shard stages on
   >   the wire, so **compute must be deployed with it first** (see Deploy gates).
   >
   > So: do the keyed sum/count (and max/min) lowering next, designed together with the
   > keyed-aggregate node, and treat the awk numeric prefilter and the jq SQL
   > simplification as small follow-ons that reuse the same variant plumbing.

Each step also needs a differential test against the real stages on data that
includes the violating rows, because the window is tolerated but the steady state
is not.

## Numeric filters: built (2026-10-30, reworked 2026-10-31)

A typed field is a **finite decimal** in every row: digits, a sign, a point and an
exponent, and nothing else (`shard.finiteDecimal`). awk also reads `inf`, `nan` and hex
floats as numbers, but DuckDB's cast reads them differently or not at all, so a field
holding one is dirty. That makes `TRY_CAST(fields[N] AS DOUBLE) op n` exactly awk's
comparison on a typed shard, with no string compare, no missing-field case and no escape
clause.

`QueryPlan.Typed` (`planner/typed.go`) carries two things for a query over one stream
(one namespace, service and kind, no follow; env from the path or empty):

- **The shard SQL** for a shard whose facts cover the locators, derived from the base plan
  by swapping each typed leaf's predicate text (only when that text occurs exactly once).
  The stage still runs in compute on what comes back, so the swap only has to keep every
  row the stage would. The coordinator (`applyTypedVariant`) hands it to the shards that
  qualify (`ShardDescriptor.sql`/`args`), so a query over old and new windows is a mix. An
  old compute ignores the descriptor's sql and runs the base, which is correct.
- **`Exact`**, the whole plan compiled again with the facts assumed (`compileFor`, which
  threads `assumeTyped` into the awk lowering). A numeric comparison is then exact, so
  `awk '$3 > 100 {print $1, $3}'` lowers entirely: a `WHERE` on the cast and a `concat_ws`
  of the fields, with no compute stage and no regexp. Its stages only fit shards whose
  rows come back already filtered, so the coordinator (`exactPlan`) runs it only when
  **every** shard in the query has the facts; one shard without them keeps the mix above.
- **jq `select(.key op number)`** (top-level key): the base was already exact; the typed
  form is the cast alone.

Measured on 200k request lines, one shard (`BenchmarkAwkFilterPrint`), whole query through
compute: `$3 > 1000` 823 ms to 11 ms, `$3 > 100` 1,062 ms to 93 ms (an earlier run of this
benchmark counted dispatch's pre-run of each request's SQL, a whole extra query, and
overstated the base). The exact plan's shard SQL costs the same as the handwritten
`fields` form (54 ms at `> 100`).

- **Tests:** planner unit tests; differential tests that run the base, mixed (the second
  shard holds `inf`, `nan` and hex floats) and exact plans against the reference stage;
  the classifier test for the finite-decimal rule; the coordinator test that the exact
  plan needs every shard; and the shard-to-coordinator-to-Postgres rig checking that the
  key a real `Plan` asks for is the key the classifier reported.
- **Not covered:** crons and dashboards (their persisted `LoweredPlan` carries the base
  plan only); a negative literal (`$2 > -1`, `.q > -1`) is not lowered by either plan; nested
  jq paths; a value awk reads as out of range (`1e999`) is dirty, so it never types a
  locator.
- **Field projection** (`planner/fieldproj.go`) feeds awk stages that remain in compute
  and read a few fields; see `field-row-spec.md`. The shard sends those fields' values
  and compute rebuilds the record, so there is no text record built in SQL.

## Keyed sum/min/max: built (2026-10-31)

`awk '{ s[KEY] += $N } ...'`, `if ($N > m[KEY]) m[KEY] = $N` and the `<` form lower to
a shard `GROUP BY` in `QueryPlan.Typed.Exact`, and so does nothing else: the base plan keeps the
stage, as for the numeric filters. What changed from the design below, and why:

- **The IR node.** `tallyNode` is now `aggregateNode` (`planner/ir.go`): an operation (count, sum,
  max, min), the key and the field number, and whether the plan assumes the facts it needs.
  `awkKeyed` (`planner/keyed.go`) recognizes the shape from the awk tree. The value is
  structure all the way down: the Query builder renders the value columns and what the group
  reports of them (`valueColumns`, `groupColumns` in `planner/sql.go`), so no pass or frontend
  builds SQL text for it. The key is still the SQL `awkPrintArgToSQL` gave the count form.
- **All shards or none, for the sum too.** The design had untyped shards run the original stage
  per shard and add their partial sums to the typed shards'. That needs per-shard stages on the
  wire; the exact plan already has the all-shards rule (`exactPlan`), so a query over a window
  with any shard lacking the facts keeps the base plan. Per-shard stages are not built.
- **A sum needs integers and a bound, not just a number.** Adding floats in a different order
  gives different last digits, and `%.6g` shows them whenever a sum lands near a rounding boundary
  (a sum of cents at 123456.75 can print either way). So the sum is exact only for whole numbers,
  and two things establish that. The shard classifier has a second fact per awk field,
  `$N#int` (`shard.finiteInteger`: a decimal integer of at most 15 digits), so a field with a
  decimal point never lowers. And the query proves its own bound: the shard returns the sum of
  absolute values (`agg_abs`) and compute fails the query if the total passes 2^52, since only
  then is every partial sum, in any order, a whole number a float holds. Float sums keep running in
  compute, as before.
- **A fact is a guess, so the aggregate checks it.** Each group also returns the count of values
  the SQL cannot vouch for (`agg_bad`: not a whole number for a sum, not a number for a max or
  min). Compute prints nothing until it has seen every group, then fails with
  `compute.InexactAggregateError` (a field of `ExecuteQueryStreamResponse` carries it across gRPC),
  and the coordinator runs the base plan (`inexactFallback`). The user gets awk's answer, never
  the error. It can only retry before a line has gone out, and compute prints nothing before the
  check, short of a stage after the aggregate that prints without reading input.
- **max and min follow warp's awk.** The comparison is against an element nothing has set,
  which is 0, and warp does not create an element by reading it, so a key whose values never
  beat 0 is absent from the output. (POSIX awk prints such a key with an empty value; the
  base plan here is warp, so the exact plan matches it.) The value printed is the field's own
  text, so `5.0` and `5` are different answers; the first row in `(ts, event_id, line_no)` order
  holding the best number wins, within a shard (`first(... ORDER BY ...)`) and across shards (the
  winning row's `ts`, `agg_best_ns`; shards in one window never tie on it).
- **`sort -k` around the aggregate.** A sort *before* a count or a sum is dropped, since
  neither depends on row order (a sum is proven exact, so addition order is moot), and the aggregate
  lowers; before a max or min it stays, because the first row of a tie is the one whose spelling
  prints. A sort *after* it, `sort -k2 -n` or `-rn`, `-k2,2nr` and the other plain forms, plus a
  `head -n N`, folds into the group as `GroupSpec.Order` and `Limit`, and compute applies it to the
  aggregate's printed lines with warp's own comparison (`compute/value_order.go`: fields split
  on blanks, the number a key leads with, ties by the whole line, reversed only by the global `-r`;
  `-k2,2nr` reverses the key but not the ties, `-r -n -k2` both). It works on the printed lines, so
  a key with a blank in it, or an empty one, shifts the fields exactly as it does for the real
  stage. Warp does not accept `-nk2`, so neither does the planner. Not folded: `sort -u`, `-t`,
  file operands, a string sort, a key other than field 2, several keys, `tail`, `head -n 0`.
  This applies to the count too, which lowers without facts. A new `GroupOrder` value carries it, so
  a compute from before this fails the request instead of printing the lines unsorted.
- **Not covered:** crons and dashboards (their `LoweredPlan` carries the base plan, as for the
  numeric filters; a count with its order is in it, since a count needs no facts); a key or value that is not a plain `$N` field (`$NF`, `$(i+1)`, an expression);
  more than 50 awk fields per source, past which a field has no fact; rows ingested before
  2026-10-14, whose `fields` is the older split, as for every lowering over `$N`; a stage after the
  aggregate that prints without reading input.
- **Deploy order:** compute first (it must know the three new `GroupRender` values, and an old
  one fails the request, as it should), then the shards (they report `$N#int`; an old shard never
  does, so the coordinator never picks the plan for it), then the coordinator. The coordinator
  alone sends the exact plan, and only when every shard has the facts, so a half-rolled fleet
  keeps the base plan.
- **Tests:** `planner/keyed_test.go` (recognized shapes, requirements, pinned SQL, `explain`);
  differential tests in `planner/difftest/keyed_test.go` against the real awk over 1, 2, 3 and 5
  shards, with tie spellings, keys that never beat zero, and the inexact cases; `compute`
  aggregator and wire tests; `coordinator/keyed_fallback_test.go` (the exact plan runs, the base plan
  runs when a shard lacks the facts, and the fallback when the facts were wrong); the
  classifier tests for `$N#int`. Each safeguard was checked by removing it and watching a test fail.

## Keyed sum/min/max: design (2026-10-30, revised)

The keyed aggregate is awk over a row's own fields:

    awk '{ s[$1] += $4 } END { for (k in s) print k, s[k] }'

`fuseAggregate` already turns the count form (`{ c[KEY]++ }`, `tallyNode`) into a shard
`GROUP BY`. A sum, max or min is the same node with an operation and a value. Over a
row, `$N` is `fields[N]`, the split ingest stored, so a typed `$4` makes the shard's
side plain SQL: `SELECT fields[1], SUM(TRY_CAST(fields[4] AS DOUBLE)) ... GROUP BY 1`.
That is the design. The facts this release reports (`$N` per source) are exactly what it
asks for.

**Rejected: aggregating over a projection.** A first sketch also lowered
`jq -r '[.k, .v] | @tsv' | awk '{ s[$1] += $2 } ...'`, which reads `$N` of the
*projected text*. That needs SQL to re-split a computed string
(`list_filter(string_split_regex(text, ...), x -> x <> '')[K]`), and it has a real
hazard: `@tsv` does not escape spaces, so a string key with a space shifts `$2`, and a
fact about `.v` does not prove `$2`. It is a contrived pipeline (a user who has the
keys in JSON would aggregate in jq, not detour through awk) and not SQL worth
generating. `fuseAggregate` keeps skipping a key that reads projected text
(`c.selected != nil`), and no benchmark should depend on it.

What the typed fact buys, per operation (all over `$N` of the row):

- **sum**: `SUM(TRY_CAST(fields[N] AS DOUBLE))` per shard; compute adds the per-shard
  sums per key and prints them as awk does (`formatAwkValue`). Float sums can differ in
  the last digits from awk's one running total, so lower only where the fact also says
  the values are integers within 2^53 (the class set and largest absolute value this
  release left out), or the differential test shows rounding to awk's six printed digits
  agrees.
- **max/min**: no clean merge across shards that are not all typed (see the table), so
  all-typed-or-nothing.
- **numeric filter** (`$N > 500`): the exact `TRY_CAST(fields[N] AS DOUBLE) > 500`
  instead of today's prefilter, which also ORs in a string compare and ships every row
  whose text sorts above "500".
- Untyped shards keep running the original stage per shard in compute, and their
  `key sum` partials add to the typed shards'. That needs **per-shard stages on the
  wire**; an old compute must fail the query rather than ignore them (Deploy gates).

Pieces, in build order: (1) `tallyNode` becomes an aggregate node with an operation
(count, sum, max, min) and a value expression; (2) the planner emits the base plan plus
an optimistic variant and its requirements (source, locators); (3) the wire carries
per-shard stage/group variants and compute merges them; (4) `executeQuery` picks per
shard from the registry; (5) differential tests against the real awk on data with
violating rows.

**Benchmark.** Weft's own sources are JSON, so they have no numeric `$N`. They stay in
`bench/fieldtypes` as controls: their output must not change. The targets need a text
source with numeric columns; see "Benchmark source" in `bench/fieldtypes/README.md`.

## Open questions

1. What a source with hundreds of distinct JSON keys should track: the first 50
   seen, or the 50 a query has asked about.
2. `explain` and the facts: **deferred (Paul, 2026-10-30)**. `explain` stays quiet about
   them until the lowerings that depend on facts exist. See below.
3. Nested jq paths: out of the first version.

### `explain` and the facts (deferred)

`explain` speaks only user terms (no shard or SQL vocabulary) and shows real
counts. With facts, the same pipeline can be optimized for part of its range and
not the rest, depending on data state, not just the pipeline's text. If it
reports that, it has to do so by time range ("`$5 > 500` is optimized for the last
6 hours; older data is read row by row"), never by shard, and it can use a stale
copy of the facts since advice tolerates them. It also makes `explain`'s output
depend on data state, so the golden tests need an injected fake facts source.

## Deploy gates

Steps 1 and 2 are safe in any order and alone: old shards send no facts, an old
coordinator drops them, and an old compute ignores a descriptor's `sql`. Nothing
can use a fact until the migration, coordinator, shard AMI and compute are all out,
so step 3 cannot be observed in prod before that deploy.

Step 3 splits at the gate. A numeric-filter variant keeps the base plan in the
request and the optimistic SQL in the descriptor, so an old compute that ignores it
still answers correctly: no ordering. Sum/count per key sends per-shard stages, which
an old compute would ignore and answer wrongly: **compute with that wire must be
deployed before the coordinator that sends it.**

