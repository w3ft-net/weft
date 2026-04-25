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

chart — render a watch's recent results as an in-terminal chart

## SYNOPSIS

```
chart <watch> [-t TYPE] [-n LIMIT] [-i INTERVAL] [-f|--follow]
```

## DESCRIPTION

Reads recent results from a `/cron.d/<watch>` and paints a chart in
the terminal. The watch name can be given bare (`chart errors`) or
as a path (`chart /cron.d/errors`).

A one-shot `chart …` invocation renders a single frame and returns
to the prompt. `chart … --follow` enters a live-redraw mode that
refreshes on a 1-second interval; press **q** (or **Esc**, **Ctrl+C**,
**Ctrl+D**) to return.

Charts always self-fit the terminal — they're never paged through
`less`. A resize re-fits in `--follow` mode on the next tick. There
is no zoom or scroll: the chart shows the configured window of
recent results, period.

The data point for each result is the **last whitespace-separated
field** of the watch's stdout — that matches the multi-row alert
convention (`alert -k <N>` where `<N>` is the value column) for the
common case where the value is the last field. Single-scalar
watches are unaffected. Rows whose last field doesn't parse as a
number are silently skipped.

## OPTIONS

- `-t`, `--type` `<line|sparkline|bar>` — chart kind. Default `line`.
  `sparkline` is a single-line ramp; `bar` plots one bar per result
  with the timestamp under it.
- `-n`, `--limit` `<N>` — number of recent results to fetch. Default 50.
- `-i`, `--interval` `<DURATION>` — redraw interval in `--follow`
  mode. Default `1s`. Minimum `100ms`.
- `-f`, `--follow` — drop into live-redraw mode. The chart re-fetches
  and repaints every `<INTERVAL>` until you press `q`.

## EXAMPLES

```
chart error-rate                        one-shot line chart
chart error-rate -t sparkline           one-line glyph
chart error-rate -t bar                 vertical bars by timestamp
chart error-rate -n 100                 last 100 results
chart error-rate --follow               live; press q to exit
chart error-rate -f -i 5s               live, 5s redraw cadence
chart /cron.d/error-rate                path form (same as bare name)
```

## RELATED

`ls -l /cron.d/` adds a colored sparkline beside each watch name
using the same rendering primitives.

## SEE ALSO

`help`, `help cron`, `vi`, `cat`
