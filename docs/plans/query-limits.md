# Query limits and a more useful `explain`

> **Status (2026-10-30): built, awaiting rollout.** The keyed aggregate, the
> `explain` line count, the server-side deadline and the first lowering gaps are
> done and tested on a stack of local branches (see "As built"); the first two
> steps are on `main`. `explain analyze` is struck (decided 2026-10-30). What is
> left is under "Left to do".

## Start here (handoff for a fresh session)

**Goal.** A query that runs too long is cut off by one server-side deadline, and
the user is taught what to change by `explain`, not by a row cap. See
"Decisions" and "As built".

**Left to do** (in rough order):

1. (Done, in the stack.) `HTTPCoordinator.Query` no longer uses the client's 10 s
   whole-request deadline, so CLI users get the server deadline's own message;
   other calls keep the short deadline and now return plain messages (`shell/remote_error.go`).
2. Roll the stack out (see "As built") and verify on production data: a tally, a
   top-N, a distinct count, `explain` with a count, each against the stage-by-stage
   output.
3. Choose the deadline value from the `query rpc: deadline` log lines (60 s is a
   placeholder).
4. Lowering gaps that need work outside the planner:
   - `grep -F`, `-w`, `-o`: warp's grep has none of them (they fail with
     "unknown option" today), so they need to land there first.
   - `wc -c`, a max (`jq … | sort -n | tail -n 1`), and keyed sum/min/max need new
     shard aggregates: an expand release (compute first), then the planner.
   - jq `group_by` needs a design call: jq's own key ordering and output shape.
5. Thin advice (see "Survey"): per-function awk advice, jq `test`, the `tr`
   reorder. The survey golden is the fixture set.
6. A cron suggestion when a result is one or two values (open: a size threshold).

Starting points in the code: `planner/ir.go` (node kinds), `planner/frontend.go`
(lowering), `planner/passes.go` (aggregate recognition, `fuseAggregate`),
`planner/awk.go` and `planner/tally.go` (awk lowering), `planner/sql.go` (shard
backend), `planner/analyze.go` and `planner/analyze_counts.go` (what `explain`
prints), `api/explain.go` (the count query), `coordinator/service/server.go`
(`Execute` and the deadline). Acceptance fixtures: `TestAnalyze_Survey`
(`planner/testdata/analyze_survey.golden`) and `TestAnalyze_SummaryGolden`
(`analyze.golden`), regenerated with
`go test ./planner -run 'TestAnalyze_(SummaryGolden|Survey)' -update`, and the
differential harness (`planner/difftest`), which pins each lowering to what the
real stages print.

**Constraints to carry.** `explain` speaks only user terms (no SQL, shard, compute
or Go vocabulary). Show real counts, not relative volume. Suggest a rewrite only
when it provably gives the same result and analyzes strictly better; otherwise stay
quiet. Count aggregates are safe to lower; sum, min and max only when the field is
provably numeric. Bug fixes start with a failing test.

**State of the tree.** The implementation is a stack of branches in the
`../weft-r3` worktree, linear, each a fast-forward push in this order, one deploy
between each:

1. `key-count-compute`: compute and proto only (the distinct-total and key-count
   renders). Pushed.
2. `awk-tally-planner`: the planner sends them (top-N, distinct count, awk tally;
   `PlannerVersion` 13). Pushed.
3. `delete-legacy-aggregate`: compute rejects the old `lines`, `distinct` and
   `count_distinct` aggregate strings.
4. `explain-counts`: `explain` shows the retrieval line count.
5. `query-deadline`: the server-side deadline.
6. `lowering-gaps`: `grep -E`, awk `$NF` and `length`.
7. `field-projection`: a first awk or jq stage that stays in compute is sent only
   the fields or keys it reads.
8. `jq-lowering-exactness`: lowered jq programs drop the rows jq errors on and
   write numbers and strings as jq does.
9. `shell-client`: plain messages for failed API calls, and a query's time limit
   left to the server.

A caller never ships in the same deploy as the callee support it needs: proto3
drops an unknown field silently, so a new coordinator sending a field an older
compute doesn't know would get wrong output with no error. Compute rolls minutes
after the coordinator in a deploy; shards only on a manual `weft-admin roll-all`.
Uncommitted in the main checkout, and not part of this work: `shell/http_remote.go`
and its test, `planner/analyze_test.go`, `planner/testdata/analyze.golden`, the
survey golden. Related docs: `shell-explain.md`, `perf-hints.md`, `planner-ir.md`.

## Problem

A query over a day of a moderately chatty service's logs failed for the user with
`Post "https://api.w3ft.net/api/shell/exec": context deadline exceeded
(Client.Timeout exceeded while awaiting headers)`. Findings:

- The limit that fired was not a product control. It is the 10-second wall-clock
  timeout on `HTTPCoordinator.client`, a default for short management calls that
  `Query` inherited. The server's SSE handler had not sent headers yet.
- There is **no server-side query deadline** (none in `api/` or
  `coordinator/service/`) and **no cap on rows handed to compute** (none found
  in `coordinator/`, `compute/` or `planner/`).
- The error text exposed the server URL and the client's implementation, and told
  the user nothing to do. Fixed on the client (the 24 call sites that go through
  `remoteClient` now return plain messages).
- The pipeline's shape explains why it is expensive: `grep` and `jq` were
  optimized retrieval, but the `awk` after them runs post-retrieval, so **every
  row matching the grep over the whole time range** crosses to compute. The
  unmeasured question is how many rows that is; the log being scanned is
  inflated by the heartbeat poller.

## Decisions (Paul, 2026-10-29)

- Timeouts are wanted: they prevent bad usage. They belong at a **server-side
  chokepoint**, not in each client.
- **One deadline value for now**, for everyone. No per-tier values yet.
- **No row limit.** The goal is to teach people to fetch the data they
  actually want, not to cap the machinery. The deadline is the only hard control.
- Backgrounding (long-running queries that dump to tmp) is a future feature for
  work that legitimately runs long; not part of this.
- The guidance a user gets when the deadline trips is **`explain`**, and the
  intent is to dig in hard on it and on the IR's interpretation of a pipeline,
  providing as many solid hints as we can (see "Survey" below).
- **Real counts, not relative volume.** Show actual counts (DuckDB counts at the
  source are fast: a day of matches in about 0.6 s). No "source" line; all
  consecutive optimized stages share one line, `N lines retrieved with
  optimization`; no explanatory prose per stage. `explain analyze` (running
  everything and reporting per-stage actuals) was considered and struck on
  2026-10-30: the timeout error carries the static report instead.
- **Quiet unless very confident:** a concrete rewrite only when provably the same
  result; a cron suggestion when the result is one or two values.
- **Lower before suggesting:** build the keyed-aggregate IR node first.

## Design

### 1. Server-owned limits

- **Deadline.** The `Execute` stream loop in `coordinator/service/server.go` is
  where every one-shot result passes. Wrap its context in a deadline there;
  cancelling it cancels the shard and compute work. Follow mode (long-lived by
  design) is exempt; it needs its own idle rule, not this one.
- **One safe message.** The deadline returns a structured error (a gRPC status the
  API maps to the SSE `error` event) whose text contains no internals. It states
  what happened, then appends the `explain` summary for that pipeline.
  `planner.Analyze(pipeline, cwd, resolveBin)` takes no customer and reads no
  state, so the server can run it at the moment a limit trips. The client's
  timeout text keeps its own hint for the case where the client gives up first.
- **Value** is config in version control (Terraform), not a literal in code.
  One value for all; per-tier remains a possible plan lever later
  (`cost-and-pricing.md`).

### 2. Make `explain` helpful for this class of query

What `explain` says today (`planner/testdata/analyze.golden`) for the failing
pipeline:

```
  1  grep "query rpc: done"   optimized retrieval
  2  jq -r .msg               optimized retrieval
  3  awk '{c=$4; …}'          post-retrieval
       Rule without a pattern: acts on every row.
       Try: To select rows, add a pattern. Totals (n++, sums) are post-retrieval;
       to count, use `grep PATTERN | wc -l`.
Barrier: stage 3.
```

Gaps, each checked against the planner:

1. **It never says what the barrier costs.** *Superseded:* the answer is the real
   row count (Improvement 2), not prose about the range.
2. **The best fix is not offered.** `grep … | awk '{ print $4 }' | sort | uniq -c`
   is entirely optimized retrieval (verified in the golden file) and is "count
   queries per customer". *Superseded:* a per-key tally is a lowering gap, so the
   keyed-aggregate node (Improvement 1) makes it optimized rather than advising a
   rewrite. Sums and maxima per key (`s[c] += v`) have no optimized form.
3. **"No optimized form." is a dead end** where a near equivalent exists:
   `cut -d' ' -f4` is post-retrieval, `awk '{ print $4 }'` is not. They differ on
   runs of spaces, so suggest it only with that caveat (the plan's rule engine
   already insists on checking placement by replanning).
4. **Advice stays in user terms** (`shell-explain.md`): no SQL, shard, or
   compute-pool vocabulary, so it is safe to attach to a server error.

Acceptance: the pipelines added to `TestAnalyze_SummaryGolden` (the failing
awk and the four rewrites) are the fixtures; the golden file changes to show the
new text, and the diff is the review.

### 3. What the user sees when the deadline trips

Plain wording that the time limit was reached, then the static `explain` report
for their pipeline, then what to change:

```
This query did not finish in the time allowed (60 seconds).

  1  grep "query rpc: done"   optimized retrieval
  2  jq -r .msg               optimized retrieval
  3  awk '{c=$4; d=$NF; …}'   post-retrieval
       Rule without a pattern: acts on every row.

Barrier: stage 3.

Narrow the time range, or filter earlier in the pipeline.
```

It carries no per-stage counts of the cancelled run: those needed the
`explain analyze` instrumentation, which was struck. `explain` itself shows the
retrieval's line count (Improvement 2), and the report in the error does not,
since a count query would take more of the time that just ran out.

## Survey: what the IR does with common log-analysis shapes

`TestAnalyze_Survey` (golden: `planner/testdata/analyze_survey.golden`) runs
about 35 common pipelines through `planner.Analyze`. The golden is the living
catalog and the fixture set for every improvement below. Corrections first:
`grep PATTERN | wc -l` **is** fully optimized (the count is an aggregate at the
source), as are `sort | uniq -c`, `sort -u`, and a bare `wc -l`. What is not:

*Status 2026-10-30: the aggregation gaps below are closed except `wc -c` and
the max, which need new shard aggregates; `grep -E`, `$NF` and `length` are
lowered; `-F`, `-w`, `-o`, `substr` and `toupper`/`tolower` are not (see "Left to
do" and "As built"). The golden in the main checkout predates this and needs
regenerating after the stack lands.*

**Aggregation gaps** (the IR has only `count` and `sort`+`uniq` aggregates)
- `sort -u | wc -l` (distinct count) is post-retrieval; it is `COUNT(DISTINCT)`.
- `sort | uniq -c | sort -rn | head -n N` (top N by count, the most common
  idiom): the first three stages are optimized, the last two run on the grouped
  rows. explain prints "Barrier: stage 4" as if it were costly, but only one row
  per distinct value crosses. It should say so, and the IR could fuse
  `uniq -c | sort -rn | head -n N` into an ordered, limited aggregate at the source.
- awk tallies and sums (`n++`, `c[$4]++ … END`, `s += $5`) are all
  post-retrieval, with the same generic "add a pattern" advice that doesn't fit.
  They are GROUP BY count/sum in disguise.
- `uniq -c` without a preceding `sort` is post-retrieval (correct: it collapses
  only adjacent lines). No hint says that `sort` first changes the meaning.
- `jq … | sort -n | tail -n 1` (a max) is post-retrieval; ordered-limit fusion
  does not cover a keyed numeric sort of a projected value.
- `wc -c` is post-retrieval; it is a SUM of lengths.

**Lowering gaps with no or weak advice**
- `grep -F`, `-E`, `-w`, `-o`: not optimized, advice only restates the flag.
  `-F` should be a plain contains-match; check what the source does for plain
  patterns before deciding on `-E` and `-w`.
- awk: `$NF` (hint exists, but `$NF` is common and could lower), `substr`,
  `toupper`/`tolower`, `length`: the "Print only" advice is wrong for a user who
  wants a transformation.
- awk numeric pattern (`$5 > 500 { print }`) reports "partly optimized" with a
  one-line reason; the action is a plain print, so the user cannot tell why it
  is partial (the comparison is only a prefilter).
- jq: `test("x")` and `@tsv`/`@csv` have no advice; `select(.msg | test(…))` could
  be hinted as `grep x | jq …` where equivalent.
- `tr … | grep ERROR` makes the grep post-retrieval with no advice. The grep can
  move first only for a plain translation (`tr a b`) whose pattern contains
  neither the old nor the new character. **Not for `tr -d`:** deleting characters
  can join text into a match (`ERR\rOR` matches `ERROR` only after `tr -d '\r'`).
  Replanning verifies placement, not meaning, so each rewrite rule must prove its
  own equivalence; `tr -d` gets no suggestion.
- `cut -d' ' -f4` ("No optimized form") where `awk '{ print $4 }'` is optimized;
  differs on runs of spaces, so any suggestion carries that caveat.

### Improvements, in order of leverage

1. **A keyed-aggregate IR node, designed first. (Built, for counts: see "As built".)** Group by an expression with
   count now and sum/min/max later, rendered as `GROUP BY` at the source, with
   the output text shaped by the frontend. `sort | uniq -c`, `wc -l`, and
   `sort -u | wc -l` (distinct count) lower into it, as do the awk tally idioms
   (the recognition lives in the awk frontend) and `uniq -c | sort -rn | head -n N`
   as an ordered, limited aggregate. Design questions to settle: the node's shape
   (key expression, aggregate list, optional order and limit); how output text
   for each frontend is reconstructed exactly (awk prints `key value`, `uniq -c`
   prints `  N key`); which awk sums and maxima are provably equivalent (numeric
   fields only) and the rest stay opaque; how `explain` reports the count of an
   aggregating group (rows matched versus rows returned); tests pin output
   equality on real inputs, not just placement.
2. **Show the real counts. (Built.)** (Replaces an earlier idea of printing a relative
   "row class", which Paul rejected as meaningless to someone trying to debug.)
   Counts that stay in retrieval are cheap, so `explain` can run them. Measured
   on production on 2026-10-29, default range (about a day) of the coordinator
   log, through the CLI (wall time includes startup and TLS):

   | Pipeline | Result | Wall time |
   |---|---|---|
   | `cat main.log \| wc -l` | 962,932 rows | 0.92 s |
   | `… \| grep "query rpc: done" \| wc -l` | 259,613 | 0.55-0.58 s (3 runs) |
   | `… \| grep … \| jq -r .msg \| wc -l` | 259,592 | 2.15 s |
   | `… \| grep … \| awk '{ print $4 }' \| sort \| uniq -c` | 259,585 for one customer | 4.22 s |

   The pipeline that timed out at 10 s would have shipped those ~260 k rows to
   the post-retrieval stage and run awk on each (not timed end to end; the
   client gives up first). Mechanism: the stage texts are verbatim
   (`StageReport.Src`), so the count for a prefix is just
   `<stages 1..k> | wc -l` run through the normal path, and only a prefix that
   `Analyze` itself reports as fully optimized is run. Projection stages do not
   change a count, so one count per filtering stage suffices, run in parallel.
   Rules: a prefilter stage's count is an upper bound ("up to"); counts get
   their own short budget and fall back to the static output with a note when
   the range is too large or a cold shard is slow; the count is "as of now",
   since the default range slides. Counts need a lowered distinct count
   (`sort -u | wc -l` today is post-retrieval) to report "N distinct values"
   after a `uniq`.

   Paul's direction: there is no "source" line (it says nothing a debugger
   needs), and **all consecutive optimized retrieval stages share one count**,
   since one scan produces it. Mockup of the failing pipeline (the 259,662 is
   measured):

   ```
   $ explain grep "query rpc: done" | jq -r .msg | awk '{c=$4; d=$NF; …}'
     1  grep "query rpc: done"   optimized retrieval
     2  jq -r .msg               optimized retrieval
          259,662 lines retrieved with optimization
     3  awk '{c=$4; d=$NF; …}'   post-retrieval
   ```

   Nothing follows the awk. This pipeline sums and maximizes per customer, which
   has no optimized form, so there is no confident rewrite and the output stays
   quiet (see Suggestions).

3. **Suggestions, only when very confident** (replaces "recipes"; see the next
   section).
4. **Lower the remaining idioms** beyond the keyed aggregate. (Partly built:
   `grep -E`, `$NF`, `length`. The rest is under "Left to do".)
5. **Fix thin advice**: per-function awk advice, the partial-pattern explanation,
   jq `test`, the `tr` reorder.
6. **The server-side deadline and its safe error.** (Built, with the static
   report; sections 1 and 3.)

## Suggestions: only when very confident

`explain` stays quiet unless it has something it can stand behind. There is no
explanatory prose per stage (the placement already says it); the line under an
optimized run is just `N lines retrieved with optimization`. Two kinds of
suggestion are allowed, and each has to meet a bar.

**1. A pushdown rewrite that gives the same result.** A rule fires only when it
can *prove* its candidate produces the same output, and the candidate must also
analyze strictly better (a later barrier, or more stages in retrieval; the
engine already replans to check). Equivalence is argued per rule, not by the
planner. A rewrite that answers only part of the question is not shown (the
failing pipeline's counts, sums and maxima: only the counts are expressible, so
that pipeline gets nothing). The first rule, a pure per-key tally, is exact
including format, because the closing awk runs on the few aggregated rows:

```
$ grep ERROR | awk '{ c[$4]++ } END { for (k in c) print k, c[k] }'
  1  grep ERROR                                            optimized retrieval
       41,310 lines retrieved with optimization
  2  awk '{ c[$4]++ } END { for (k in c) print k, c[k] }'  post-retrieval

  Same result, counted in retrieval:
    grep ERROR | awk '{ print $4 }' | sort | uniq -c | awk '{ print $2, $1 }'
  (Line order may differ: awk leaves the order of `for (k in c)` undefined.)
```

Why it is exact: `$4` has no whitespace, so `uniq -c`'s `N key` splits back
into `$1` and `$2`; an empty key prints the same line both ways. The rule must
match the tally shape exactly (no pattern, one array, a single END loop that
prints the key and its count) and stay silent on anything else. Tests pin the
output equality on real inputs, not just the placement.

**Where the knowledge for rule 1 would live (an honest accounting).** As sketched,
the tally rewrite is mostly hardcoded awk knowledge, not something the IR derives:

- *Recognizing the shape* (no pattern, one array incremented by a field, a single
  END loop printing key and value) is a match on warp's awk AST in
  `planner/awk.go`. Today that frontend lowers only patterns and print
  projections; END is a flat refusal (`reasonAwkEnd`) and arrays are not
  understood at all.
- *The replacement text* (`… | awk '{ print $2, $1 }'`) is a template, and
  *why it is exact* is a hand-written argument per rule.
- *What the IR contributes* is the check that the candidate analyzes strictly
  better, and the existing `sort | uniq` aggregate the candidate leans on. It has
  no notion of "group by key"; its aggregates are `wc -l` and `sort`+`uniq`.

That is a signal to **prefer lowering over suggesting**. If the IR had a keyed
aggregate node (group by an expression, with count, and later sum/min/max), then
the awk frontend would lower a recognized tally into it, `sort | uniq -c`,
`wc -l` and `sort -u | wc -l` would lower into the same node, and the tally
would simply *be* optimized retrieval: no suggestion, no rewritten text for the
user to take. The awk-specific recognition does not go away (it moves into the
frontend, which is where awk knowledge belongs, as with patterns today), but the
user-facing result changes from "type this instead" to nothing to fix. Care is
needed per aggregate: a count is safe; sum, min and max must honor awk's
number-or-string rules for each value, which SQL's casts do not, so those
aggregates lower only when the field is provably numeric (for instance a jq
number) or not at all. Suggestions remain for rewrites that need the user's
text to change, and the tally rule is the stopgap until the node exists.

**2. A cron, when the result is one or two values.** Provable statically when the
pipeline ends in a scalar aggregate (`wc`, an awk END that prints once with no
loop, `head -n 1` or `-n 2` last), when the pipeline's result is statically known
to be small. It names the exact file, because `/cron.d`
scripts are plain files with a schedule comment, and each run sees only its own
interval:

```
$ explain grep "query rpc: done" | wc -l
  1  grep "query rpc: done"   optimized retrieval
  2  wc -l                    optimized retrieval
       259,662 rows matched, 1 comes back

  This returns a single number. To track it over time, save a cron as
  /cron.d/rpc-queries:
      # schedule: */5 * * * *
      cat /log/weft/coordinator/main.log | grep "query rpc: done" | wc -l
  Each run sees only the last five minutes.
```

The suggested pipeline carries no `head`/`tail` (they undercount per interval, per
`docs/manual/crons.md`); the schedule is one the user's tier allows (5 minutes is
the floor on every tier); it is shown only to someone who can edit `/cron.d`
(`cronmgr` or admin) and below the cron-count limit; and it is skipped when the
output does not fit the cron contract (a number, optionally followed by a tag).
Open: a size threshold so a trivial count over a few hundred rows does not nag.

## As built

**Grouping.** A shard-side grouping is a `GroupSpec` (render, optional order and
limit) in `ExecuteQueryRequest.group`; `aggregate` now carries only the trace and
span aggregates. The shard returns one row per distinct line with its count
(`GROUP BY raw`), compute sums across shards and prints:

| Render | Prints | Replaces |
|---|---|---|
| total | one number | `wc -l` |
| line | each distinct line | `sort -u`, `sort \| uniq` |
| count line | `%7d line`, optionally highest count first and cut to N | `sort \| uniq -c [\| sort -rn \| head -n N]` |
| distinct total | one number | `sort -u \| wc -l` |
| key count | `key count` | awk `{ c[K]++ } END { for (k in c) print k, c[k] }` |

Output equality is pinned by the differential harness against the real stages:
ties (descending byte order, as `sort -rn` leaves them), non-ASCII, empty keys,
blank and whitespace-only lines. The awk tally is recognized only as that exact
shape (no pattern, one array, one END loop printing key then count); the key
lowers as a print argument does, and a tally after a projection or a pushed line
count stays as stages. The node is count-only: typed keys and sum/min/max wait.

**`explain` count.** One query, the optimized prefix piped to `wc -l`, shown above
the first stage that isn't entirely optimized. For an aggregate it is the lines
returned (the groups), which is what the next stage reads. It runs through the
same coordinator call as the caller's own query (their token, org, directory
stack, range), so access is checked as for any run, within a 5 s budget; a failure
prints `line count unavailable: <reason>` and the rest of the report is unchanged.
None for a fully optimized pipeline, a follow, or a trace.

**Deadline.** `QueryDeadlineSeconds` (runtime config, SSM key
`QUERY_DEADLINE_SECONDS`, default 60, 0 disables), applied in `Execute` to every
one-shot query. Follow and crons are exempt. Cancelling stops the shard and
compute work. The error is `DeadlineExceeded` with no internals; each trip logs
`query rpc: deadline` with the customer and pipeline. The value is in the runtime
config's version-controlled defaults with an SSM override, not Terraform. Tiered
deadlines were considered and deferred: one value until the logged trips say what
a normal slow query looks like, and there is almost nobody to differentiate yet.

**Field projection.** A first awk stage that reads only some fields is sent, per
line, a record with just those fields in their places (placeholders before the
last one named, padded to the true field count only if it reads `NF` or `$NF`; a
line with no fields passes through as it is when `$NF` is read, since there
`$NF` is `$0`), and the stage runs unchanged. A first jq stage that reads only
some top-level keys is sent a `json_object` of just those, with each value whole;
a payload that isn't an object, or has a bare integer `-0` (which the JSON
functions would write as `0`), is sent whole. Both are whitelists, so anything
not known to leave the rest of the line or object alone (`$0`, a bare `print`, a
rule with no action, a `/re/` on its own, a computed field, `keys`, `.`, `has`,
`..`, an assignment to a field or `NF`) keeps shipping everything. Measured over
wide rows: about 20% of the bytes for an awk reading two of eleven fields, about
1.5% for a jq reading two of thirty-two keys. Not applied after a pushed line
count, a projection, row stages, a follow, or a trace.

**jq exactness.** Lowered jq programs (`jq '.a'`, `jq '[.a, .b]'`, `select(...)`)
had two divergences from running jq, both found by the differential harness and
fixed (`jq-lowering-exactness`):

- A row jq errors on printed nothing, but the lowering printed `null`: a payload
  that isn't an object, a key read through a value of the wrong type. Every path
  a program reads is now guarded, the payload itself included, and a tail
  projection (an array or object of paths) is guarded for each path.
- Numbers and strings were written as DuckDB's JSON functions write them, not as
  jq does. Warp's jq prints the float64 it parses in jq 1.7's layout (`2.0` as
  `2`, `1e3` as `1000`, `1e-7` as `1e-07`, `1e21` as `1e+21`, a 20-digit integer
  rounded to float64 digits), and writes a string through Go's encoder once it
  holds a quote, backslash, control character or non-ASCII character, which also
  escapes `<`, `>` and `&` and writes backspace and form feed as `\u0008` and
  `\u000c`. Arrays and objects are now written out as text, each value through
  the same rules. A number that is already in jq's form (an integer below 2^53,
  a plain decimal between 1e-4 and 1e15) is passed through, so the cost is
  about that of a string: 100 to 250 ms per 100,000 rows.

Not fixed, and exact only where stated: a payload holding a bare integer `-0`
prints `0` where jq prints `-0` (DuckDB parses it as integer 0, so no function
sees the sign; `-0.0` is right); and a number or an HTML-escapable string nested
inside a value the program prints whole (`jq '.o'` where `o` is an object) keeps
DuckDB's spelling, since only the top-level value and the values of arrays and
objects the program builds are rewritten.

**Lowering.** `grep -E` (warp's grep ignores the flag: every pattern is RE2),
awk `$NF` (the last field, or `$0` on a blank line) and awk `length` (bytes, as
warp counts). The partial-pattern advice no longer blames a plain print action
for a stage that a number comparison holds back. Not lowered, and why: `substr`
slices bytes (and can fail on a negative length), and `toupper`/`tolower` use
Go's Unicode case mapping, neither provably equal to SQL's; `-F`, `-w` and `-o`
don't exist in warp's grep.

## Open questions

1. The deadline value (one value for all). Choose it from the logged trips, or by
   timing the failing aggregation at 1 h, 6 h and 24 h with the client timeout
   lifted.
2. (Settled) The keyed-aggregate node is a `GroupSpec`; an aggregating group's
   count is the lines returned.
3. How the source lowers plain `grep` patterns (to decide `-F`, `-E`, `-w`).
4. A size threshold for the cron suggestion.
