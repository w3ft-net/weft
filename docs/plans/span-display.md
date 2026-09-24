# Span display

## Status

🟢 Phase 1 shipped: `theme/span_rollup.go` (parsing + flat-table
rendering), `planner/parse.go`'s `--spans` flag, wiring into
`ExecNonInteractive` and the interactive REPL, and the `spanview`
package (list + samples drill-down, registered like chart/dashboard).
Builds on `weft-go`'s `Span` type (`StartSpan`/`RecordError`/`End`,
wired through api→coordinator→compute→shard) and picks up
`docs/plans/request-tracing.md`'s explicitly deferred hook: "nothing
stops adding `duration_field`/`parent_id_field` siblings to the same
schema-extraction pattern later."

🟢 Phase 2 (`trace spans [name]` cross-trace aggregation) implemented:
`weft-go`'s `duration_ns` numeric field, `ingest.Schema`'s four new
extraction field/pattern pairs, the four reserved shard columns,
`planner`'s `trace spans [name]` grammar, the coordinator's `TraceTag`
extension, and `spanview`/`theme` generalized to serve both phases from
one parser/renderer/View. The turned-out-simpler-than-planned
implementation note: the SQL projection needed **no synthesized
expression at all** — see "Aggregation mechanics" below. One item is
still open: `weft-go`'s `duration_ns` change is committed locally
(`~/dev/weft-go`) but not yet tagged/published as a new version; weft's
`go.mod` currently points at it via a local `go.work` entry for
development. Publishing v0.5.0 and repinning `go.mod` is the last step
before this ships to production.

## Why

Spans now exist on the wire and in the log stream — every traced
request leaves a `span start`/`span done`/`span error` line at each
of the four layers. But `trace <id>` has no idea they exist: it's
still one gap-histogram bar per raw log line (`theme/
trace_columns.go`'s `RenderTraceColumns`), so a span line renders
exactly like any other row — no name, no duration column, no way to
tell "this is where the time went" at a glance. Reading a trace today
means eyeballing raw `span done ... duration=1.2ms` text mixed in
with everything else.

Two distinct needs, at two different scales:

1. **One trace, read by a human**: "what happened, in what order, how
   long did each part take." A rolled-up view — span name + duration,
   collapsed when the same span fired more than once (a `compute`
   fan-out hits `shard.Query` once per shard) — beats scrolling raw
   lines.
2. **Many traces, read as a distribution**: "is `shard.Query` usually
   fast and sometimes slow, or always this slow?" That's an
   aggregation question — count / avg / percentile of a span's
   duration across every trace in a time range — and it can't be
   answered by fetching one trace at a time.

## Non-goals

- **No waterfall, no tree, no indentation.** `parent_span_id` already
  exists (needed for correct propagation, not display), but nothing
  in this plan renders it as a nested structure. This is a deliberate
  extension of `request-tracing.md`'s own non-goal ("No dedicated
  trace UI. No waterfall, no service map.") to spans specifically —
  every span display in this plan is a **flat list grouped by span
  name**, never a recursive tree. A waterfall is real UI complexity
  (layout, collapsing, horizontal time-axis scaling) this plan
  deliberately avoids taking on.
- **No live/streaming aggregation.** Cross-trace aggregation queries a
  historical range, same as any other weft query. No dashboard tile,
  no auto-refresh — `weft.dashboard_propose`-style surfacing is a
  possible future rider, not part of this plan.
- **No approximate/sampled percentiles.** Resolved during design (see
  "Aggregation mechanics" in Phase 2) — every matching `duration_ns`
  value per shard is pulled and stats (including percentiles) are
  computed exactly, in Go, at merge time. This is strictly cheaper
  per row than the raw log text an ordinary `cat`/`grep` query
  already pulls for the same row set (still bounded by whatever time
  range is queried, same as any weft query — just with no additional
  row-count cap layered on top of that), and that existing path is
  already fine at today's scale — sampling isn't a v1-vs-later
  question, it's just not a fit for this data
  shape at all.
- **No general-purpose "tags" system.** This plan adds specific,
  named fields (`span_name`, `span_id`, `parent_span_id`,
  `duration_ns`) the same way `trace_id` was added — not a generic
  key=value extraction mechanism for arbitrary future fields.

## Architecture

### Two phases, cleanly separable

**Phase 1 — per-trace rollup, client-side only, no backend changes.**
`trace <id>` already buffers a trace's entire row set before display
(`compute/local.go`'s `emitTraceTagged` merges the full shard fan-out
before emitting). Every span line is already in that buffer today —
rolling them up is pure client-side text parsing on data weft already
fetches, the same "server ships plain data, client renders" split
`0f710f7` (the gap-histogram commit) established for the existing
trace display. Zero schema, DDL, or SQL changes.

**Phase 2 — cross-trace aggregation, needs real backend work.**
Aggregating "every `shard.Query` in the last hour" means scanning
however many traces touched a shard in that window — that has to run
as SQL at the shard layer (the same reason `trace_id` itself is a
reserved, indexed column rather than a whole-line regex search:
"the entire value of a trace ID is exact-match point lookup," per
`request-tracing.md`). This phase promotes `span_name`/`span_id`/
`parent_span_id`/`duration_ns` to reserved columns, mirroring exactly
how `trace_id` was promoted (`af20640`).

### Phase 1 in detail

**Parsing.** `theme/trace_columns.go`'s `RenderTraceColumns` splits
each line on `\t` into `(ts_unix_ns, namespace/service, raw)` and
never inspects `raw`'s content — anything that doesn't parse as the
3-field shape already passes through unchanged (that's how the
summary line and "no lines found" survive today). A new pass, run
after that split, regex-matches `raw` against the three fixed
`log.Printf` shapes `weft-go`'s `Span` emits verbatim:

```
span start name=%s trace_id=%s span_id=%s parent_span_id=%s
span done name=%s trace_id=%s span_id=%s parent_span_id=%s duration=%s status=%s
span error name=%s trace_id=%s span_id=%s err=%v
```

These are fixed-format, no JSON, easy single-regex matches. A `span
done` line carries everything needed for the rollup (name, duration,
status) without needing to pair it against its `span start` — so the
rollup pass only needs to find `span done` lines, group by `name`,
and aggregate (count, min/avg/max duration, error count if any
`span error` shares that `span_id`).

**Rendering.** A new renderer, `theme.RenderSpanRollup(lines
[]string) []SpanRollupRow`, sibling to `RenderTraceColumns` — same
package, same input shape, different output: one row per distinct
span name, columns `NAME | COUNT | AVG | P50 | P99 | MIN | MAX |
ERRORS`. Cheap here specifically because it's already-local, already-
small data (one trace's own rows, already in memory) — sort a slice
and index into it, same computation Phase 2's aggregation does at a
larger scale (see "Aggregation mechanics" below). No terminal-width-
dependent bar chart needed here (unlike the gap histogram); a plain
table is the whole ask.

**Non-interactive flag.** `trace <id> --spans` — flag parsing added
to `planner/parse.go`'s `case "trace":` (today: `if len(args) != 1 {
error }`, no flags at all). Precedent already exists two cases above
it: `case "tail":` hand-parses `-f`/`-n N`/bare `-N` with a manual
loop over `args`; `--spans` follows the identical shape. Threading it
through: `sourceInfo` gains a `spansOnly bool` field, `QueryPlan`
mirrors it, and — same relay path `TraceTag` already uses today
(`coordinator.go`'s `c.Pool.ExecuteQuery` call → `pb.
ExecuteQueryRequest` → `compute/local.go`'s `NewJob` →
`job.traceTag`/`consume()`) — a sibling `SpansOnly` bool rides the
same wire request. Server-side, this changes nothing about the SQL
(`trace_id = ?`, unchanged) or the row data emitted — `emitTraceTagged`
doesn't need to know about the flag at all. **The rollup is entirely
a client-side reinterpretation of the same rows `trace <id>` already
returns** — this is the cleanest possible v1 slice, and ships with no
proto/SQL/DDL changes at all if we're willing to have the client
decide `--spans` behavior purely from the flag it parsed locally
(true for the CLI; the wire-relay above is only needed if the flag
should also affect what the *server* buffers/limits — worth deciding
at implementation time whether Phase 1 needs the relay at all, or can
be 100% client-side).

**`weft sh -c`/interactive parity.** Both entry points already call
one shared function for trace rendering
(`shell.RenderTraceIfApplicable`, added this session specifically to
kill the duplicate-rendering-logic bug class) — `RenderSpanRollup`
slots in beside it the same way, called from both `shell/shell.go`'s
`ExecNonInteractive` and `cmd/weft/runner/runner.go`'s `runQuery`.
**Do not** repeat this session's mistake of wiring one client entry
point and not the other.

### Phase 2 in detail

**`weft-go`: numeric duration.** `Span.End()` currently logs
`duration=%s` via `time.Duration.String()` — human-formatted
(`"1.234ms"`, `"820µs"`), not machine-parseable back into a number
without re-deriving Go's duration-format quirks (unit suffix varies
by magnitude). Add `duration_ns=%d` alongside it (keep the human
string too — raw-log readability still matters). One-line change to
`span.go`'s `End()`, a minor `weft-go` version bump.

**`ingest.Schema` extraction**, mirroring `TraceIDField`/
`TraceIDPattern`'s exact convention (`ingest/schema.go:54-85`):
`SpanNameField`/`SpanIDField`/`ParentSpanIDField`/`DurationNsField`
+ matching `*Pattern` siblings, same "field index refines one
positional field, empty field + pattern searches the whole line"
rule `extractTraceID` (`ingest/parse.go:231-268`) already
implements. Whether each gets its own field/pattern pair (more
config surface, consistent with existing precedent) or a single
combined regex with named capture groups (`(?P<span_id>...)` etc.,
one pass instead of four) is an implementation-time call — flagged
under "Open questions," not resolved here.

Practical note for weft's own dogfood sources: the `span start`/`span
done` lines are pure whitespace-delimited text where the first two
fields are literally `span` and `start`/`done`/`error` — so
`span_kind` (do we even need it as a column? see open questions)
could come from a positional field the same way `LevelField` works,
no regex required. `span_id`/`parent_span_id`/`duration_ns` still
need pattern extraction (trailing key=value tags), same shape
`trace_id_pattern` already uses for these exact log lines.

**Shard DDL.** One `ALTER TABLE logs ADD COLUMN IF NOT EXISTS <col>
TEXT` (or `BIGINT` for `duration_ns`) per new column in `shard/
shard.go`'s `migrationSteps`, same pattern `trace_id`'s own promotion
used (`shard/shard.go:252-253`) — cheap, proven, no backfill for
existing rows.

**Query grammar — a new sub-verb, not an overloaded flag.**
`trace <id>` currently requires its one positional argument to *be*
a trace ID (`planner/parse.go`'s `case "trace":`, `len(args) != 1`
check). Aggregation has no trace ID at all — proposing a sub-verb
instead of trying to disambiguate "is this token a hex trace ID or a
span name" from shape alone:

```
trace spans                 — every span name in the current time range,
                               aggregated: NAME | COUNT | AVG | P50 | P99 | ERRORS
trace spans <name>          — same, filtered to one span name, plus a
                               sample list of trace_ids that hit it
                               (the "drill down on samples" surface)
trace <id> --spans          — Phase 1's per-trace rollup (unchanged)
```

`trace spans` fans out across shards the same way `trace <id>` does
today (`Provisioner.ShardDescriptorsFor` with no namespace/service
filter) but with `WHERE span_name = ? AND ts BETWEEN ? AND ?`
(exact-match reserved column, parameterized, same shape as today's
`trace_id = ?`) instead of a trace-id lookup.

**Implementation note (as shipped): no synthesized SQL projection, and
no `span_kind` column.** Two things resolved cleaner than this section
originally assumed:

1. **The projection is plain `raw`, unmodified.** `raw` already holds
   weft-go's exact `"span done name=... trace_id=... duration_ns=...
   status=..."` / `"span error name=... trace_id=... err=..."` text —
   the same text `theme.RenderSpanRollup` already parses for Phase 1.
   There's no need to reconstruct that shape from the reserved columns
   with string concatenation; a plain `SELECT raw, ts [+ MetaVarColumns]
   FROM logs WHERE ...` (`sql.includeMetaVars`, same as bare `trace
   <id>`) produces rows Phase 1's parser consumes as-is. One parser,
   one renderer, one `spanview.SpanView` — both phases, zero new
   client-side code.
2. **No `span_kind` reserved column** (resolving the "Open questions"
   entry below). The WHERE clause is `(duration_ns > 0 OR raw LIKE
   'span error%')`: a "span done" row is the only row shape that ever
   carries a positive `duration_ns` (a "span start" row's tag is
   absent, extracted as the zero value); a "span error" row is
   identified by matching the fixed, always-present `raw` prefix
   instead of a dedicated column. Together these exclude "span start"
   noise while keeping both "done" (for stats) and "error" (for the
   `ERRORS` count) rows — exactly what `trace <id>`'s existing parser
   already expects to see.

This also means the coordinator wiring is a one-line extension, not a
new code path: `TraceTag` (the flag that makes compute buffer the
merged stream and emit the summary-line + `<ts>\t<label>\t<raw>`
format via `emitTraceTagged`) is set whenever `plan.SourceCmd ==
"trace"` **or** `"trace_spans"`. `emitTraceTagged` itself needed no
changes at all.

**Aggregation mechanics — decided, not left open.** Percentiles
aren't a decomposable aggregate (`COUNT`/`AVG`/`MIN`/`MAX` are —
merge trivially across shards; a percentile depends on the full
combined distribution's shape, not a per-shard summary number).
Confirmed empirically against the exact DuckDB version this repo
embeds (`duckdb-go/v2 v2.10501.0` → engine v1.5.1, distinct from the
locally-installed CLI's v1.5.2 — worth re-checking if the vendored
version ever moves): naively averaging four shards' own
`approx_quantile(duration_ns, 0.99)` results (`1,886,079` /
`50,494` / `50,496` / `50,496` → avg `509,391`) landed **9x off**
the true, exact global p99 (`55,489`, via `quantile_cont` over the
unsharded data) on a synthetic table with a skewed slow tail
concentrated in one shard — exactly the kind of skew a real shard
can plausibly have (a slow deploy landing inside one time-sharded
window).

Given that, v1 doesn't special-case percentiles out — every stat
(`COUNT`, `AVG`, `MIN`, `MAX`, `P50`, `P99`) is computed the same
way: each shard returns its matching rows for `span_name = ? AND ts
BETWEEN ? AND ?`; compute concatenates every shard's list (via the
same `emitTraceTagged` buffering `trace <id>` already uses — see the
"Implementation note" above) and `theme.RenderSpanRollup` computes all
statistics once, in plain Go (`sort.Slice` + indexing for percentiles)
— no DuckDB in the compute layer (confirmed compute has no DuckDB
dependency today; adding one just for this would be new infrastructure
for no reason). Exact, not approximate — full stop, not "exact as long
as row counts stay reasonable." **As shipped, this ships each row's
full `raw` text (not a projected `duration_ns`-only column)** — the
"Implementation note" above explains the code-reuse tradeoff that
motivated it; see also "No scaling lever needed" immediately below,
whose actual point (no LIMIT, no sampling, cost bounded by time range
like any other query) holds regardless of that projection choice, even
though its `duration_ns`-is-8-bytes framing described the narrower
projection this plan originally proposed.

**No scaling lever needed, and this plan doesn't invent one.** An
earlier draft of this section proposed DuckDB's native reservoir
sampling (`USING SAMPLE <n> ROWS (reservoir)` — confirmed real,
working syntax on the exact embedded version) as an escalation path
for "when row counts don't stay reasonable." That was solving a
problem this system doesn't have: `duration_ns` is a fixed 8-byte
value, while `shard/shard.go`'s `raw TEXT` column — what an ordinary
`cat`/`grep`/`awk` query already pulls per matching row, for the
exact same row set a `span_name = ?` query would match — is a full
log line, typically 100+ bytes. Pulling `duration_ns` is strictly
*cheaper* than what weft already does today as its core, everyday
function. And that existing path has **no row-count LIMIT applied by
default** (confirmed across `planner/` — every query is still
bounded by whatever time range it's given, same as any weft query;
there's just no further cap on rows *within* that range). The
streaming/backpressure machinery that actually moves those rows is
explicitly documented as fine as-is at today's scale
(`docs/plans/streaming-backpressure.md`: *"For the workloads we run
today... unbounded is fine. The plan captures the mechanism for when
it isn't"* — that "unbounded" describes queue/buffer depth, not the
absence of a time range) — a system-wide statement, not one scoped
to any particular query shape. If wide-
range fan-out ever does become a real cost problem, it shows up in
ordinary log queries first, and `streaming-backpressure.md` already
owns that fix uniformly across every query type — inventing a
span-aggregation-specific sampling mechanism ahead of that would be
solving the same problem twice, in a way that doesn't even trigger
first. Reservoir sampling is real and available if it's ever
needed for some *other* reason, but it isn't part of this plan.

**`MetaVarColumns`.** `planner/jq.go`'s `MetaVarColumns` list (the
reserved-column set forced into every `trace` projection so
`NAMESPACE`/`SERVICE`/etc. are always real, `awk`-filterable fields)
gains `span_name`/`span_id`/`duration_ns` so `trace <id> | awk
'SPAN_NAME == "shard.Query"'` works the same way `SERVICE == "api"`
does today.

### Interactive drill-down (both phases share this View)

`warp`'s `shellcore` already has the exact extensibility point this
needs — the `View` interface (`Name`/`OnEnter`/`OnExit`/`HandleKey`/
`Render`/`Tick`), registered via `RegisterView`, entered via
`EnterView(name, args)`. Two weft implementations already exist to
build on:

- `chart/view.go` — simple, read-only, Esc/`q` to exit. Not the
  shape we need (no navigation).
- `dashboard/view.go`'s `DashboardView` — **this is the template.**
  `focus`/`zoom` fields; arrow keys move focus (`moveVertical` is
  grid-aware, falls back to linear below a width threshold); `Enter`
  sets `zoom = true` and re-renders the focused tile full-screen;
  `Esc` un-zooms if zoomed, else calls `c.ExitView()`. That's
  exactly "open and close individual spans."

A new `spanview.SpanView` (new package, or folded into `theme/`)
implements two levels:

1. **List** — one row per span name (Phase 1: within one trace;
   Phase 2: aggregated across the queried range), j/k to move
   (`~/dev/warp/pkg/shellcore/less.go`'s `ModeLess` key handler is
   the vim-nav convention to copy — `j`/`k`/`g`/`G`, `/`+`n`+`N` for
   search if useful here too).
2. **Samples** (Enter on a row) — individual instances of that span:
   Phase 1 shows each occurrence within the one trace (e.g. which
   shard each `shard.Query` hit and its own duration); Phase 2 shows
   a sample of `trace_id`s that hit this span name, each with its own
   duration — letting the user pick one and hand off into the
   existing raw `trace <id>` view for that specific request. Esc
   backs out one level, same as `DashboardView`.

**Entry point.** Simplest option, and consistent with how `chart`
already behaves differently one-shot vs. interactive on the exact
same invocation (`ExecNonInteractive`'s inline `chart.RenderFromStream`
vs. the interactive `chart.Register`'d View): `--spans` means "give
me the rolled-up view" in both modes — one-shot prints the flat
table (Phase 1 architecture section above), interactive enters the
`SpanView` instead. Same flag, same query, rendering fork happens at
the client boundary that already forks other trace-specific
rendering (`RenderTraceIfApplicable`'s call sites) — no separate `-i`
flag needed.

## Components to build

Phase 1 (client-side rollup) — 🟢 shipped:
1. `theme/span_rollup.go` (new) — span-line regex parsing +
   `RenderSpanRollup`, sibling to `trace_columns.go`.
2. `planner/parse.go` — `--spans` flag on `case "trace":`.
3. `shell/shell.go` + `cmd/weft/runner/runner.go` — wire
   `RenderSpanRollup` into both entry points (see the shared-helper
   warning above).
4. New `SpanView` (level 1 + 2, Phase 1 data only) registered like
   `chart`/`dashboard`.

Phase 2 (backend aggregation) — 🟢 shipped, pending `weft-go` publish
(see Status):
5. `weft-go`: `duration_ns` on `Span.End()` — done, committed locally
   (`~/dev/weft-go`), not yet tagged/published as v0.5.0.
6. `ingest/schema.go` + `ingest/parse.go`: `SpanNameField`/
   `SpanIDField`/`ParentSpanIDField`/`DurationNsField` (+ patterns) —
   done, via a shared `extractSpanField` generalizing `extractTraceID`
   rather than four separate copies.
7. `shard/shard.go`: reserved columns via `migrationSteps` — done
   (`span_name`/`span_id`/`parent_span_id` TEXT, `duration_ns` BIGINT).
8. `planner/`: `trace spans [name]` grammar + SQL shape — done. Turned
   out not to need a new SQL projection at all; see the "Implementation
   note" above.
9. `planner/jq.go` + `planner/awk.go`: `MetaVarColumns`/`AwkMetaVars`/
   `JQMetaVars` additions — done (`span_name`/`span_id`/`duration_ns`,
   as `SPAN_NAME`/`SPAN_ID`/`DURATION_NS` and `_span_name`/`_span_id`/
   `_duration_ns` respectively).
10. ~~`compute/local.go`: new emit/merge path~~ — **not needed.**
    `coordinator.go`'s existing `TraceTag` condition (which drives
    `emitTraceTagged`) extended to also fire for `SourceCmd ==
    "trace_spans"`; the buffer-then-emit path itself needed zero
    changes. See the "Implementation note" above for why.
11. `SpanView` extended to source Phase 2 (aggregate) data in
    addition to Phase 1 (per-trace) data — done, via generalizing
    `QueryFunc`/`ListArgs` from a trace-ID-specific signature to a
    full pipeline string + display title; `spanview.Builtin` now
    recognizes three trigger shapes (`trace <id> --spans`, `trace
    spans`, `trace spans <name>`).

## Open questions

Percentile computation itself is fully resolved (see "Aggregation
mechanics" above) — no scaling lever needed, nothing deferred. If
wide-range query cost ever becomes a real problem, it's `docs/plans/
streaming-backpressure.md`'s territory (applies uniformly to every
query shape, not something this plan should special-case).

- ~~Per-field patterns vs. one combined tag-extraction regex~~ —
  resolved: per-field (`SpanNameField`/`SpanNamePattern`, etc.,
  mirroring `TraceIDField`/`TraceIDPattern` exactly), each schema field
  independently configurable.
- ~~Does a `span_kind` column (start/done/error) pull its own
  weight?~~ — resolved: no. See the "Implementation note" under "Query
  grammar" above — `duration_ns > 0 OR raw LIKE 'span error%'` is
  sufficient to isolate done+error rows from start-row noise without a
  dedicated column.
- **Sample size for `trace spans <name>`'s drill-down list** — still
  open. Current behavior: unbounded (every matching row across the
  queried range becomes a samples-level occurrence, same as the
  aggregate stats query — no separate, smaller drill-down query
  exists yet). Whether that needs a cap (N most recent, N random,
  range-bucketed) is a real question once someone drives this against
  a span name with a very large occurrence count in production; not
  addressed by this implementation.

## Cross-links

- `docs/plans/request-tracing.md` — the propagation mechanism and
  reserved-`trace_id` precedent this plan extends; also the source of
  the "no waterfall" non-goal this plan continues.
- `weft-go`'s `span.go` (v0.4.0 shipped; `duration_ns` pending as
  v0.5.0, see Status) — the `Span` type whose log lines Phase 1 parses
  and Phase 2 promotes to columns.
- `~/dev/warp/pkg/shellcore/view.go` — the `View` interface;
  `dashboard/view.go` — the focus/zoom pattern the new `SpanView`
  copies.
