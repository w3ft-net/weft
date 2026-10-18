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
last stage of the pipeline; piping its output onward is an error.
Use it to sketch a chart before saving the pipeline as a cron.
`--follow` is not supported in pipe form.

Charts fit the terminal and are never paged through `less`. A resize
re-fits in cron `--follow` mode on the next tick. There is no zoom or
scroll; the chart shows the configured window of data. When a bar
chart or sparkline has more points than the terminal has columns,
neighbouring points are merged into one bar each, re-applying `-a` to
their rows (`count`/`sum` add up, `min`/`max` keep dips and spikes; with
no `-a`, values are averaged), and a footer line says how many points
went into each bar. Line charts always fit and are never merged.

In cron form, the data point for each result is the **last
whitespace-separated field** of the cron's stdout, the same
convention as multi-row alert (`alert -k <N>`). The cron's output must
end in a number on each line; a cron that emits JSON rows cannot be
charted. In pipe form, `-x` and
`-y` pick fields explicitly; when omitted, x is the row sequence
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

- `-x <field>[::time[(<format>)]]` — x source. `<field>` is either a
  1-indexed whitespace column (`-x 1`) or a `.`-prefixed JSON path
  (`-x .ts`). Default: row sequence (0, 1, 2, …). An RFC3339 timestamp
  is recognised as time on its own; anything else is a plain number or
  label unless cast:
  - `::time` — x is a time: RFC3339 or epoch seconds (`-x 1::time`).
  - `::time(<format>)` — x is a time in a strftime format: `%Y` year,
    `%m` month, `%d` day, `%H` hour, `%M` minute, `%S` second, `%b`
    month name, `%z` UTC offset, `%s` epoch seconds, and the rest of
    the usual `date +FORMAT` codes. A stamp with no zone is read as
    UTC. A row that doesn't match the format stops the chart with an
    error.
  - With a time cast, a column range `<start>-<end>` joins those
    columns with a space before parsing, for stamps that span two
    fields: `-x '1-2::time(%Y/%m/%d %H:%M:%S)'`.
- `-y <field>` — y source. Same grammar as `-x`. Default: last
  whitespace field of each line. `-x` and `-y` may not mix
  positional with JSON-path in the same call. Casts are only
  supported on `-x`.
- `-a`, `--aggregate` `<fn>` — y reduction within an x-group.
  One of `count`, `sum`, `avg`, `min`, `max`, `p50`, `p95`,
  `identity`. Default `identity`, or `count` when `--bucket` is set.
- `--tag <field>` — split output into one line per distinct value of
  `<field>` — e.g. one line per host, one per
  coordinator instance. Same field grammar as `-x`/`-y`; all three
  must agree on positional vs JSON-path. Only valid with `-t line`
  (the default when `--tag` is set). Each line gets its own color and
  a legend row.
- `--bucket <DURATION>` — group rows into time buckets on x.
  Implies `-x …::time`: x must parse as RFC3339, epoch seconds, or the
  cast's format. With `-a count` (the default) or `sum`, a bucket with
  no rows is drawn as 0, so a stretch with no logs shows as a hole.
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
cat /log/myco/prod/api/host.log | jq -c '{host: $host, pct: .memory.pct}' \
  | chart -x .host -y .pct -a avg

# Pipe form: time-bucketed p95 over the current range
cat /log/myco/prod/api/host.log \
  | jq -c '{ts: $ts, pct: .memory.pct}' \
  | chart -x .ts -y .pct --bucket 1m -a p95

# Pipe form: log lines per minute, to spot a window with no logs.
# The stamp "2026/10/13 15:04:05" spans columns 1 and 2.
cat /log/myco/prod/api/main.log \
  | chart -x '1-2::time(%Y/%m/%d %H:%M:%S)' --bucket 1m

# Pipe form: plot a stream of numbers from jq
cat /log/myco/prod/api/host.log | jq '.memory.pct' | chart

# Pipe form: one line per instance
cat /log/myco/prod/api/host.log \
  | jq -c '{ts: $ts, host: $host, pct: .memory.pct}' \
  | chart -x .ts -y .pct --tag .host --bucket 1m -a avg

# --tag requires one JSON object per line, so use jq -c
```

## RELATED

`ls -l /cron.d/` adds a colored sparkline beside each cron name
using the same rendering primitives.

## SEE ALSO

`help`, `help cron`, `vi`, `cat`
