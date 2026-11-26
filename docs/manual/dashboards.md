---
id: manual-dashboards
title: Dashboards
web:
  section: dashboards
  category: analysis-alerting
  order: 55
---

Weft allows you to visualize the results of your crons in the terminal and in
the browser, both as ad hoc charts as you are in the middle of an investigation
and as durable dashboards you come back to routinely.

Since dashboards are built on some of the same grammar as charts, let's start
with the `chart` command.

## The `chart` command

### Synopsis

```
chart <cron>       [-a <fn>] [--bucket <dur>] [--kind KIND] [--title <text>]
                   [-f|--follow [-i INTERVAL]]
<pipeline> | chart [-x <field>] [-y <field>] [-a <fn>] [-t <field>]
                   [--bucket <dur>] [--kind KIND] [--title <text>]
```

### How `chart` works

`chart` has two forms:

**Standalone**: `chart <cron>` draws what a [cron has
stored](crons.md#cron-result-persistence) over the current range. A tagged cron
draws one line per tag for the top eight series, with the rest collapsed into a
single line.  With `--follow`, the chart redraws every `INTERVAL` until you
exit. This requires the current range to be relative.

**End of pipeline**: `<pipeline> | chart [...]` draws the pipeline's output
once and returns to the prompt. `chart` must be the last stage; piping its
output onward is an error. By default, the x axis unit is line number of the
input, and the y axis values are the first whitespace-separated field of each
line. Both can be overridden with flags.

### chart options 

- `-x <field>[::time[(<format>)]]`: `<field>` is either a 1-indexed whitespace
  column (`-x 1`) or a `.`-prefixed JSON path (`-x .ts`). Derived automatically
  for crons. Value parse as an integer unless cast:
  - `::time`: x is a time in RFC3339 or epoch seconds (`-x 1::time`).
  - `::time(<format>)`: x is a time in a strftime format A row that doesn't
    match the supplied format aborts the chart with an error.
  - You can supply a column range `<start>-<end>` when casting to time for
    stamps that span two fields: `-x '1-2::time(%Y/%m/%d %H:%M:%S)'`.
- `-y <field>`:  same grammar as `-x`, except does not support time casts. Derived 
  automatically for crons
- `-a`, `--aggregate` `<fn>`: combines y values with the same x value with
  `count`, `sum`, `avg`, `min`, `max`, `p50`, `p95`, `identity`.  Defaults to
  `identity`, or `count` when `--bucket` is set.
- `-t`, `--tag` `<field>`: draw one line per distinct value of `<field>`, such
  as one per host. Same field grammar as `-x` and `-y`. Only valid with `--kind
  line` (the default when `--tag` is set). Each line gets its own color and a
  legend row.
- `--bucket <DURATION>`: group rows into time buckets on x. Arguments formatted
  as `\d[.\d](ms|s|m|h)`. 
- `--kind` `<line|sparkline|bar>`: chart kind. Default `bar`, or `line` when
  `--bucket` or `--tag` is set.
- `--title <text>`: chart title.

`-x`, `-y` and `-t` must all be columns or all be JSON paths. `-t`
needs one JSON object per line, so use `jq -c`.

### chart examples

```
# One bar per line
cat /log/myco/prod/api/host.log | jq '.memory.pct' | chart

# Error lines per five minutes. The stamp "2026/10/13 15:04:05" spans
# columns 1 and 2.
cat /log/myco/prod/api/main.log | grep ERROR \
  | chart -x '1-2::time(%Y/%m/%d %H:%M:%S)' --bucket 5m

# 95th-percentile memory use per minute from JSON fields
cat /log/myco/prod/api/host.log \
  | jq -c '{ts: $ts, pct: .memory.pct}' \
  | chart -x .ts -y .pct --bucket 1m -a p95

# One bar per host, average memory use
cat /log/myco/prod/api/host.log | jq -c '{host: $host, pct: .memory.pct}' \
  | chart -x .host -y .pct -a avg

# One line per host over time.
cat /log/myco/prod/api/host.log \
  | jq -c '{ts: $ts, host: $host, pct: .memory.pct}' \
  | chart -x .ts -y .pct --tag .host --bucket 1m -a avg
```

## Building a dashboard

Dashboards are rendered from a YAML specification in `/etc/dashboards`. The
YAML arranges your crons into sections of tiles. Tiles are a superset of what
`chart` renders, with keys mapped to the flags and arguments provided to that
command. A brief example:

```yaml
# written to /etc/dashboards/prod.yaml
title: Production
tiles:
  - kind: sparkline
    cron: myco/error-rate
    title: Errors per 5 minutes
```

You can open it in the terminal with the `dashboard` command. It will also
appear on the **Dashboards** page of the web app. You can have as many
dashboards as you like, each in its own file.  ## The dashboard command

### dashboard synopsis

```
dashboard
dashboard <name>
```

## Dashboard YAML

The dashboard YAML schema has lets you have one flat bucket for your tiles. You can also group your tiles into sections. The top-level keys in the schema are as follows:

| Field      | Required | Notes                                           |
|------------|----------|-------------------------------------------------|
| `title`    | no       | Heading at the top of the dashboard            |
| `sections` | one of   | Array of sections, each with an optional `title` and a required `tiles` list |
| `tiles`    | one of   | A flat tile list (one untitled section)         |

A simple example dashboard with sections:

```yaml
title: Production
sections:
  - title: Errors
    tiles:
      - kind: sparkline
        cron: myco/error-rate
        size: md
  - title: Capacity
    tiles:
      - kind: counter
        cron: myco/pending-jobs
        format: "%d jobs"
        size: sm
```

### Tile kinds

Dashboard tiles are built on the same foundation as the `chart` command, so you can easily convert a 1-liner to to the appropriate YAML. For example,`chart myco/error-rate --kind bar -a max --bucket 1h --title "Worst hour"` becomes:

```yaml
- kind: bar
  cron: myco/error-rate
  aggregate: max
  bucket: 1h
  title: Worst hour
```

In addition to the `chart` kinds (`line`, `sparkline`, and `bar`), dashboards
provide a few more options for laying out values on a tile grid where, in the
terminal, normal output works just fine:

| Kind        | Default display                                     | Extra options                   |
|-------------|-----------------------------------------------------|---------------------------------|
| `counter`   | latest value (on a tagged cron, the highest)        | `format` -- strftime e.g. `"%d ms"`, `"%.1f%%" |
| `gauge`     | latest value as a bar from 0 to `max` (default 100) | `max` |
| `table`     | latest run, row per tag                             | - |

### Permissions

All dashboards are team-wide, but tile content applies standard read privileges rules. When someone opens a dashboard
if a tile relies on a cron they don't have read rights for, its content will be blank.

### Rendering alerts

If a tile targets a cron that uses `alert`, weft  will render that alert level
automatically as a part of the tile wherever possible:

* Lines, sparklines, and bar charts draw the cutoff as a red line
* Counters and gauges shows their content in red when the value exceeds the threshold
* Tables highlight offending rows in red

A counter, gauge or table that aggregates over a range compares the aggregate,
not the latest run. Other alert conditions, such as `rising` or `absent`, have
no single number to draw. To change where a tile turns red, change the alert.

## Layout

Tiles appear in YAML order, flowing left to right and then down. You can
specify width of a given tile with the optional `size` key. Valid values are
`sm`, `md`, and `lg`. `lg` will always be full width, with packing of `sm` and
`md` tiles best effort in both browser and terminal.

## Building dashboards with robots

The weft MCP server provides tools for proposing dashboards (and crons if you
are starting from scratch). Worth trying if you need a little help getting
started.
