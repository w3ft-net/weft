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
chart <cron> [-t TYPE] [-n LIMIT] [-i INTERVAL] [-f|--follow]
<pipeline> | chart [-x <field>] [-y <field>] [-a <fn>] [--tag <field>]
                   [--bucket <dur>] [-t TYPE] [--title <text>]
```

### How chart draws

`chart` has two forms.

**Standalone**: `chart <cron>` draws a cron's stored results, one point per
run, so it can cover days or weeks instead of the current range. Give the
name bare (`chart error-rate`) or as a path (`chart /cron.d/error-rate`).
The point for each run is the last whitespace-separated field of its
output, the same rule `alert -v` uses, so each run's output has to end in a
number. With `--follow`, the chart redraws every `INTERVAL`
until you exit.

**Pipe form**: `<pipeline> | chart [...]` draws the pipeline's output once
and returns to the prompt. `chart` must be the last stage; piping its
output onward is an error. `-x` and `-y` pick the fields to plot. Without
them, x is the row number and y is the last whitespace-separated field of
each line, drawn as one bar per line. The chart covers whatever the current
[`range`](../shell/man-range.md) covers, so change the range and run it
again to see a different window.


Charts fit the terminal and are never paged through `less`. There is no
zoom or scroll. When a bar chart or sparkline has more points than the
terminal has columns, neighbouring points merge into one bar, re-applying
`-a` to their rows (`count` and `sum` add up, `min` and `max` keep dips and
spikes; with no `-a`, values are averaged), and a footer line says how many
points went into each bar. Line charts always fit and never merge.

### chart options for pipelines

- `-x <field>[::time[(<format>)]]`: the x field. `<field>` is either a
  1-indexed whitespace column (`-x 1`) or a `.`-prefixed JSON path
  (`-x .ts`). Default: the row number (0, 1, 2, …). An RFC3339 timestamp
  is read as a time on its own; anything else is a plain number or label
  unless cast:
  - `::time`: x is a time, RFC3339 or epoch seconds (`-x 1::time`).
  - `::time(<format>)`: x is a time in a strftime format: `%Y` year,
    `%m` month, `%d` day, `%H` hour, `%M` minute, `%S` second, `%b`
    month name, `%z` UTC offset, `%s` epoch seconds, and the rest of the
    usual `date +FORMAT` codes. A stamp with no zone is read as UTC. A row
    that doesn't match the format stops the chart with an error.
  - With a time cast, a column range `<start>-<end>` joins those columns
    with a space before parsing, for stamps that span two fields:
    `-x '1-2::time(%Y/%m/%d %H:%M:%S)'`.
- `-y <field>`: the y field, same grammar as `-x`. Default: the last
  whitespace field of each line. Casts are only supported on `-x`.
- `-a`, `--aggregate` `<fn>`: how to combine the y values that share an x.
  One of `count`, `sum`, `avg`, `min`, `max`, `p50`, `p95`, `identity`.
  Default `identity`, or `count` when `--bucket` is set.
- `--tag <field>`: draw one line per distinct value of `<field>`, such as
  one per host. Same field grammar as `-x` and `-y`. Only valid with
  `-t line` (the default when `--tag` is set). Each line gets its own color
  and a legend row.
- `--bucket <DURATION>`: group rows into time buckets on x. x must parse as
  RFC3339, epoch seconds, or the `-x` cast's format. With `-a count` (the
  default) or `sum`, a bucket with no rows is drawn as 0, so a stretch with
  no logs shows as a hole.
- `-t`, `--type` `<line|sparkline|bar>`: chart kind. Default `bar`, or
  `line` when `--bucket` or `--tag` is set.
- `--title <text>`: chart title.

`-x`, `-y` and `--tag` must all be columns or all be JSON paths. `--tag`
needs one JSON object per line, so use `jq -c`.

### chart options for crons

- `-t`, `--type` `<line|sparkline|bar>`: chart kind. Default `line`.
  `sparkline` is a single-line ramp; `bar` draws one bar per run with its
  timestamp under it.
- `-n`, `--limit` `<N>`: how many recent runs to draw. Default 50.
- `-f`, `--follow`: redraw until you press **q**.
- `-i`, `--interval` `<DURATION>`: redraw interval with `--follow`.
  Default `1s`, minimum `100ms`.

### chart examples

```
# One bar per line: each line's last field.
cat /log/myco/prod/api/host.log | jq '.memory.pct' | chart

# Error lines per five minutes. The stamp "2026/10/13 15:04:05" spans
# columns 1 and 2; an empty bucket draws as 0.
cat /log/myco/prod/api/main.log | grep ERROR \
  | chart -x '1-2::time(%Y/%m/%d %H:%M:%S)' --bucket 5m

# 95th-percentile memory use per minute, from JSON fields.
cat /log/myco/prod/api/host.log \
  | jq -c '{ts: $ts, pct: .memory.pct}' \
  | chart -x .ts -y .pct --bucket 1m -a p95

# One bar per host: average memory use.
cat /log/myco/prod/api/host.log | jq -c '{host: $host, pct: .memory.pct}' \
  | chart -x .host -y .pct -a avg

# One line per host over time.
cat /log/myco/prod/api/host.log \
  | jq -c '{ts: $ts, host: $host, pct: .memory.pct}' \
  | chart -x .ts -y .pct --tag .host --bucket 1m -a avg
```

To keep the errors-per-five-minutes chart, save it as a cron that runs
every five minutes and prints that interval's count. Each run sees only the
five minutes since the last one (see [Crons](crons.md#scheduling)), so the
cron drops the `--bucket` and the timestamp field; each run's count becomes
one point. Create it with `vi /cron.d/error-rate`:

```
# schedule: */5 * * * *
cat /log/myco/prod/api/main.log | grep ERROR | wc -l
```

Once it has run a few times, chart it by name:

```
chart error-rate                  line chart of the last 50 runs
chart error-rate -n 200           the last 200 runs
chart error-rate -t sparkline     a one-line glyph
chart error-rate --follow         redraw every second; press q to exit
```

`ls -l /cron.d/` shows a small sparkline next to each cron, so you can scan
every cron at once.

### chart limitations

- Chart kinds other than `line`, `sparkline` and `bar`: no pie, area or
  table.
- Aggregates other than `count`, `sum`, `avg`, `min`, `max`, `p50`, `p95`
  and `identity` (no `p99`).
- `--follow`, `-i` and `-n` in pipe form, and `-x`, `-y`, `-a`, `--bucket`,
  `--tag` and `--title` in cron form.
- `--tag` with `bar` or `sparkline`; it draws one line per value, so it
  needs `line`.
- Casts on `-y` or `--tag`, and a `-x` column range without `::time`.
- Mixing column numbers and `.json.path` fields in one call.
- Charting a cron whose output is JSON rows (a dashboard can, with
  [`tag:`](#multi-line-sparklines-tag)), and piping the output of `chart`
  into another command.
- `--flag=value` and attached short forms (`--type=bar`, `-n5`). Write the
  flag and its value as separate words.
- Zoom and scroll.

Other options are rejected with `chart: unknown option <flag>`.

## Put charts on a dashboard

A dashboard arranges crons into tiles. Each tile names a cron and how to
draw it: a sparkline of its history, its latest value as a counter or
gauge, or its last output as a table.

Dashboards are YAML files under `/etc/dashboards/`. Writing under `/etc`
requires the `configmgr` group. Create one with `vi`:

```
vi /etc/dashboards/prod.yaml
```

```yaml
title: Production
tiles:
  - kind: sparkline
    cron: error-rate
    title: Errors per 5 minutes
    threshold: { warn: 50, crit: 200 }
```

Every `cron:` must name a cron that already exists (`ls /cron.d`); a
dashboard that names an unknown cron is rejected when you save it.

Open it in the terminal:

```
dashboard prod
```

It also appears on the **Dashboards** page of the web app. You can have as
many dashboards as you like, each in its own file. When one grows, group
its tiles into sections; [Dashboard YAML](#dashboard-yaml) has a full
example and every key.

## The dashboard command

### dashboard synopsis

```
dashboard
dashboard <name>
```

### Opening a dashboard

With no arguments, `dashboard` lists your dashboards. With a name, it opens
`/etc/dashboards/<name>.yaml` full screen. Arrow keys move between tiles,
**Enter** zooms the selected tile to full screen, and **Esc** leaves the
zoom or the dashboard.

On the **Dashboards** page of the web app, each tile's header links to its
cron's page, which shows the full run history with zoom, range selection
and a per-tag legend.

### dashboard limitations

- Creating or editing a dashboard with the command; use `vi` on
  `/etc/dashboards/<name>.yaml`.
- Options (`dashboard -x`) and more than one name. Both are rejected with
  `dashboard: unknown option` or `dashboard: unexpected argument`.
- Tile kinds other than `sparkline`, `counter`, `gauge` and `table`, and
  tiles fed by anything but a cron. Keys not listed in
  [Dashboard YAML](#dashboard-yaml) are rejected when the file is saved.
- Mixing `sections:` and top-level `tiles:` in one file.

## Dashboard YAML

A dashboard with sections:

```yaml
title: Production
sections:
  - title: Errors
    tiles:
      - kind: sparkline
        cron: error-rate
        size: md

      - kind: table
        cron: top-errors
        columns: [host, count]
        size: lg

  - title: Capacity
    tiles:
      - kind: counter
        cron: pending-jobs
        format: "%d jobs"
        threshold: { warn: 100, crit: 500 }
        size: sm

      - kind: gauge
        cron: cpu-usage
        threshold: { warn: 70, crit: 90, max: 100 }
        size: md
```

Each cron here has to exist first. A `table` tile shows the cron's last
output split into whitespace-separated columns, so `top-errors` might be:

```
# schedule: */5 * * * *
cat /log/myco/prod/api/main.log | grep ERROR | jq -r '$host' \
  | sort | uniq -c | sort -rn | awk '{print $2, $1}'
```

### Top-level fields

| Field      | Required | Notes                                           |
|------------|----------|-------------------------------------------------|
| `title`    | no       | Heading at the top of the dashboard.            |
| `sections` | one of   | List of sections, each with an optional `title` and a required `tiles` list. |
| `tiles`    | one of   | A flat tile list: one untitled section.         |

Use either `sections:` or a top-level `tiles:`, not both.

### Tile kinds

| Kind        | Shows                                                      | Required fields           |
|-------------|------------------------------------------------------------|---------------------------|
| `sparkline` | The cron's recent results over time.                       | `cron`                    |
| `counter`   | The latest numeric value.                                  | `cron`                    |
| `gauge`     | The latest value as a bar from 0 to `threshold.max` (default 100). | `cron`            |
| `table`     | The last output, split into whitespace-separated columns.  | `cron`, `columns`         |

### Tile fields

| Field        | Applies to                | Notes                                                        |
|--------------|---------------------------|--------------------------------------------------------------|
| `kind`       | all                       | One of `sparkline`, `counter`, `gauge`, `table`.             |
| `cron`       | all                       | Name of an existing cron (must exist when the file is saved). |
| `title`      | all                       | Optional label for the tile.                                 |
| `size`       | all                       | `sm`, `md`, or `lg`. Default `md`. See [Layout](#layout).    |
| `format`     | counter                   | Text with exactly one `%d` (rounded integer) or `%.Nf` (N decimals); `%%` is a literal `%`. E.g. `"%d ms"`, `"%.1f%%"`. |
| `threshold`  | counter, gauge, sparkline | `warn`, `crit` cutoffs; `max` (gauge) sets full scale.       |
| `columns`    | table                     | Header labels for the whitespace-separated columns.          |
| `tag`, `y`   | sparkline                 | Multi-line mode, see below. Set both or neither.             |

A field on a kind it doesn't apply to is rejected when the file is saved
(`format` on a gauge, `columns` on a counter, `threshold` on a table).

### Threshold colors

Values at or above `crit` render red; values at or above `warn` (and below
`crit`) render yellow. A gauge also uses `max` as its full-scale value
(default 100).

### Multi-line sparklines (`tag:`)

A plain sparkline plots one number per cron run, the same as
`chart <cron>`. When a cron prints several JSON rows per run, one per
host or instance, set `tag:` to the field that names the row and `y:` to
the field to plot. The tile draws one line per tag value:

```yaml
- kind: sparkline
  cron: coordinator-mem-pct
  tag: .coordinator_id
  y: .mem_pct
  size: lg
```

Each row's x value is the time of the run that printed it. Lines get a
legend and a distinct color per tag.

## Layout

Tiles appear in YAML order, flowing left to right and then down. `size:`
is a hint that each screen interprets for its own width:

- **Browser**: a 6-column grid on wide screens, 2 on a tablet and 1 on a
  phone. `sm` spans 2 columns, `md` 3, and `lg` all 6.
- **Terminal**: tiles pack into rows as they fit. Below about 60 columns,
  each tile gets its own row.

## Validation

Saving a dashboard checks that the YAML parses, every `kind:` and `size:`
is valid, every `table` tile has `columns:`, and every `cron:` exists. A
bad file is rejected when you save it, and the previous version stays in
place.

## Delete a dashboard

```
rm /etc/dashboards/old.yaml
```

Deleting a dashboard leaves its crons running; remove them from
`/cron.d` separately if you no longer need them.

## Authoring with an AI assistant

Connect Claude Code (or any MCP host) to the weft MCP server and the
assistant can draft dashboards for you to review before they take effect:

| Tool                    | Effect                                                   |
|-------------------------|----------------------------------------------------------|
| `weft.dashboard_propose`| Stage a new or updated dashboard and show a diff against the current version. |
| `weft.proposal_delete`  | Stage a delete with `kind: dashboard, name: <n>`.        |
| `weft.proposals_apply`  | Apply staged proposals once you've reviewed them.        |

Staged dashboards go through the same checks as a saved file, including
that every cron exists.
