# Plan: Watch lowering follow-ons

Save-time lowering of cron-watch pipelines landed across eight commits
ending 2026-05-13 (`156ecb1`). The hot fire path is now ~842 ns/op
(from 1.64 ms/op baseline; ~1950× faster). What follows is the work
that was deliberately *not* in that arc, in priority order. Each item
stands alone — none are blockers for each other.

## Current state

`coordinator.Coordinator.ExecuteForWatch` checks `watch.LoweredPlan`
first. On hit, it hydrates args from a fire-time `ResolveCtx`,
dispatches directly through the compute pool, and returns. On miss
it falls back to `preflight + Plan` and synchronously refreshes the
lowered_plan column. Refresh hits also flow through a per-watch
in-memory `loweredCache` keyed by `(watch.ID, watch.ContentHash)`.

`planner.Lower` is the canonical save-time entry point.
`planner.LoweredPlan` is the persisted JSON shape; `planner.ArgSource`
distinguishes runtime values (customer, time bounds) from baked
literals (namespace/env/service names). `expandForLowering` inlines
exactly one level of `/bin/<name>` bash invocation.

Save handlers in three packages call `planner.LowerForStorage`:
`api/handlers.go`, `coordinator/service/server.go`, `mounts/cron.go`.

## Why more work

The lowered path covers the dominant production case (a watch whose
pipeline references at most one `/bin/` script). Three categories of
watches still take the slow path on every fire, plus there's an
adjacent optimization for interactive queries:

1. **Nested `/bin/` references** — a `/bin/foo` whose body itself
   calls `/bin/bar`. `expandForLowering` is single-level; Plan
   rejects the unplannable shape and Lower returns an error. The
   save handler stores NULL `lowered_plan`. Every fire pays the
   ~1.2 ms slow path.

2. **User-supplied literals in per-stage predicates** (grep
   patterns, awk values, sed addresses, jq paths). These bake into
   the SQL string today (`regexp_matches(raw, 'ERROR')`). They're
   security-OK because they pass through `escapeSQLString`, but
   every value variation produces a different SQL string — defeating
   DuckDB's prepared-plan cache at the shard. A watch that re-runs
   with the same shape but different regex inputs misses the shard
   cache every time.

3. **Alert + follow modes**. `executeLowered` explicitly falls
   through when `lp.AlertArgs != nil` or `lp.Follow`. Common
   pipelines (`… | alert above 5`, `tail -f …`) take the slow
   path. The post-process / live-broker code paths weren't ported
   to the lowered shape because they touch alert dedup state and
   the broker subscription protocol respectively.

4. **Interactive shell queries**. The shell's `Coordinator.Execute`
   pipeline doesn't have a save-time hook — every interactive `cat
   /log/foo | grep X` re-runs Plan from scratch. For dashboards
   that auto-refresh every 5 s, this is 200 fires/min per tile, all
   re-planning.

## What to build

### 1. Nested `/bin/` recursion in `expandForLowering` (highest leverage, ~2d)

Today `planner/lower.go::expandForLowering` walks the bash AST,
inlines the leading `/bin/<name>` invocation if one exists, and
records a single `BinDep`. It does NOT recurse into the inlined
body — so a body whose own leading command is another `/bin/<name>`
leaves a token Plan rejects.

The fire path's `coordinator/bin_expand.go::expandBinPipeline` has
the same single-level limit, so save-time and fire-time fall back in
the same cases. Lift both at once: walk the inlined body once more,
splice again if its leading command resolves, repeat with a depth
bound (5? 10?). Record each inlined name as a `BinDep` so the fire
path's hash check invalidates on any edit anywhere in the chain.

Risks:
- **Cycles.** `/bin/foo` calls `/bin/bar` calls `/bin/foo` —
  unbounded loop. Cap recursion depth and refuse to lower beyond
  the limit (NULL `lowered_plan`, slow path fires inline).
- **Bash positional arg semantics through nested splices.** When
  `/bin/foo a b` inlines and the body calls `/bin/bar $1 c`, the
  `$1` resolves to `a` *before* `/bin/bar` is splatted. Verify
  `bash.BindPositionals` does this correctly when called repeatedly
  across nested splices (the warp test suite probably covers it but
  the planner test suite doesn't).

Tests:
- `TestLower_NestedBin` in `planner/lower_test.go` — two-level
  `/bin/a → /bin/b → cat /log/…`, assert ShardSQL has both layers
  inlined and `BinDeps` contains both names.
- `TestLower_CyclicBin` — `/bin/a → /bin/a`, assert NULL lowered or
  a clear error, no infinite loop.

Files:
- `planner/lower.go` — extend `expandForLowering` with a recursion
  loop and a depth cap.
- Optionally `coordinator/bin_expand.go` for symmetry at the slow
  path. Skip if the slow path's recursion handling is already
  correct (the test suite will tell us).

### 2. Per-stage helper parameterization — "Step 8b" (medium leverage, ~3d)

The pre-stage adders in `Plan()` (customer, timestamps,
namespace/env/service) parameterize via `?`. The per-stage helpers
(`planner/awk.go`, `grep.go`, `sed.go`, `jq.go`, `project.go`) still
bake literals:

```go
// planner/grep.go:80
expr := fmt.Sprintf("regexp_matches(raw, '%s')", escapeSQLString(pattern))
```

That's the security path — `escapeSQLString` defends against
malicious user input — but every regex value generates a unique SQL
string. DuckDB's prepared-statement cache at the shard sees a fresh
query plan for each. With parameterization:

```go
return Predicate{
    SQL:  "regexp_matches(raw, ?)",
    Args: []ArgSource{{Kind: ArgLiteral, Value: pattern}},
}
```

The shard sees a constant SQL string across all variants of the
pipeline, and `regexp_matches`'s plan is reused across fires.

Scope:
- Every emit site in the five helper files: ~15-20 functions.
- `pushdownResult.predicate` changes from `string` → `Predicate`
  (already exists in `planner/sql.go`).
- All 252 `wantSQL` test assertions update from
  `regexp_matches(raw, 'X')` → `regexp_matches(raw, ?)` with a
  matching `wantArgs` addition. Codemod-friendly.

Watch out for:
- **awk numeric comparisons** that emit `CAST(field AS DOUBLE) > 1.5`.
  The `1.5` is a literal value that should parameterize.
- **jq path expressions** that emit
  `json_extract(payload, '$.foo.bar')`. The path string is
  user-supplied; parameterize it.
- **sed regex addresses**. Same shape as grep.
- **project.go's projection expressions** that emit field indexes
  (`list_extract(fields, 3)`). The index is numeric — fine to bake.
  Only string literals need parameterizing.

Decision gate (don't auto-proceed): run
`BenchmarkPlan` and `BenchmarkExecuteForWatch` before and after.
If shard-side plan cache hit rate doesn't improve materially in a
realistic load test, the test churn isn't worth it for the
defense-in-depth alone.

Files:
- `planner/awk.go`, `grep.go`, `sed.go`, `jq.go`, `project.go` —
  change return types from `string` to `Predicate`.
- `planner/planner.go` — `pushdownResult.predicate` from
  `string` to `Predicate`.
- 4 planner test files (`pushdown_test.go`,
  `pushdown_edge_test.go`, `namespace_test.go`,
  `tail_grep_order_test.go`) — codemod assertions.

### 3. Lower alert + follow modes (medium leverage, ~2d)

`coordinator/coordinator.go::executeLowered` falls through on
`AlertArgs != nil` and `Follow`. Two separate sub-features:

**3a. Alert pipelines.** `coordinator.runQuery` runs the alert stage
*after* draining the result via
`coordinator.runAlertStage(qr, plan.AlertArgs, alertCtx)`. The
lowered path already has `lp.AlertArgs`; what's missing is calling
`runAlertStage` after `dispatchLoweredWithTier` returns. The
returned stream's `Drain` produces a `QueryResult`; pass that to
`runAlertStage` with the existing `alertCtx`.

**3b. Follow mode (`tail -f`).** This is harder — the live-tail
broker subscription happens in `coordinator/service/server.go` and
takes a different code path (`req.Follow` triggers
`c.Coord.Tail.Subscribe` instead of `ExecuteQuery`). The lowered
plan would need to carry the broker subscription parameters
(`FollowApp`, `FollowPatterns`) and the executor would re-run the
post-source compute stages per live row. `lp.LiveStages` already
holds the stage list. The wiring is a 50-line addition to
`executeLowered` that branches on `lp.Follow` and emits via the
broker.

Watch out for:
- **Alert dedup state**. `runAlertStage` consults the `incidents`
  table to dedup. The lowered path must not bypass that — pass
  `alertCtx` through.
- **Follow + alert combined**. A watch can be `tail -f … | alert`.
  The current slow path supports it; the lowered path should too,
  or fall through cleanly when both flags are set.

Tests:
- `TestExecuteForWatch_LoweredAlert` in
  `coordinator/cron_lowered_test.go` — fire an alert pipeline
  through the lowered path, verify the incident is emitted.
- `TestExecuteForWatch_LoweredFollow` — the broker subscription
  path. May need fixture work (the broker is harder to fake).

Files:
- `coordinator/coordinator.go::executeLowered` — drop the
  alert/follow short-circuit, route both through the lowered path.
- `coordinator/coordinator.go::dispatchLoweredWithTier` — handle
  the follow branch.
- `coordinator/cron_lowered_test.go` — new tests.

### 4. Interactive-query plan cache (low leverage, ~1d)

The interactive shell's `Coordinator.Execute` path (and the
`runQuery` rewriter callback inside /bin scripts) re-plans every
call. For dashboards with 5-second tile refresh, that's 12
re-plans/minute per tile. Each plan is ~10 µs so it's not a
production problem — but it's free work to skip.

Add a `*Coordinator.planCache sync.Map` keyed by
`(pipeline_string, customer_id)`. The cached `*planner.QueryPlan`
holds the structural shape; time bounds get re-resolved from the
per-call `TimeRange` via the same `ResolveCtx` plumbing the watch
path uses. Eviction: simple max-size with LRU, or just rely on the
process restart cycle (the cache is bounded by distinct interactive
pipelines per user, which is small).

Decision gate: profile the interactive shell's per-query overhead
first. If `Plan` is already negligible relative to shard execution
(usually true), this is theoretical work and should not ship.

Files:
- `coordinator/coordinator.go` — `planCache sync.Map`,
  `cachedPlan(pipeline, custID) *QueryPlan` helper.
- `coordinator/coordinator.go::runQuery` — consult cache before
  calling `planner.Plan`.

### 5. Compute orchestration in the slow path (low leverage, research first, ~3d)

`BenchmarkExecuteForWatch/bare_inline` is still ~1.2 ms/op on the
slow path even with the lowered path bypassing it. The Phase A
profile pinned the bulk in goroutine scheduling, channel handoff
between merge and stages, and the warp interpreter's pipeline
setup. The lowered hot path sidesteps most of this; the slow path
still pays it.

This matters less than it did before lowering (every fire used to
pay this; now only fall-through fires do — alert pipelines,
unsupported shapes, version mismatches). But for those that do, the
1.2 ms could come down.

Likely levers:
- **Zero-stage fast path in compute.** `compute.LocalComputeJob`
  already special-cases zero-stage in `consume()` (streams directly
  to `streamCh` without warp). Verify it's being hit; if there's a
  reason it isn't, fix that.
- **Channel size tuning**. `LocalComputeJob.ch` is 1024; check if
  smaller (or unbuffered) hurts or helps for low-row queries.
- **Warp interpreter cold-start**. If warp does heavyweight setup
  per `bash.RunStreaming` call, amortize via a pooled interpreter
  instance.

Open this with a focused profile run after Phase B v2 (the new
lowered path drops some fire-class entirely; the remaining
slow-path callers may have different shape). Then write targeted
benches before any code change.

Files:
- `compute/local.go::consume` — zero-stage path inspection.
- Possibly `compute/compute.go` — pool the interpreter.
- New `compute/orchestration_bench_test.go` — focused per-stage
  benches.

## Estimated effort

| Item | Effort | Priority |
|---|---:|---|
| 1. Nested `/bin/` recursion | 2d | Highest — covers a real production case |
| 3. Lower alert + follow | 2d | High — `alert` pipelines are common |
| 2. Per-stage helper parameterization | 3d | Medium — depends on shard-side cache measurements |
| 4. Interactive plan cache | 1d | Low — premature without profiling |
| 5. Compute orchestration | 3d | Low — research-first, no commit before benches |

Total: ~11d. None of these blocks the others, so they can be sequenced
independently. The order above is roughly the production-impact ranking.

## Non-goals

- **Watch lowering for non-cron pipelines.** The cron path is fixed:
  same pipeline fires repeatedly. Interactive shell queries are the
  topic of #4, not the same shape.
- **Pre-warming the lowered cache at coordinator startup.** Tested
  briefly; the in-memory cache fills naturally on the first fire
  after restart and the per-fire cost is sub-µs anyway.
- **Schema changes to track `/bin/` invalidation in reverse**
  (cron_watches.bin_deps as a proper relational table). The current
  hash-check-at-fire approach is sufficient — reverse lookup would
  only matter if `/bin/` edits were on a fire-rate budget, which
  they aren't.
- **Cache for the shell's `Execute` over the gRPC path** (CLI
  invocations). Same shape as #4; if #4 ships, the gRPC handler
  gets it for free.
- **Custom binary encoding for the lowered_plan column**. JSON
  unmarshal isn't on the hot path anymore (the in-memory cache
  amortizes it). The persisted form's inspectability (`SELECT
  lowered_plan -> 'shard_sql' FROM cron_watches`) is worth more
  than the marshalling cost.

## Cross-references

- Eight commits landed 2026-05-13: `610fa8c` → `156ecb1`. The arc
  is summarized in `156ecb1`'s commit body.
- `coordinator/cron_fire_bench_test.go` is the regression net; any
  change to the lowered path should land with before/after bench
  numbers in the commit body.
- `planner/lower.go` is the canonical save-time entry point.
  `planner.PlannerVersion` is the bump mechanism for invalidating
  persisted plans after planner-breaking changes — increment when
  any of the items above changes the SQL shape.
- `coordinator/coordinator.go::Coordinator.loweredCache` is the
  per-watch decoded-plan cache; items #2 and #3 both touch the
  paths it short-circuits.
