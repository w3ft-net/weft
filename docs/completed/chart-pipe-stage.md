# Plan: chart as a pipe stage

## What we want

```
cat /log/acme/prod/api.log | chart -x ts -y latency_ms --bucket 1m -a p95
cat /log/acme/prod/api.log | jq '{path, latency}' | chart -x .path -y .latency -a sum
cat /log/acme/prod/api_metrics.log | jq '.cpu_pct' | chart
```

vs. today's "chart only reads from a saved watch" form:

```
chart error-rate
```

The pipe-stage form lets an analyst sketch a chart without first naming and
saving a watch — the same way `wc -l` doesn't require committing the count
to a database row before showing it.

## Decisions

These were open questions in earlier drafts; locked in below.

### No `bucket` builtin

The originally-proposed `bucket <duration>` pipeline stage stays gone.
Bucketing is a rendering concern, not a pipeline concern — it exists *for*
the chart and folds into chart's flag surface.

### Flag surface (v1)

```
-x <field|N>          x source — 1-indexed column, or `.foo` JSON path
-y <field|N>          y source — 1-indexed column, or `.foo.bar` JSON path
-a / --aggregate <fn> y reduction within a group: sum|count|avg|min|max|p50|p95
                      default: count when --bucket is set OR when -y is omitted; identity otherwise
--bucket <duration>   time-bucket x values (only when x resolves to a timestamp)
-t / --type <kind>    line|bar|sparkline (default: bar; line when --bucket is set)
--title <text>        chart title (no axis labels in v1 — see follow-up plan)
```

Defaults preserve today's `chart <watch>` ergonomics for the watch path,
which keeps using its own argument parser. The pipe-stage flags only apply
when `chart` appears as a pipeline consumer.

### Aggregation: one flag, not two

There is no separate x-aggregation. `-a/--aggregate` is the y reduction
applied when multiple input rows share the same x value (or fall in the
same `--bucket` window). For non-time data, identical x values are grouped
naturally; for time data, `--bucket` discretizes x first, then the same
rule applies. There is no use case for "aggregate x" — x is the grouping
axis, not the value axis.

### Field addressing: positional or JSON path

- A bare positive integer (`-x 1`, `-y 2`) means a 1-indexed whitespace
  column on each input line.
- A `.`-prefixed token (`-x .ts`, `-y .request.latency_ms`) means a
  JSON path against each input line parsed as JSONL.
- The presence of any `.`-prefixed flag value flips the line parser into
  JSONL mode for that invocation. Mixing positional and JSON-path flags
  in the same chart call is an error.
- Default: if both flags are omitted, x is the schema-projected `ts`
  column when present; y is the last whitespace field of each line.
  This preserves today's `watchResultsToPoints` convention and makes
  `wc -l | chart` continue to work.

### Default chart type: bar

When `-t` is omitted, default to `bar`. `--bucket` overrides to `line`
(time-bucketed data is unambiguously a line chart). `-t` always wins.

This is a deliberate departure from the watch-driven path's line default
because the pipe-stage form is more often categorical (`-y .path -a sum`)
than time-series.

### Axis labels: v2

`--x-label` / `--y-label` are out of scope for v1 — the chart package
has no axis-label support today. See `chart-pipe-stage-axis-labels.md`
for the feasibility subplan; estimate is a half-day, deferred until we
have real demand from `| chart` users to inform the rendering convention.

### Grammar: flags, not expressions

No `--x-agg 'bucket(1m)'` expression form. Discrete flags only. If a
second time-bucket strategy ever lands (e.g. log-spaced), revisit then.

### Stdin contract: chart-side, no planner involvement

Originally proposed as planner-tagged. **Won't do.**

The chart-args signal (`.`-prefixed → JSONL, else text) already covers
every case where the user has expressed intent. The only scenario
planner StreamShape would uniquely address is "auto-pick a sensible
field from JSON with no flags," and it can't actually do that — chart
still needs `-y .field` to know what to plot. For bare-scalar `jq` output
(e.g. `jq '.cpu_pct'`) the text fallback already gives the right answer.

Cost would be: shellcore importing planner (or duplicating bash-AST
classification), plus a stage-tag wire format. Marginal gain, real
plumbing — leave it out permanently.

## Bonus: JSON-path field selectors for `alert` (landed)

The `-k <N>` and `-t/--tag <N>` flags on `alert` were 1-indexed
positional. They now accept either form via the shared `FieldSpec`:

```
... | jq 'select(.type=="disk")' \
    | alert above 80 -k .pct -t .host --notify=ops
```

Plumbed end-to-end:

- `shellcore/alert_args.go` — `AlertArgs.Key`/`Tag` changed from `int`
  to `FieldSpec`; `ParseAlertArgs` calls `ParseFieldSpec`. Mixing
  positional and JSON-path across `-k`/`-t` is rejected (the line
  parser can't be in two modes at once).
- `coordinator/alert.go:splitAlertRows` — uses `ResolveField` instead
  of whitespace-split + index.
- `store/shell.go` — `CompiledWatch.AlertKey`/`AlertTag` and
  `CronWatch.AlertKey`/`AlertTag` changed from `*int` to `*string`,
  storing the FieldSpec source form (e.g. `"3"` or `".foo.bar"`).
- `coordinator/cron.go:persistResults` — re-parses the stored string
  back to `FieldSpec` per tick and uses `ResolveField` for value/tag
  extraction.
- `store/migrations/037_alert_key_jsonpath.sql` — column-type change
  with a `USING ::TEXT` cast that preserves existing positional values.
- Docs (`docs/shell/man-alert.md`, `docs/shell/help-alerts.md`) and
  generated `shellcore/docs_gen.go` + `frontend/src/generated/docs/`
  updated.
- Tests: `store/watch_validate_test.go` covers JSON-path stamping;
  `coordinator/alert_test.go` covers JSONL e2e + parser mixing
  rejection.

## Cross-cuts with already-shipped work

- The pipe-stage form benefits from the cwd-aware path resolution
  proposed in `cd-cwd-state-hook.md` so `chart` invocations can use
  bare names against cwd.
- Multi-source cat (already shipped — see `planner/parse.go:extractAppsEnv`)
  means `cat /log/acme/prod/*.log | chart` is a coherent input — the
  glob expander produces a multi-app union and chart consumes the
  result stream.
- The chart package itself (`shellcore/chart/`) already supports line,
  bar, sparkline, time-x, thresholds, and self-fitting to the terminal
  (commit `8087a15`). v1 of pipe-stage chart only adds the input-side
  plumbing — no chart-package changes required.

## Out of scope for v1

- **Replacing the watch-driven chart.** `chart <watch>` stays — the
  pipe-stage form is *additive*. They serve different audiences: the
  watch form is for persistent dashboards; the pipe form is for one-shot
  exploration.
- **`--follow` for pipe-stage chart.** Doesn't fit — there's no
  retargetable upstream subscription. Follow remains watch-only.
- **Web shell.** The terminal renderer is ANSI; the web shell would need
  the SVG path that `chart <watch>` already uses for the dashboard.
  Same renderer choice but different transport.
- **Axis labels.** See `chart-pipe-stage-axis-labels.md`.

## What landed (v1)

1. **JSON-path resolver** — `shellcore/field_resolver.go` defines
   `FieldSpec` (positional or gjson path) and `ResolveField`. Uses
   `github.com/tidwall/gjson` (already a transitive dep, now direct)
   for path extraction — single-pass, no allocation on the leaf.
2. **`ChartPipeArgs` parser** — `shellcore/chart_pipe.go` parses
   `-x`, `-y`, `-a/--aggregate`, `--bucket`, `-t/--type`, `--title`.
   Defaults: kind = `bar` (or `line` when `--bucket`); aggregate =
   `identity` (or `count` when `--bucket`). Mixing positional and
   JSON-path in one chart call is rejected.
3. **`RenderChartFromStream`** — same file. Walks input lines, applies
   x/y extraction, optional time bucketing, group-by-x reduction, and
   feeds `chart.Builder` (line / bar / sparkline). Time bucketing
   recognizes RFC3339 and epoch (int or float).
4. **`pendingChart` wiring** in `shellcore/core.go` — mirrors
   `pendingLess`. Adds `chart` to `terminalCommands`; `splitTerminalStage`
   now returns `[]string` argv with quote-unwrapping; `startQuery`
   parses chart args and sets `pendingChart`; `handleQueryResult` routes
   to `RenderChartFromStream` instead of scrollback when `pendingChart`
   is set. Errors during arg parsing surface to scrollback without
   dispatching the upstream query.
5. **Non-interactive runner** — `shell/shell.go:ExecNonInteractive`
   calls `shellcore.SplitChartPipeline` to separate the upstream from
   the chart args, runs the upstream query, and renders the chart to
   stdout once.
6. **Tests**:
   - `field_resolver_test.go` — positional, JSON path, missing keys,
     non-JSON lines, bare-scalar JSON.
   - `chart_pipe_test.go` — parser defaults / overrides / validation;
     bucketed sum, categorical bar; reduce / parseTime.
   - `chart_pipe_e2e_test.go` — end-to-end through `HandleEvent`,
     verifying the upstream pipeline is dispatched chart-stripped, the
     rendered chart lands in scrollback, parse errors don't dispatch,
     and `splitTerminalStage` handles chart with quoted args + leaves
     the legacy `less` contract unchanged.

## Deferred

- **Axis labels** — `chart-pipe-stage-axis-labels.md` (v2).
- **Web shell** — interactive native CLI and `weft sh -c` are working;
  the SVG path used by the dashboard chart isn't wired to pipe-stage.

## Won't do

- **Planner StreamShape inference** — see section above. Chart-args
  signal is enough; planner-side classification is plumbing for
  marginal gain. Don't reopen unless `jq '...' | chart` ergonomics
  become a recurring complaint.

## Estimate (final)

Chart pipe-stage v1: ~1 day's work end-to-end. Alert JSON-path follow-up
is a second commit at roughly the same size driven mostly by store +
cron + dashboard plumbing.
