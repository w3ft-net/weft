---
id: man-chart
title: chart
shell:
  const: ManChart
  topics:
    - "man chart"
    - "chart --help"
  banner: "CHART(1)"
---

## NAME

chart — render a chart in the terminal, from a cron or a pipeline

## SYNOPSIS

```
chart <cron> [-t TYPE] [-n LIMIT] [-i INTERVAL] [-f|--follow]
<pipeline> | chart [-x <field>] [-y <field>] [-a <fn>] [--tag <field>]
                   [--bucket <dur>] [-t TYPE] [--title <text>]
```

## DESCRIPTION

`chart` runs in two forms:

**Cron form** — `chart <cron>` reads recent results from a
`/cron.d/<cron>` and paints a chart in the terminal. The cron
name can be given bare (`chart errors`) or as a path
(`chart /cron.d/errors`). One-shot renders a single frame and
returns; `--follow` enters a live-redraw mode that refreshes every
`<INTERVAL>` until you press **q** (or **Esc**, **Ctrl+C**,
**Ctrl+D**).

**Pipe form** — `<pipeline> | chart [...]` renders the upstream
output once and returns to the prompt. The chart stage is the
terminal of the pipeline (no further commands after it). Useful for
sketching a chart without first committing the pipeline to a saved
cron. `--follow` is not supported in pipe form.

Charts always self-fit the terminal — they're never paged through
`less`. A resize re-fits in cron `--follow` mode on the next tick.
There is no zoom or scroll: the chart shows the configured window
of data, period.

In cron form, the data point for each result is the **last
whitespace-separated field** of the cron's stdout — matching the
multi-row alert convention (`alert -k <N>`). In pipe form, `-x` and
`-y` pick fields explicitly; without them, x is the row sequence
and y is the last whitespace field of each line.

## OPTIONS (cron form)

- `-t`, `--type` `<line|sparkline|bar>` — chart kind. Default `line`.
  `sparkline` is a single-line ramp; `bar` plots one bar per result
  with the timestamp under it.
- `-n`, `--limit` `<N>` — number of recent results to fetch. Default 50.
- `-i`, `--interval` `<DURATION>` — redraw interval in `--follow`
  mode. Default `1s`. Minimum `100ms`.
- `-f`, `--follow` — drop into live-redraw mode. The chart re-fetches
  and repaints every `<INTERVAL>` until you press `q`.

## OPTIONS (pipe form)

- `-x <field>` — x source. `<field>` is either a 1-indexed
  whitespace column (`-x 1`) or a `.`-prefixed JSON path (`-x .ts`).
  Default: row sequence (0, 1, 2, …).
- `-y <field>` — y source. Same grammar as `-x`. Default: last
  whitespace field of each line. `-x` and `-y` may not mix
  positional with JSON-path in the same call.
- `-a`, `--aggregate` `<fn>` — y reduction within an x-group.
  One of `count`, `sum`, `avg`, `min`, `max`, `p50`, `p95`,
  `identity`. Default `identity`, or `count` when `--bucket` is set.
- `--tag <field>` — split output into one line per distinct value of
  `<field>` instead of a single line — e.g. one line per host, one per
  coordinator instance. Same field grammar as `-x`/`-y`; all three
  must agree on positional vs JSON-path. Only valid with `-t line`
  (the default when `--tag` is set). Each line gets its own color and
  a legend row.
- `--bucket <DURATION>` — group rows into time buckets on x.
  Requires x to parse as a timestamp (RFC3339 or epoch seconds).
  Switches the default chart kind to `line`.
- `-t`, `--type` `<line|sparkline|bar>` — chart kind. Default `bar`,
  or `line` when `--bucket` or `--tag` is set.
- `--title <text>` — chart title.

## EXAMPLES

```
# Cron form
chart error-rate                        one-shot line chart
chart error-rate -t sparkline           one-line glyph
chart error-rate -t bar                 vertical bars by timestamp
chart error-rate --follow               live; press q to exit

# Pipe form: a quick distribution by host
cat /log/myco/api/main.log | jq '{path, latency}' \
  | chart -x .path -y .latency -a sum

# Pipe form: time-bucketed p95 over the current range
cat /log/myco/api/main.log \
  | chart -x .ts -y .latency_ms --bucket 1m -a p95

# Pipe form: just plot a stream of numbers from jq
cat /log/myco/api/main.log | jq '.cpu_pct' | chart

# Pipe form: one line per instance
cat /log/myco/coordinator/runtime_stats.log \
  | jq 'select(.metric=="mem_pct")' \
  | chart -x .ts -y .value --tag .coordinator_id --bucket 1m -a avg
```

## RELATED

`ls -l /cron.d/` adds a colored sparkline beside each cron name
using the same rendering primitives.

## SEE ALSO

`help`, `help cron`, `vi`, `cat`
