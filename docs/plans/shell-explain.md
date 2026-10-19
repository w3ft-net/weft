# `explain` in the weft shell

A shell command that takes a one-liner and tells the user, in their own
vocabulary, where each stage runs and what would make it faster. It is built
on the planner's IR, so it describes exactly what a run would do. It exposes
no SQL, columns, shard, or compute-pool vocabulary.

The existing `planner.Explain` / `cmd/explain` stays as the internal planner
debugger (it prints SQL, bound args, pass logs). This plan is the
customer-facing counterpart, the same relationship `docs/plans/perf-hints.md`
describes between debugging `explain` and hints. The new one is a shell
builtin, not a `cmd/`, so the two don't collide.

## What the user sees

```
weft> explain cat log/weft/coordinator/main.log | awk '$3 == "ingest-stream:" { printf "%s\n", $0 }' | grep 'first row'

  1  cat log/weft/coordinator/main.log        reads the source
  2  awk '$3 == "ingest-stream:" { printf…}'  partly optimized
       The pattern is optimized retrieval. The action, which does more
       than print fields, runs on every row that comes back.
  3  grep 'first row'                         post-retrieval
       Runs on the rows stage 2 returned.

  Barrier: stage 2. Everything after it runs on returned rows.

  Try:
    - Use `print` instead of `printf` in stage 2: then stages 2 and 3
      both are optimized retrieval.   (same result)
```

A fully-lowered pipeline says so plainly, and says there is nothing to gain:

```
  Every stage is optimized retrieval. Only matching rows are returned.
```

### Vocabulary

Two placements only, in user terms:

| Term | Meaning | Internal equivalent |
|---|---|---|
| **optimized retrieval** | part of the retrieval itself: rows are narrowed and reshaped as they are fetched, so only the result comes back | node left of the shard/compute line |
| **post-retrieval** | runs on rows already returned to you | node right of the line (`opaqueNode`, anything above one) |
| **partly optimized** | the stage's filter is optimized retrieval, the rest of the stage still runs post-retrieval | `filterNode(prefilter)` + `opaqueNode` |

"Barrier" is the first stage that is not fully optimized retrieval. This is the most
useful single fact: the existing pass log already records it ("stopped at
Opaque[1: awk]: it runs in compute, and so does every node above it").

No cost model in v1. The command says where work runs and what would move it;
it does not predict seconds. Row counts and durations are the hints system's
job (`perf-hints.md`), which has runtime stats.

## Design

### 1. A public analysis type in the planner

`planner.Analyze(pipeline, cwd, resolveBin) (*Analysis, error)` (in
`planner/analyze.go`) is built from `compile()`'s result, the same compile
`Plan` uses, so `explain` can't drift from what a run does. It takes no
customer: nothing in the analysis reads one. A leading `/bin` script is
inlined through `resolveBin`, as a run does (nil leaves it alone).

```go
type Analysis struct {
    Stages  []StageReport
    Barrier int          // 1-based index of first non-"at source" stage; 0 if none
}

type StageReport struct {
    Index int
    Src   string        // the user's text, verbatim
    Where Placement     // Optimized | Partial | PostRetrieval
    Why   *Advice       // nil when AtSource; {Because, Try}
}
```

`Suggestion` (rewrites) and source facts come in later phases; the per-stage
`Advice.Try` is all phase 1 has.

A stage's placement is derived from the lists a run uses: a stage still runs
post-retrieval iff it is one of `Plan`'s row or compute stages (after every
pass: aggregation, projection pushdown and filter hoisting have already
removed theirs), and part of it is optimized retrieval iff one of its nodes is in
the shard (`Partial`). A
multi-node stage (awk Filter+Project, which `rowStage.nodes` now emits for awk
and jq alike) therefore needs no per-language code here: this is where the IR
pays off.

Nothing in `Analysis` mentions SQL, columns, bound args, or node kinds.

### 2. Advice keyed by `reasonCode`

`reasonCode` is already a typed enum and every opaque/prefilter node carries
one. Add a table `reasonCode → Advice{Because, Try}` written in user terms
("reads a field by a computed index like `$NF`; use a fixed position, or jq").
A test enumerates every code through `reasonCmpLast` and fails when one has no
advice, so adding a frontend reason forces writing its user-facing text.

Reasons that carry a `detail` (a flag, a command name) interpolate it.

### 3. Rewrite suggestions, verified by replanning

Rules propose a candidate pipeline string. The engine runs `Analyze` on the
candidate and keeps the suggestion only if placement is strictly better: more
stages in optimized retrieval, or a later barrier. The planner judges the benefit, not
the rule, so rules can't claim a speedup that doesn't happen.

Equivalence is the hard part and the planner can't check it. Each rule carries
its own argument, and is labelled `Same: true` only when it is a pure
rewrite. v1 keeps this list short and conservative:

1. Merge adjacent jq stages into one (`jq A | jq B` → `jq 'A | B'`), when
   neither uses `-s`, `-n`, `--arg`, or other flags.
2. Replace `printf "%s\n", X` / single-arg-equivalent awk actions with `print X`.
3. Collapse `grep A | grep B` chains the planner already pushes: no-op
   (listed only to say "nothing to do").

Everything else is advice text, not a rewrite. The planner already hoists
filters across a plain sort, so the "move grep before sort" case from
`perf-hints.md` mostly needs no suggestion; `explain` just reports it ran at
the source.

Non-equivalent but useful narrowing (such as the schema-aware `LEVEL`
suggestion in section 6) is `Same: false`, phrased as an option.

### 4. Where it runs

Every shell (CLI interactive, CLI one-shot, browser) ships each line verbatim
to `/api/shell/exec`, whose server owns shell semantics. `explain` is answered
there, in `api.maybeHandleExplain`, beside the team commands that are also
handled in-process before the line reaches the coordinator. So:

- No client change: all three shells get it, and none of them link the planner
  (the shell is wasm-buildable).
- The line arrives as typed. A shellcore builtin would only see
  whitespace-split arguments, so it could not hand the planner the real text.
- It never reaches the coordinator, runs nothing, and reads no log data. It
  names no source to check access to, so it can't be used to probe what exists.
- Output travels as ordinary `line` events; errors (not a log query, a parse
  error) as `stderr`. No wire change and no new route.
- The word is `explain` followed by whitespace or end of line; `explainer foo`
  goes to the coordinator like any other line. `explain` is a reserved name, so
  a saved `/bin/explain` can't shadow it.

MCP's `weft.suggest_query` (phase 5) calls the same `planner.Analyze`; it adds
a structured form of `Analysis` when something consumes one, not before.

### 5. Command grammar

Decided: intercept the `explain ` prefix on the raw line, before it is parsed,
so `explain cat a | grep b` explains the whole line rather than piping
`explain` into `grep`. Shell highlighting and history treat what follows
`explain` like a normal pipeline. `explain` never runs the pipeline.

### 6. Schema facts

**Placement does not depend on schema, so `Plan` does not change.** The shard
fills `fields` from `raw` with `awkFields` (`shard/shard.go`), a split
independent of the source's schema, made so awk's `$N` could lower to
`fields[N]` (commit `8c31c814`, 2026-10-14). Whether a stage runs at the
source therefore never consults a schema, and `explain` stays a faithful
description of a run.

One residue: rows ingested before that commit hold the older, schema-dependent
split, and no marker distinguishes them (no version on a row or a shard). It
affects only pre-2026-10-14 data still inside retention. Not worth plumbing;
record it in `docs/known-issues.md` if it isn't there already.

**Deferred: source-specific advice.** Advice that depends on which meta-variables
a source actually populates (a variable that matches nothing, `grep ERROR` →
`LEVEL == "ERROR"`) waits for a follow-up pass, after the weftd changes that
are reworking how meta-variables get set. Nothing in phases 1 and 2 depends on
it, and `Plan` and placement never will.

Constraints to keep in mind when that pass happens, not designs:

- The source's schema alone doesn't prove a column is empty or filled (SDK
  line headers and agent config also set columns, and not every ingest path
  copies every schema-extracted value), so a "never set" warning needs
  evidence from what rows actually carried, not the schema.
- Claims must be one-directional and carry their evidence ("not seen since
  <date>"), and say `Unknown` when there is none.
- The planner stays pure: facts arrive through an interface, and a nil one
  means no source-specific advice.
- The source's format (text/json/prefix_json) is available from the schema and
  is a safe, independent input for the "wrong tool for the format" advice.

### 7. Explain at cron save

A saved cron runs on every fire, so a stage stuck post-retrieval is paid for
on every tick; the person saving it is the one who can fix it, once. So saving
a cron explains it automatically, with no `explain` to remember to type.

This rides on the mechanism `docs/plans/cron-truncation-lint.md` already
built: `store.SaveCronWatch` ends in `lintSavedWatch`, whose non-blocking
messages travel as `store.Warning`s and are rendered by every surface (shell,
API, MCP) through one generic "show what came back" path. Explain-at-save is
another caller of that path, not a new channel, and no surface gets a branch
for it.

- **Input.** `lintSavedWatch(pipeline, loweredPlan)`: `Analyze` needs the
  pipeline as well as the stored plan. The planner is pure and DB-free, so
  `store` calls it as it calls `LintTruncationRisk`. No endpoint is involved.
  A cron whose plan did not lower, or that inlines a `/bin` script
  (`BinDeps`), is not explained: its stages aren't what `Analyze` sees, and a
  wrong barrier is worse than none.
- **Output.** The same report the `explain` command prints, as text, but only
  when something is perf-impacting: a barrier, or a suggestion. A pipeline that
  is entirely optimized retrieval saves silently, with no message at all. Wording
  for a cron says what it costs ("runs every 5m"), since the repetition is the
  point.
- **Never blocks.** The save has already succeeded; this only adds messages,
  like the truncation lint.
- **One renderer.** `Analysis.Summary()` in the planner package produces the
  text for both the `explain` command and the save message, so the two can't
  drift. Styling and width-fitting are not built; add them when a surface needs
  them.
- **Two compiles, not one.** The truncation lint reads the stored lowered plan
  and the explanation compiles the pipeline; sharing them would mean
  restructuring the stored plan for no user-visible gain.
- **Stored plans don't re-explain on a planner upgrade.** The advice is what
  the planner said at save time. Re-explaining crons when `PlannerVersion`
  bumps would let existing crons benefit from planner improvements, but it is
  a separate feature (it needs somewhere to show the result).

## What must never leak

A test renders `Analyze` output (text and JSON) for the full planner harness
corpus and fails on any of: SQL keywords as words, column names from
`AwkMetaVars`'s targets, `shard`, `compute`, `gpipe`, `DuckDB`, `Parquet`,
`warp`, `prefilter`, `pushdown`, `Opaque`. This is the guard rail for the
requirement and it runs over every pipeline the planner is tested against.

## Phases

1. **Analysis + render, no rewrites, no schema.** `planner.Analyze`, placement
   from IR spans, advice table with the exhaustiveness test, leak test, a
   golden-file corpus of rendered output. Test that placement agrees with
   `Plan`'s compute-stage list for every corpus pipeline.
2. **`explain` command + cron save.** Server-side interception in
   `/api/shell/exec` (section 4), the reserved name, `man explain`, and
   explain-at-save through `lintSavedWatch` (section 7). Docs under
   `docs/shell/` (so `make docs` regenerates the help corpus and manual);
   never name warp.
3. **Source-specific advice.** Deferred to a follow-up pass (see section 6).
4. **Verified rewrites.** The candidate/replan engine and the v1 rules above.
5. **Share with hints and MCP.** `hints.Suggest` consumes `Analysis` plus
   `RuntimeStats`; add the MCP tool. Update `perf-hints.md` to say the engine
   is this one.

Phase 1 alone is useful (users learn where their pipeline stalls) and carries
the risky part (the public shape), so ship and live with it before adding
source-specific advice or rewrites.

## Open questions

None outstanding.
