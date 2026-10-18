# Planner IR: compile pipelines to SQL + compute, never regenerate source

## Status

🟡 Phase 1 landed 2026-10-13: compute stages are the user's text, jq and
row-identity semantics are defined (see "jq" and "Row identity"), and the
differential harness (`planner/difftest`) passes on 307 pipelines. Needs the
matching warp commit (`StreamingOptions`, awk hook scope).

Phase 2 landed 2026-10-13 (`planner/ir.go`, `frontend.go`, `passes.go`):
frontends, passes, IR-rendering `Explain`, `$N` lowering (ingest splits
`fields` the way awk does), and a harness corpus of ~640 pipelines (docs + a
generator). Needs the matching warp commit (`Value.String`).

Phase 3 landed 2026-10-13: `cut` and `jq -r` project in the shard, and
`wc -l`, `sort -u` and `sort | uniq [-c]` aggregate there
(`QueryPlan.Aggregate`); the harness covers each. Perf hints (rule 1) are
not built.

## Problem

The planner pushes pipeline stages into shard SQL with one recognizer per
command (`tryPushdown` in `planner/planner.go`, dispatching to `awk.go`,
`jq.go`, `grep.go`, `sed.go`, and `project.go` for projections). Each
recognizer does two jobs:

1. translate the part it understands into a SQL fragment, and
2. **synthesize source code** for the remainder, which becomes a compute stage.

Job 1 is mostly sound. Job 2 is where the bugs come from, because it means
hand-writing an unparser for every language the shell speaks, and every gap in
that unparser corrupts a user's program:

| Pipeline | What compute receives | Effect |
|---|---|---|
| `awk '$1 == "x" { print $1 $2 }'` | `awk '{ print ? }'` | `formatAwkExpr` has no case for concatenation, calls, arrays, `if`, loops, `getline`, regex match. Each becomes `?`, which is a syntax error at runtime. |
| `awk '$1=="a" { print "A" } $1=="b" { print "B" }'` | SQL `$1 = 'a'`, compute `awk '{ print "A" }'` | Only the first rule's pattern and action survive. Rows for every other rule are **silently dropped**. |
| `… \| sort \| awk '{ print "it'\''s" }'` | `awk '{ print "it's" }'` | `formatToolStage` wraps the program in bare single quotes. An embedded quote re-parses as a different program. |

Earlier bugs from the same root cause: `tail | jq | sort` dropped the
`ORDER BY`/`LIMIT` (string-level re-planning of compute stages). Other places
the planner re-reads its own generated strings:

- `parseComputeString` re-parses a synthesized compute string to look for a
  projection.
- `classifyShardMode` scans compute strings for `sort`.
- Meta-variable projection is decided by regex-sniffing compute strings.
- jq remainders are re-rendered from the AST (`jqRenderPipe`).

Every one of these is a string round trip that has to stay exactly in sync
with how the string was produced.

## Principles

1. **Never regenerate source.** Compute only ever receives text the user typed:
   a whole stage, sliced from the original pipeline. If a stage needs to keep
   running after something was pushed down, it runs *unchanged*.
2. **Pushdown is either a prefilter or exact, and the difference is explicit.**
   - A *prefilter* is a SQL predicate `P` such that, for every input,
     `stage(rows) == stage(rows WHERE P)`. It's always safe to add, as long as
     the stage still runs. Re-checking awk's own pattern on rows the shard
     already filtered is harmless, and it makes awk the final authority if the
     SQL is ever looser than awk's semantics.
   - *Exact* lowering proves that SQL computes the stage's whole output, so the
     stage can be dropped from compute. It's an optimization earned per
     construct, never the default.
3. **Fallback is structural.** An `Opaque(stage)` node is a first-class part of
   the plan, not an error path. The worst possible plan is
   `Scan → Opaque(every stage)`, which is always correct. A construct the
   compiler doesn't understand costs speed, never correctness.
4. **One generic correctness gate.** Every plan has a trivially correct
   reference: plain scan, every stage in compute. A differential harness
   compares the two on generated data. Rewrite rules are trusted because the
   harness passes, not because each recognizer has hand-written parity tests.
5. **warp stays weft-agnostic.** warp supplies parsers and syntax trees
   (`pkg/awk`, the jq parser, `pkg/bash`). The IR, the lowering and the passes
   live in weft's `planner/`. No query or pushdown vocabulary goes into warp.

## Design

### Row type

The IR operates on shard rows, with the columns the shard exposes today: `ts`,
`raw`, `fields` (the whitespace split), `payload` (JSON), `level`, `namespace`,
`env`, `service`, `kind`, `trace_id`, `span_name`, `duration_ns`, plus the
row-level-security `customer_id`, which the planner injects and no stage can
reference. Each node knows which columns it *requires* from below. That
replaces regex-sniffing for meta-variables: a node that reads `HOST` or
`$host` requires that column, and projection follows.

### Nodes

```
Scan(source)                      -- path → namespace/env/service/kind, time bounds, customer
Filter(pred, kind=prefilter|exact)
Project(exprs)                    -- exact only: replaces raw with an expression (cut, awk print $n, jq .path)
Limit(n, order=ts asc|desc)       -- head/tail; also an awk NR window
Sort(key, dir)                    -- recognised only, for shard ORDER BY (today's Mode B)
Aggregate(keys, fn)               -- exact only: uniq -c, sort|uniq -c|sort -rn|head, wc -l
Opaque(stage)                     -- run this stage, verbatim, in compute
```

Every node carries a `span` pointing at the original stage text. A stage that
lowers to `Filter(prefilter) → Opaque` keeps one span on both nodes.

### Frontends (stage → IR)

Each command lowers its own syntax tree into a small IR fragment. Unknown
flags, unknown constructs or failed preconditions produce `Opaque(stage)`.

- **grep**: `Filter(regexp_matches(raw, p), exact)`. `-i` and `-v` handled.
  Any other flag → `Opaque`.
- **awk**: let *main rules* be the rules other than `BEGIN`/`END`.
  - *Pure filter* (one rule, default print, no `END`): `Filter(exact)`.
  - *Exactly one main rule with a lowerable pattern*:
    `Filter(pattern, prefilter) → Opaque(whole program)`.
  - *Two or more main rules*: a prefilter is the OR of every rule's pattern,
    and only if every pattern lowers. Otherwise `Opaque`.
  - Preconditions that force `Opaque`:
    - `BEGIN` (it can set `FS`/`OFS`, changing how fields split)
    - any `NR`/`FNR` reference outside a recognised NR window, since a
      prefilter changes what `NR` counts
    - `getline`
    - a pattern whose string-vs-number semantics can't be matched exactly in
      SQL (see "awk semantics")
  - *Projection*: when the whole program is `{ print $i, $j }`,
    `Project(exact)`. This is today's `awkProgramToSQL`, moved behind the
    exactness tag.
- **jq**: see "jq" for what a jq stage reads and emits.
  - A select chain is a row filter: `Filter(exact)` when every select
    lowers, else the lowered ones prefilter and the chain is a row stage.
  - Selects followed by a tail SQL computes fully (`.a.b`, `{k: .x}`,
    `[.a, .b]`, meta-variables) → `Filter(exact) → Project(exact)`.
  - Anything else: selects prefilter, then `Opaque(whole stage)` reading the
    payload. Never a re-rendered tail.
- **sed**: an address-only program (`/re/p` with `-n`, `/re/d`) →
  `Filter(exact)`. A substitution with an address →
  `Filter(prefilter) → Opaque`.
- **head/tail**: `Limit`. **sort/uniq/wc/cut**: recognised for `Sort`,
  `Aggregate` and `Project`, else `Opaque`.

### Passes

Passes are rewrites on the IR, each with an explicit precondition:

1. **Filter pushdown.**
   - A `Filter` moves below an `Opaque` only when it came from that
     `Opaque`'s own stage (its prefilter).
   - A `Filter` moves below another `Filter` or a `Project` that doesn't
     change the columns it reads.
   - **A `Filter` never moves below a `Limit`.** This is today's
     `limitFrozen` rule ("a line count ends predicate pushdown",
     `docs/completed/cross-shard-post-limit-filter.md`), now a single
     precondition instead of loop state.
2. **Projection pushdown.** The first `Project(exact)` above the SQL boundary
   becomes the `SELECT` expression (today's `extractRawExpr`). Columns a later
   node requires are added to the select.
3. **Sort/limit fusion.** `Sort` right above the boundary, optionally followed
   by `Limit`, becomes the shard `ORDER BY`/`LIMIT`, under today's
   `classifyShardMode` rules. Those rules are re-expressed on nodes, so no
   compute strings are scanned.
4. **Aggregate recognition (Phase 3).** `Sort(raw) → Aggregate(uniq -c)` and
   friends become `GROUP BY`, and the shard returns counts instead of rows.
   This is where the big data-transfer wins are. Built as `fuseAggregate`
   (see Phase 3).

### Backends

- **SQL**: everything from `Scan` up to the first node that can't be SQL
  renders through the existing `sqlBuilder` (placeholders, tail wrap,
  `DirectShardSQL`), so the shard protocol doesn't change.
- **Compute**: each remaining node emits its stage's original text by span.
  A node that lowered exactly but ended up above the boundary emits its span
  too. Consecutive nodes sharing a span emit the stage once.
- **Follow mode**: `LiveStages` keeps its current definition (the user's
  post-source stages, verbatim) and comes straight from the spans.
- **Persisted plans** (`LoweredPlan`): the shape is unchanged (SQL strings,
  arg sources, compute strings). Bump `PlannerVersion`; stored cron plans
  recompile on their next fire.

### Explain

`planner.Explain` (and `cmd/explain`, which wraps it) shows the IR, not just
what comes out of the backends. Today it prints the shard SQL and the compute
strings, so it can say *what* ran where but never *why*. The why is the part
people need: "why didn't my awk filter push down?"

That means the IR has to keep its reasons:

- **Frontends record why a stage went `Opaque`**, or why it lowered only as a
  prefilter: `BEGIN present`, `NR outside a window`, `regex uses a
  back-reference`, `unknown flag -A`, `action is not a plain print $n`. These
  are typed reason values, not free text, so tests can assert on them.
- **Passes record what they did and what they declined**: `moved Filter below
  Opaque (own prefilter)`, `Filter held above Limit (head 10)`, `no Sort next
  to the boundary, no ORDER BY fusion`.

Sketch of the output:

```
Pipeline: cat app.log | awk '$3 > 10 { print $1 $2 }' | head 5

Plan:
  Scan     app.log  namespace=(all) service=app kind=log
  Filter   prefilter  CAST(fields[3] AS DOUBLE) > 10         [1: awk]
  ─── shard │ compute ───
  Opaque   awk '$3 > 10 { print $1 $2 }'                     [1: awk]
             not exact: action prints a concatenation, not a plain $n
  Limit    5                                                 [2: head]
             not pushed: above an Opaque

Passes:
  filter-pushdown    moved Filter[1] below Opaque[1] (own prefilter)
  sort-limit-fusion  skipped: no Sort next to the boundary

Shard SQL:
  …                  (as today, with bound args inline)

Compute stages:
  1. awk '$3 > 10 { print $1 $2 }'                           [stage 1, verbatim]
  2. head 5                                                  [stage 2, verbatim]
```

- Each node shows its span as `[stage index: command]`, so the reader can
  line every SQL fragment and every compute stage up with what they typed.
  Every compute line says `verbatim`, which makes principle 1 visible: it's
  only ever true.
- The `shard │ compute` line is the SQL boundary from the backends section.
- Explain is a test surface too:
  - planner tests can golden-test `Explain` output instead of comparing raw
    SQL strings;
  - when the differential harness finds a mismatch, it prints `Explain` for
    the plan under test next to the reference plan, so the failure shows which
    rewrite fired.
- `Explain` keeps working on an Opaque-only plan. A pipeline the compiler
  understands nothing about shows `Scan → Opaque(every stage)` with a reason
  on each `Opaque`. That's the most useful output of all, because it lists
  exactly what to teach the frontends next.

### awk semantics the lowering must respect

- **Positional fields are `fields[N]`.** Decided 2026-10-13: the shard
  splits each row's `raw` into `fields` the way warp's awk splits `$0` with
  the default `FS` (runs of space, tab, newline, carriage return;
  `shard.awkFields`, pinned against warp by a test), whatever the source's
  schema. The schema keeps its own split for its positional extractors, in
  memory. Senders no longer send a split (`LogRow` tag 8 is reserved). So
  `$N` with a constant `N` lowers to `list_extract(fields, N)`, which is
  NULL past the last field, where awk reads an unset value: `""` as a
  string, 0 as a number. `$NF`, `$i` and other computed indexes stay
  `Opaque`, as does any awk with `-F` or a `BEGIN`. `cut -f` still doesn't
  lower: cut splits on each tab, not on runs of blanks.
  - Rows ingested before the change hold the old schema-dependent split.
    We don't reconcile them: a `$N` query that reaches them may disagree
    with awk until retention ages them out. No schema history.
- **String vs number comparison.** awk compares as numbers when both sides
  look numeric (warp: `strconv.ParseFloat`), a string constant included, so
  `$3 == "500"` matches `500.0`; otherwise as strings. Against a string
  that doesn't look numeric, a comparison is a string comparison, exact.
  Against a number (or a numeric-looking string) SQL can't tell which
  reading applies, so it lowers to a prefilter that holds under either:
  `TRY_CAST(f AS DOUBLE) op n OR f op 'n' OR` a field DuckDB doesn't parse
  as a number but Go does (`inf`, `nan`, hex), plus `f IS NULL` when an
  unset 0 satisfies it. The same rule covers `$0` and meta-variables
  (`LEVEL == "500"` was wrongly exact before).
- **`$0` modification and `NF`** in the pattern → `Opaque`.
- **Regex dialects.** awk ERE vs DuckDB RE2: the constructs that differ
  (back-references, POSIX classes) → `Opaque`.
- **Uninitialised variables** in patterns (`x == ""`): they're always true on
  the first record. Only meta-variables and literals lower.
- **Meta-variables** are strings, and a NULL column is an unset variable,
  which is `""`. A comparison lowers with `COALESCE(col, '')` exactly when
  `""` would satisfy it. `TS` doesn't lower (awk sees its text, SQL a
  TIMESTAMP).

### jq

Decided 2026-10-13. A jq stage that reads rows (the first stage in compute,
or the first after row stages, see "Row identity") reads each row's
**payload**: the whole line for a JSON log, the JSON part of a line with
text around it. Meta-variables are spliced in as `_`-prefixed keys, but
only when a stage names one, so `keys` and `.` don't show them.

- A program made only of `select(...)`s is a **row filter**: it keeps or
  drops the row, which stays its raw line. `| .` after the selects prints
  the payload instead.
- Everything else prints jq's output, **compact** (one value per line;
  warp's `JqCompact`). A missing field prints `null`.
- A stage after one that changed the text reads that text.

A path step (`.a`, `[0]`) before or between selects moves the **subject**,
the value the program holds, and a `select` tests the subject:
`.a | select(.b > 1)` compares `$.a.b` and emits `.a`. The head of the
program, its selects and path steps, lowers as a filter on the longer
paths plus a projection of the final subject. Each step also needs its
parent to be the type jq would accept (an object before a field, an array
before an index; null and missing pass), which the filter checks because
jq errors there and the row drops. A tail that isn't a path (`{x: .y}`,
`length`) reads the payload from its root, so after a path step it runs
whole in compute, behind the head's selects as a prefilter.

Comparisons lower by **jq's total order**, null < false < true < numbers <
strings < arrays < objects, with a missing field as null. The SQL picks the
branch by `json_type`: `.n == 500` doesn't match the string `"500"`, and
`.n < 100` matches a row without `.n`. Only `field op literal` lowers.

### Row identity

Decided 2026-10-13. A stage that passes rows through unchanged keeps the
row, payload and meta-variables included, so the stage after it still reads
a row:

- `grep` with only `-i`, `-v`, `-E` and one pattern,
- an awk that's one pattern with the default print, no `BEGIN`/`END`, no
  `NR`/`FNR`, no `getline`, and no side effects in the pattern (assignments,
  `++`, calls other than pure builtins),
- `sed -n '/re/p'`,
- a jq select chain,
- `head`/`tail -n N`.

This holds whether or not the stage pushed down. When such stages land in
compute ahead of a stage that reads row metadata, they run per row in the
engine (`QueryPlan.RowStages`: filters through warp on the one row, head and
tail counted), and the next stage gets the rows whole. Otherwise they stay a
plain text pipe. The awk meta-variable hook only feeds an awk reading the
pipeline's stdin.

## Differential harness

`planner/difftest` (test-only). Built in Phase 1:

- Two in-memory `shard.DuckDBShard`s, loaded round-robin with 400 generated
  rows:
  - JSON lines, text with a JSON tail, kevcons envelopes, l2met, plain text
  - status as a number, a numeric string, or missing; quotes, unicode,
    nested objects, arrays, nulls
  - unique timestamps, some sub-second, spanning midnight
- Runs a pipeline both ways through the real `compute.Engine`
  (`ExecuteQuery`, as the coordinator dispatches):
  - **plan under test**: `Plan()` as compiled;
  - **reference**: a scan of the source (its head/tail kept), every leading
    row stage per row, everything else in compute.
- The outputs must match byte for byte. They're compared as multisets when
  the plan has no merge key, since rows then arrive in shard order.
  Identical errors on both sides count as agreement.
- `TestRowStagesAreRowwise` checks the row-stage classifier: a text filter
  it accepts keeps, row by row, exactly what it keeps over the stream.
- The corpus today:
  - every finite pipeline in the planner's tests, plus `extraCorpus`;
  - every one-line pipeline in `docs/shell` and `docs/manual`, its source
    pointed at a generated source (`docsCorpus`);
  - 300 seeded pipelines of one to three stages, composed from constructs
    the frontends lower and ones they don't (`generatedCorpus`).

  Still to add: pipelines exported from the dogfood account's crons and
  dashboards, plus shell history (kept outside git, per the
  no-account-config rule).

The harness runs in CI. A new rewrite rule ships with corpus entries that
exercise it.

## Phases

1. **Contract change + harness**. ✅ Landed 2026-10-13.
   - Each recognizer returns a predicate that's exact (the stage drops) or a
     prefilter (the stage runs verbatim in compute). An AND keeps the side
     that lowers as a prefilter; multi-rule awk lowers as an OR prefilter.
   - Deleted `formatAwkAction`/`formatAwkStmt`/`formatAwkExpr`, the jq tail
     re-render (`formatJqExpr`, `jqRenderPipe`), `parseComputeString`,
     `formatToolStage`/`joinArgs`. A compute stage is its own `SimpleCommand`
     through warp's `bash.Unparse`.
   - `NR`/`FNR` outside a window, `$N` (N>0), `cut -f` → no pushdown.
   - jq: payload input, select chain as row filter, compact output,
     jq-ordered comparisons (see "jq").
   - Row identity: `QueryPlan.RowStages` and `StagesReadPayload`, also on
     the live path (`LiveRowStages`) and in `LoweredPlan`;
     `ExecuteQueryRequest.row_stages`/`stages_read_payload`;
     `PlannerVersion` 4.
   - `explain` lists row stages.
   - Harness in place (see above).
   - warp: `StreamingOptions{JqCompact}`; the awk per-record hook applies
     only to an awk reading the interpreter's stdin.
   - Bugs the harness found and Phase 1 fixed: jq `"500" == 500` and
     missing-field comparisons; meta-variable keys spliced into the text of
     every stage, not just jq's; `$ts`/`TS` never set in compute; adjacent
     awk stages deadlocking on the meta-variable channel.
2. **IR with `Scan`/`Filter`/`Limit`/`Opaque`**. ✅ Landed 2026-10-13.
   - ✅ grep, awk, jq, sed, head/tail and sort lower through frontends
     (`frontend.go`) into `filterNode`/`limitNode`/`projectNode`/
     `sortNode`/`opaqueNode`, each with its span and a typed `reason`. The
     `Project` and `Sort` nodes cover what the planner already lowered
     (awk `print` of meta-variables, jq paths; Mode B); Phase 3 adds new
     constructs.
   - ✅ Passes (`passes.go`): filter pushdown replaces the `tryPushdown`
     loop and `limitFrozen`; projection pushdown and sort/limit fusion
     replace `extractRawExpr` at the boundary and `classifyShardMode`.
     Meta-variable projection comes from each stage's syntax tree, not a
     regex over its text. Every pipeline in the old corpus compiled to the
     same `QueryPlan` before and after.
   - ✅ `Explain` renders the IR, with spans, opaque/prefilter reasons and
     the pass log.
   - ✅ Exit criteria: no query-planning code reads a compute string
     (`LintTruncationRisk` still scans a saved plan's stages, which are now
     always the user's text), and the harness is green on the full corpus.
   - Bugs the wider corpus found, fixed with `PlannerVersion` 5:
     - folding a projection into the SELECT promoted the next stage to
       first, so an awk after it got the row's `LEVEL` instead of the
       projection's text. A projection now stays in compute when the stage
       after it names a meta-variable;
     - compute spliced `_ts` (and any projected meta-variables) into every
       jq that read rows, so `jq -c .` and `keys` showed them. Compute now
       splices only for a stage that names one, by
       `planner.StageReadsMetaVars`;
     - `MergeMetaVars` panicked on a `null` line, which a jq projection the
       shard computes prints for a missing field.
   - ✅ `$N` lowers (`PlannerVersion` 6), by changing what ingest writes
     rather than by schema facts on `Scan`: see "awk semantics". Bugs the
     harness found on the way: warp printed a numeric meta-variable
     (`500.0`) as `500` (`Value.String`, fixed in warp), and numeric-looking
     string constants were lowered as exact string comparisons.
3. **Exact lowering**. ✅ Landed 2026-10-13 (`PlannerVersion` 7).
   - ✅ `Project`: `cut [-d D] -f LIST` (`string_split(raw, D)`, fields in
     list order, `N`, `N-M`, `N-`), and `jq -r` on whatever `jq` already
     projected (a string prints unescaped, anything else as compact JSON).
   - ✅ `Aggregate`: frontends lower `uniq`/`uniq -c` to `uniqNode` and
     `wc -l` to `countNode`, and `sort` records `-u`. The `aggregate` pass
     (`fuseAggregate`) turns the first compute stages into the shard's
     `GROUP BY` when they're `wc -l`, `sort -u` or `sort [-r] | uniq [-c]`:
     each shard returns its distinct lines with a `count` column, merged
     on `raw`; compute (`compute/aggregate.go`) sums the counts and feeds
     the next stage what the aggregated stages would have printed
     (`ExecuteQueryRequest.aggregate`). The grouped lines are the input
     split on newlines, as compute feeds a stage. Declined after a pushed
     line count, after row stages, in follow mode, for trace sources, and
     when the next stage names a meta-variable; the pass log says which.
     The engine refuses an aggregate name it doesn't know, so a compute
     older than its coordinator fails the query instead of printing the
     shards' grouped rows.
   - Harness: corpus entries for each aggregate and projection, a check
     that the corpus aggregates every kind in the shard, and
     `TestDifferentialLineSplits` over rows of several lines.
   - Found on the way: warp's line readers drop a trailing `\r` per reader,
     so CR-terminated lines aggregate differently in the shard
     (`docs/known-issues.md`).
   - Perf-hints rule 1 (pipeline reordering) is a pass, not a hint: the
     planner can prove the reorder exact, so it does it. Filter hoisting
     (`hoistFilters`, `PlannerVersion` 8, 2026-10-14) moves grep and
     `sed -n` filters above plain sorts, or the SELECT's projection, into
     the shard, filtering each line (`Query.Lines`). Its multi-line probes
     found compute's awk meta-variable queue deadlocking on multi-line
     rows (fixed) and cut/awk projections running per row
     (`docs/known-issues.md`). Hints are left for rewrites the planner can't
     prove (constructs that don't lower, narrowing the source).

## Open questions

- **Cost of re-running prefiltered stages, and of per-row row stages.**
  Row stages start an interpreter per row, so `grep -E x | jq ...` over a
  full scan is slow. Measure on the dogfood account's heaviest crons. If it
  bites, lower more filters (`grep -E` is RE2-compatible for most patterns)
  or reuse one interpreter per stage.
- **warp's jq rejects `.end`** (a keyword, valid as a field in real jq), and
  its grep has no `-w`. The harness treats both sides failing alike as
  agreement; both belong in warp.
