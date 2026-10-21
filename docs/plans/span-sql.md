# Span queries in SQL

## Status

Implemented. Supersedes the client-side rollup in `docs/plans/span-display.md`.

Deviations from the proposal: `status` was not added to the meta-variable
table, because no column backs it (error spans are rows at level ERROR);
the samples query is `trace spans <name> --samples`; the partial and final
aggregates are computed in `compute/span_aggregate.go`, with the SQL in
`planner/spans.go`.

## Problem

`trace <id> --spans` and `trace spans [name]` find spans by running a regex over
each row's raw text (`theme/span_rollup.go`: `span done name=… span_id=…
duration_ns=… status=ok`). The span name, span ID, parent ID and duration are
already columns in the shard (`span_name`, `span_id`, `parent_span_id`,
`duration_ns`), filled either by the SDK line header or by schema extraction.
Reading them back out of text is redundant, and it broke when the SDK started
writing those values into the header: the body that reaches the rollup is
`{"msg":"span done","status":"ok"}`, so both views render nothing for any
SDK-fed source.

The same duplication shows up one layer down. The meta-variable columns are
spelled in three places (`metaVars`, `MetaVarColumns`, `JQNumericMetaColumn`),
and `parent_span_id` is in none of them, so a trace query cannot project it.

It is also slow. `trace spans` ships every span row in the time range to the
client and aggregates there, so cost grows with span volume, not with the
number of distinct span names.

## Goals

- Span aggregation runs in DuckDB, per shard, and the coordinator merges small
  partial results. Cost grows with span names, not span rows.
- One definition of the row meta-variables drives awk, jq, the projected
  columns and the numeric-column set.
- Span views read columns only. The raw body is never parsed for span data.
- Output rows are ordinary JSON lines, so `trace spans` is pipeable like any
  other source.

## Design

### 1. One meta-variable table

Replace `metaVars`, `MetaVarColumns` and `JQNumericMetaColumn` (`planner/jq.go`)
with one ordered table:

```go
type metaVar struct {
	Name    string // lower case: "host"
	Column  string // "instance"
	Numeric bool   // jq reads it as a number
	Project bool   // appended to the SELECT when meta-variables are included
}
```

Everything else is derived from it at init: `AwkMetaVars` (upper case),
`JQMetaVars` (lower case), `MetaVarColumns` (the distinct `Project` columns, in
table order), and the numeric set. Aliases (`host` for `instance`) are rows that
share a column and are not projected twice. `parent_span_id` and `status` are
added here once.

A test asserts that every name in the table appears in
`docs/manual/metavars.md`, and that every projected column exists in the shard
schema, so a new column cannot be added in one place only.

The pinned SQL strings in the planner tests change once, when the order is made
explicit.

### 2. `trace spans` as a shard-side aggregate

`trace spans [name]` becomes a plan with an aggregate, like the existing
`planner.Aggregate` (`lines`, `distinct`, `count-distinct`), instead of a row
fetch. Each shard runs:

```sql
SELECT span_name AS name,
       count(*)                                   AS count,
       count(*) FILTER (WHERE level = 'ERROR')    AS errors,
       sum(duration_ns)                           AS total_ns,
       min(duration_ns), max(duration_ns),
       <histogram bucket>                         AS bucket,
       count(*)                                   AS bucket_count
FROM logs
WHERE span_name IS NOT NULL
  AND (duration_ns > 0 OR level = 'ERROR')     -- done and error lines, not starts
  AND ts BETWEEN ? AND ?                        -- plus the usual source predicates
  [AND span_name = ?]
GROUP BY span_name, bucket
```

The coordinator merges per-shard partials: counts, errors and totals add; min
and max combine; buckets add.

**Percentiles.** Exact percentiles cannot be merged across shards. Each shard
returns a log-scale histogram (bucket = `floor(ln(duration_ns) / ln(1 + e))`
with relative error `e` of 1%), and p50 and p99 are read from the merged
histogram. They are within 1% of the true value, which is far finer than the
resolution anyone reads latencies at. Count, errors, total, min, max and the
average are exact. The manual says percentiles are approximate.

**Error spans.** The SDK logs a failed span at level ERROR, so an error is a
row with `span_name` set and `level = 'ERROR'`. This replaces matching
`raw LIKE 'span error%'`.

### 3. `trace <id> --spans` reads columns

A single trace is small, so it stays a row fetch, but it fetches only the span
rows (`span_name IS NOT NULL`) and only the columns it needs (`span_name`,
`span_id`, `parent_span_id`, `duration_ns`, `ts`, namespace, service). Self time
is computed from them: a span's start is `ts - duration_ns`, its children come
from `parent_span_id`, and self time is its duration minus the union of its
children's intervals. This is the logic in `computeSelfDurations`, fed from
columns.

### 4. Samples drill-down

The span view's samples level (the slowest occurrences of one span name, each
with its trace ID) becomes its own query: the same filter, `ORDER BY
duration_ns DESC LIMIT n`, returning span rows. Selecting one hands off to
`trace <id>` as today.

### 5. Output shape

The aggregate returns one JSON line per span name:

```json
{"name":"db.query","count":18420,"errors":3,"total_ns":9210000000,
 "avg_ns":500000,"p50_ns":410000,"p99_ns":2300000,"min_ns":90000,"max_ns":8100000}
```

A bare `trace spans` renders the table and the interactive view from these
lines. Piped, it is plain data, so `trace spans | jq 'select(.p99_ns > 1e9)'`
works and can feed `alert`. `trace <id> --spans` emits the same shape plus a
`self_ns` field.

### 6. Old sources

Rows from text-format sources have the same columns, filled by the schema's
span extraction patterns, so they work unchanged. A source with no span columns
filled has no spans to roll up and says so.

## Components

1. Meta-variable table, derived maps, the docs/schema consistency test.
2. Shard-side span aggregate in the planner (`trace_spans` plan), SQL, and the
   proto field that carries it.
3. Coordinator merge for the span aggregate, next to the existing aggregators
   in `compute/aggregate.go`.
4. `trace <id> --spans` fetch narrowed to span columns, with self time from
   columns.
5. `theme.RenderSpanRollup` and `spanview` read the JSON lines. The regexes in
   `theme/span_rollup.go` are deleted.
6. Samples query.
7. Manual page for `trace`, `trace <id> --spans` and `trace spans`.

Steps 1 to 5 land together, because 5 deletes the code the others replace.

## Tests

- A span line written by the SDK (header values, JSON body) rolls up correctly.
  This is the regression test and is written first.
- The same spans from a text-format source (columns from extraction) give the
  same rollup.
- Merging partial aggregates from several shards equals one aggregate over the
  union, for count, errors, total, min, max, and percentiles within 1%.
- Self time for sequential, nested and overlapping children.
- `trace spans | jq` and `| alert` work.
- Meta-variable table consistency (docs and shard schema).

## Open questions

- **Percentile accuracy.** 1% relative error from a log histogram, or exact
  percentiles with a single-shard fast path and a documented caveat for
  multi-shard ranges?
- **Per-service breakdown.** Decided: the aggregate always groups by service
  (namespace and service are part of the group key), since the same span name in
  two services is usually two different things. There is no `--by` option.
- **Retention.** `trace spans` over a long range scans every span row in it.
  Whether to cap the range, as `trace <id>` does by default, or leave it to the
  shell's `range`.
