# Plan: Log-to-metric (ingest-time counters)

A declarative "every row matching X increments a metric tick" mechanism. Small extension of `docs/plans/ingest-governance.md`'s rule engine — same matcher, new terminal action that emits a synthetic metric row instead of (or alongside) dropping/sampling the original.

## Why add this

Two cases the watch-based aggregation route handles awkwardly:

1. **High-frequency counters.** "Count 5xx responses, bucketed by minute." A watch polling every minute re-scans the last minute of logs each tick — fine at a few GB/day, wasteful at a few hundred. Incrementing a counter on ingest is O(rows), not O(rows × watches × ticks).
2. **Throwaway events we still want to count.** An `ingest-governance` `drop` rule kills noise for cost — but sometimes we want to keep a trace of the drop (a count) without keeping the row. Today that's all-or-nothing.

The fix is the smallest possible addition to the governance rule engine: one new action.

## Architecture

Rule engine already walks each row through an ordered list of rules (`coordinator/ingest_rules.go` per the governance plan). Add a `count` action:

```yaml
rules:
  - match: { app: "api" }
    field: ".status"
    gte: 500
    action:
      count:
        metric: api.5xx
        also_keep: true         # false by default — matching rows are dropped

  - match: { app: "nginx" }
    action:
      count:
        metric: nginx.requests
        by: [".status", ".host"]
        also_keep: true
```

When a rule fires:

1. Increment an in-memory counter keyed on `(customer, metric_name, dimension_values)`.
2. If `also_keep` is false (the default), short-circuit the row like `drop` does.
3. On a 1-minute flush tick, write each counter as a synthetic `LogRow` into an app called `<namespace>/_metrics` with payload:

```json
{
  "type": "counter",
  "metric": "api.5xx",
  "count": 142,
  "ts": "2026-04-24T14:05:00Z",
  "status": "503",
  "host": "api-3"
}
```

4. Reset the counter.

Synthetic rows go through the same drain path as any other log row, so they show up in `/log/<namespace>/_metrics.log` and every existing query mechanism works on them.

## Why synthetic log rows instead of a separate metrics table

weftd's existing metrics path already writes system metrics as JSON log rows to `<namespace>/metrics` — covered in `docs/manual/weftd.md`. The shell already knows how to query that shape with `jq`, watches already know how to aggregate it, and there's no second storage layer to maintain. Log-to-metric is just "let customers mint their own `<namespace>/_metrics` lines declaratively."

The underscore prefix (`_metrics`) distinguishes rule-generated metrics from weftd's host metrics (`metrics`), which keeps the two sources separable in queries without requiring customers to rename anything when they turn this on.

## Components to build

1. **Extend `ingest-governance` rule types** with a `count` action variant, the `metric` name, optional `by` dimension list, and `also_keep` flag.
2. **`coordinator/metric_counters.go`** — in-memory map keyed on `(customer, metric, dimVals)`; thread-safe increment; 1-minute flush goroutine that builds synthetic `LogRow`s and hands them to the existing drain.
3. **Overflow handling** — cap on distinct dimension tuples per metric per customer (say 10k). Over-cap tuples get merged into a synthetic `_overflow` tuple rather than unbounded memory growth.
4. **Crash behavior** — counters are in-memory. A coordinator restart loses the current minute's increments. Acceptable: log-to-metric is an approximate cost/noise tool, not a billing ledger. Documented clearly.
5. **Docs** — extend `docs/manual/ingest-rules.md` (from the governance plan) with the `count` action and a "common recipes" section:
   - Count 5xx rate without storing the rows
   - Count per-endpoint request rate
   - Shadow metric alongside full log retention (`also_keep: true`)

## Relationship to other plans

- **Prereq: `docs/plans/ingest-governance.md`.** Don't ship this without the rule engine; as a standalone feature it's too small to justify its own code path.
- **Uses: `docs/plans/ingest-dimensions.md`.** The `by:` list of fields picks out dimensions; if the customer has already declared `host`/`region`/`env` as configurable dimensions, `by: [host, region]` just works.
- **Obviates: per-host polling watches.** A customer who's been running "count 5xx by host every minute" as a watch can delete it and replace with a rule + consume `<namespace>/_metrics` via the same `weft.*` helpers.

## Estimated effort

*(Incremental on top of ingest-governance.)*

- Rule action extension: **0.25 day**
- Counter map + flush loop: **0.5 day**
- Overflow handling + tests: **0.5 day**
- Manual addition: **0.25 day**

**Total: ~1.5 days** on top of the governance baseline.

## Non-goals

- **No Prometheus/OpenMetrics compatibility.** Metrics flow into the log store as synthetic rows; there's no `/metrics` scrape endpoint, no exposition format. If a customer wants to feed weft metrics to Grafana, they query them out via the existing read path.
- **No histograms / summaries / gauges.** Counters only in v1. A gauge is just a counter that gets written with `count: <latest_value>` every tick, which the `also_keep: true` path already gives you.
- **No cross-coordinator counter aggregation.** If/when we run >1 coordinator, each flushes its own counters; downstream sum-by-metric-name queries add them. No distributed counter state.
- **No retroactive metrics.** Rules apply to rows as they ingest; you can't turn on a rule and get metrics for last week.
- **No exactly-once counts during coordinator restart.** In-memory means you lose the partial minute; documented as a known limit.
