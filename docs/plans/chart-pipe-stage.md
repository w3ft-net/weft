# Plan: chart as a pipe stage

## What we want

```
cat /log/acme/prod/api.log | bucket 1m | wc -l | chart
cat /log/acme/prod/api_metrics.log | jq '.cpu_pct' | chart
```

vs. today's "chart only reads from a saved watch" form:

```
chart error-rate
```

The pipe-stage form lets an analyst sketch a chart without first naming and
saving a watch — the same way `wc -l` doesn't require committing the count
to a database row before showing it.

## Why this is a plan, not a follow-on patch

Three independent missing pieces have to land before `| chart` works:

### 1. Time-bucketing primitive

Without a `bucket` (or equivalent) stage, raw-row → time-series doesn't
exist in the pipeline. The original demo plan referenced `bucket 30s`; per
the user, that builtin is being removed in favor of `weft.avg` / `weft.max`
over saved watches. So the pipe-stage chart can't piggyback on the same
machinery the watch path uses (which is "value per cron tick"); it needs
its own bucketing.

Two paths:
- **Add `bucket <duration>` back as a pipeline stage** (groups upstream
  rows into time buckets and passes one record per bucket downstream).
  Conflicts with the explicit removal of `bucket`.
- **Push the bucketing into `chart` itself** — give it `--bucket 30s` /
  `--field N` flags so it consumes raw rows + a column index and does the
  time grouping internally. Simpler for the user, hides the bucket as a
  rendering concern (fits the "charts are visual, not textual"
  feedback memory).

The second is more honest: bucketing exists *for* visual rendering, so
making it a chart-side option keeps the pipeline pure.

### 2. Chart's stdin contract

Today `RenderChartOneShot` consumes `[]WatchResultValue` (stdout + ran_at
per row). Pipe-stage chart consumes a stream of (timestamp, value) tuples
or raw lines that it'll bucket itself.

Decisions:
- Schema: assume the upstream produced one record per row, with a `ts`
  column (or take a `--ts-field N` / `--value-field N` pair). For
  `cat ... | jq '.cpu_pct'` the upstream is JSON-typed; for
  `cat ... | bucket | wc -l` it's plain text with `<bucket-time> <count>`.
- Default extraction: the *last whitespace field is the value*, the
  ts comes from the upstream's `ts` projection (today's
  `watchResultsToPoints` already follows this convention — reuse it).

### 3. Pipeline / shell wiring

`chart` today is dispatched by name in `shell/shell.go:571` as a special
case (it's not a real warp command). To run it as a pipe consumer:

- Either teach the planner to recognize `| chart` and emit a chart-as-
  terminal-stage marker (mirror to how `| less` works today via
  `pendingLess`), then have the shell collect the upstream rows and feed
  them into the chart renderer instead of writing them to stdout.
- Or expose chart as an actual warp builtin with a stdin reader. Warp's
  builtin model is already used for other commands; this is the cleaner
  path long-term but warp doesn't currently know about `WatchResultValue`
  or chart rendering — it'd need a generic byte-stream chart helper.

The `pendingLess` analogue (option A) is the smaller ask — a few dozen
lines in `shellcore/core.go` to pivot rendering when the pipeline ends in
`chart`.

## Cross-cuts with already-shipped work

- The pipe-stage form would also benefit from the cwd-aware path
  resolution proposed in `cd-cwd-state-hook.md` so `chart` invocations
  can use bare names against cwd.
- Multi-source cat (already shipped — see `planner/parse.go:extractAppsEnv`)
  means `cat /log/acme/prod/*.log | chart` is a coherent input — the
  glob expander produces a multi-app union and chart just needs to
  consume the result stream.

## Out of scope for this plan

- **Replacing the watch-driven chart.** `chart <watch>` stays — the
  pipe-stage form is *additive*. They serve different audiences: the
  watch form is for persistent dashboards; the pipe form is for one-shot
  exploration.
- **`--follow` for pipe-stage chart.** Doesn't fit — there's no
  retargetable upstream subscription. Follow remains watch-only.
- **Web shell.** The terminal renderer is ANSI; the web shell would need
  the SVG path that `chart <watch>` already uses for the dashboard.
  Same renderer choice but different transport.

## Sequencing

1. Decide bucket placement (chart-side flag vs separate stage).
2. Wire `| chart` as a terminal-stage marker in the planner +
   shellcore (mirror `pendingLess`).
3. Generalize `watchResultsToPoints` to consume a stream rather than a
   `[]WatchResultValue`.
4. Add the `--bucket` / `--ts-field` / `--value-field` chart flags.
5. Tests in `planner/` (does `| chart` produce the right plan?) and in
   `shellcore/` (does the rendered chart match against a known input?).

Estimate: 2–3 days for a working `| chart` if we go chart-side bucketing.
Longer if we re-add `bucket` as a separate stage and have to revisit the
removal.

## Question for review

The blocker decision is **bucket placement**. If we keep "no `bucket`
builtin", chart needs to absorb the responsibility. Confirm that's the
right call before this plan moves to implementation, since it changes
chart's flag surface meaningfully.
