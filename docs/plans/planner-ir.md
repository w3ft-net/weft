# Planner IR: compile pipelines to SQL + compute, never regenerate source

## Status

🟡 Designed, not started. Three bugs below are reproduced by failing tests in
`planner/awk_compute_test.go` (uncommitted; red until Phase 1 lands).

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
`._host` requires that column, and projection follows.

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
- **jq**: `select(cond)` prefix → `Filter`. Then `.path` → `Project(exact)`.
  The remainder is `Opaque(whole stage)`, not a re-rendered tail. This
  replaces `jqRenderPipe`. Re-running the `select` in compute is the cost of
  never regenerating source.
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
   This is where the big data-transfer wins are.

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

### awk semantics the lowering must respect

- **String vs number comparison.** `$3 > 10` is numeric only when `$3` looks
  like a number ("strnum"); otherwise it's a string comparison. The current
  `CAST(... AS DOUBLE)` is fine as a **prefilter** only if non-numeric fields
  can't satisfy the awk comparison. As exact lowering it needs a `CASE` on
  numeric-looking fields, or it stays a prefilter.
- **`$0` modification and `NF`** in the pattern → `Opaque`.
- **Regex dialects.** awk ERE vs DuckDB RE2: the constructs that differ
  (back-references, POSIX classes) → `Opaque`.
- **Uninitialised variables** in patterns (`x == ""`): they're always true on
  the first record. Only meta-variables and literals lower.

## Differential harness

`planner/difftest` (test-only):

- An in-memory DuckDB with the shard `logs` schema (`shard/shard.go`), loaded
  with generated rows:
  - mixed structured and plain text
  - numeric-looking and non-numeric fields
  - quotes, empty fields, unicode
  - timestamps spanning midnight
- Runs a pipeline both ways:
  - **plan under test**: `Plan()`, then its `ShardSQL` on DuckDB, then a
    `compute.Engine` job with its `ComputeStages`;
  - **reference**: `SELECT raw, ts` with only the scan predicates, then a
    `compute.Engine` job with the original stages.
- The outputs must match byte for byte (or as multisets where the pipeline
  has no ordering).
- The corpus:
  - every pipeline in `docs/shell` and `docs/manual`
  - every planner test's pipeline
  - pipelines exported from the dogfood account's crons and dashboards,
    plus shell history (kept outside git, per the no-account-config rule)
  - a generator that composes stages from the supported constructs

The harness runs in CI. A new rewrite rule ships with corpus entries that
exercise it.

## Phases

1. **Contract change + harness** (~2d). Turns the three repro tests green.
   - Every recognizer returns only a prefilter, and the stage runs verbatim in
     compute.
   - Delete `formatAwkAction`/`formatAwkStmt`/`formatAwkExpr`, the jq tail
     re-render, and `parseComputeString`.
   - Compute stages are sliced from the original pipeline, with no
     `formatToolStage` re-quoting.
   - Multi-rule awk lowers as an OR prefilter.
   - Programs that use `NR`/`FNR` outside a window → no pushdown.
   - Harness and corpus in place.
   - Cost: some stages that were dropped from compute now re-run (a jq
     `select` that was fully pushed, an awk pattern). That's CPU on compute,
     not extra rows shipped, since the shard still filters.
2. **IR with `Scan`/`Filter`/`Limit`/`Opaque`** (~4–5d). Move grep, awk, jq
   and sed onto frontends, replace the `tryPushdown` loop and `limitFrozen`
   with passes, and re-express `classifyShardMode` and meta-variable
   projection on nodes.
   - Exit criteria: no planner code reads a compute string, and the harness
     is green on the full corpus.
3. **Exact lowering** (~4d). `Project` (`cut`, awk `print $n`, jq `.path`)
   and `Aggregate` (`uniq -c` chains, `wc -l`), each behind the harness. Also
   gives [perf-hints](perf-hints.md) rule 1 (pipeline reordering) a natural
   home: hints become IR rewrites that are legal but change the meaning, so
   they're offered to the user, never applied.

## Open questions

- **Multiset vs ordered comparison in the harness.** Shard fan-out merges by
  `ts`, so ties across shards can reorder. The harness probably needs a
  single-shard mode (exact order) and a multi-shard mode (order only where
  the pipeline imposes it).
- **jq exactness.** jq's `select` truthiness (`null`/`false` vs everything
  else) and number semantics vs DuckDB `json_extract` casts. Phase 1 makes jq
  prefilter-only, which removes the risk. Phase 3 decides whether exact jq
  filters are worth proving.
- **Cost of re-running prefiltered stages.** Probably negligible next to row
  transfer. Measure on the dogfood account's heaviest crons before and after
  Phase 1.
