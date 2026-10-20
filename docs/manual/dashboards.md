---
id: manual-dashboards
title: Dashboards
web:
  section: dashboards
  category: analysis-alerting
  order: 55
---

## What dashboards are

A dashboard is a named view that arranges existing crons into tiles.
Each tile reads from a cron. The cron's schedule and history
define what the tile shows; the dashboard arranges the tiles into a
layout.

Dashboards live as YAML files under `/etc/dashboards/`. Create one from
the shell:

```
vi /etc/dashboards/prod.yaml
```

Writing under `/etc` requires the `configmgr` group.

Each customer can have any number of dashboards, distinguished by name.

## YAML grammar

```yaml
title: Production
sections:
  - title: Errors
    tiles:
      - kind: sparkline
        cron: error-rate-hourly
        size: md

      - kind: table
        cron: top-errors-hourly
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

Each tile's `cron:` key names a cron. Create the crons first; a dashboard that references an unknown cron is rejected.

Top-level fields:

| Field      | Required | Notes                                           |
|------------|----------|-------------------------------------------------|
| `title`    | no       | Heading at the top of the dashboard.            |
| `sections` | yes\*    | List of grouped tile sections.                  |
| `tiles`    | yes\*    | Shorthand: a flat tile list = one untitled section. |

\* Use one of `sections:` or top-level `tiles:`.

## Tile kinds

| Kind        | Body                                                       | Required fields           |
|-------------|------------------------------------------------------------|---------------------------|
| `sparkline` | Time-series glyph from the cron's recent results.          | `cron`                    |
| `counter`   | Latest numeric value.                                      | `cron`                    |
| `gauge`     | Bar from 0 to `threshold.max` (default 100).               | `cron`                    |
| `table`     | Multi-row stdout parsed as whitespace-split columns.       | `cron`, `columns`         |

Tile fields:

| Field        | Applies to                | Notes                                                        |
|--------------|---------------------------|--------------------------------------------------------------|
| `kind`       | all                       | One of `sparkline`, `counter`, `gauge`, `table`.             |
| `cron`       | all                       | Name of an existing cron (must exist when the file is saved). |
| `title`      | all                       | Optional label for the tile.                                 |
| `size`       | all                       | `sm`, `md`, or `lg`. Default `md`. Layout hint.              |
| `format`     | counter                   | Optional. Text with exactly one `%d` (rounded integer) or `%.2f` (any decimals, `%.Nf`); `%%` is a literal `%`. E.g. `"%d ms"`, `"%.1f%%"`. |
| `threshold`  | counter, gauge, sparkline | `warn`, `crit` numeric cutoffs; `max` (gauge) sets full scale. |
| `columns`    | table                     | Header labels; columns parse as whitespace-split fields.     |
| `tag`, `y`   | sparkline                 | Multi-line mode — see below. Must be set together.           |

A field on a kind it does not apply to is rejected when the file is saved
(`format` on a gauge, `columns` on a counter, `threshold` on a table).

### Multi-line sparklines (`tag:`)

A sparkline plots one scalar per cron run (the cron's last
whitespace field). When a cron's stdout is multiple JSON rows
per run — one per tagged entity, e.g. one row per instance — set `tag:`
(which field identifies the row) and `y:` (which field to plot) to draw
one line per distinct tag value:

```yaml
- kind: sparkline
  cron: coordinator-mem-pct
  tag: .coordinator_id
  y: .mem_pct
  size: lg
```

Each row's x value is its run's timestamp from the cron run history. Lines
get a legend and a distinct color per tag; `tag:`/`y:` are rejected on
any kind other than `sparkline`.

The `size:` field is a layout hint. Each renderer
(browser / terminal) maps the hint onto its own viewport. On a wide
browser an `lg` tile spans the full row; on a narrow terminal it falls
back to single-column.

## Threshold colors

`counter`, `gauge`, and `sparkline` accept a `threshold:` map:

```yaml
threshold: { warn: 70, crit: 90 }
```

Values at or above `crit` render in red; values at or above `warn` (and
below `crit`) render in yellow. `gauge` additionally uses
`threshold.max` to pick its full-scale value (default 100).

## Sections

Each section gets its own heading and grid. Sections split a
dashboard into groups such as "Errors", "Capacity" and "Throughput".

For a dashboard without sections, use a flat top-level `tiles:` list:

```yaml
title: Quick view
tiles:
  - kind: counter
    cron: pending-jobs
  - kind: sparkline
    cron: error-rate
```

## Layout

Dashboards describe layout intent. The renderer
flows tiles row-major and chooses how many fit per row based on the
viewport and each tile's `size:` hint.

- **Browser**: CSS grid with 6 tracks on wide screens, collapsing to 2
  on tablet and 1 on phone. `sm` spans 2 tracks, `md` spans 3, `lg`
  spans 6.
- **Terminal**: greedy row packing using kind-specific minimum widths.
  Below ~60 columns the layout collapses to one tile per row.

Tiles are placed by size hint and YAML order. The order
in YAML is the order on screen.

## Navigation

Tile headers link to the cron's detail page (`/cron/<name>`) so
clicking a tile takes you to its full history with zoom, range
selection, and per-tag legend.

## Deletion

Remove a dashboard from the shell:

```
rm /etc/dashboards/old.yaml
```

## Validation

Saving a dashboard checks the YAML parses, every `kind:` is recognized,
every `size:` is one of `sm` / `md` / `lg`, every `table` tile has
`columns:`, and every referenced cron exists. A bad file is rejected
at write time — your previous version stays intact.

## Authoring via the MCP server

Connect Claude Code (or any MCP host) to the weft MCP server and the
model can stage dashboards through the same review-and-apply pipeline
as scripts and crons:

| Tool                    | Effect                                                   |
|-------------------------|----------------------------------------------------------|
| `weft.dashboard_propose`| Stage a new or updated dashboard. Validates YAML + cron refs. Returns a unified diff against any existing version. |
| `weft.proposal_delete`  | Stage a delete with `kind: dashboard, name: <n>`.        |
| `weft.proposals_apply`  | Commit one or more staged proposals after human review.  |

Cron references are checked against the live cron list; a dashboard
that names an unknown cron cannot be staged.
