---
id: manual-dashboards
title: Dashboards
web:
  section: dashboards
  category: alert-visualize
  order: 55
---

## What dashboards are

A dashboard is a named view that arranges existing watches into tiles.
Each tile reads from a watch — there are no pipelines, no template
variables, and no time-range pickers. The watch's schedule and history
define what the tile shows; the dashboard just composes them into a
layout.

Dashboards live as YAML files under `/etc/dashboards/`. Create one from
the shell:

```
vi /etc/dashboards/prod.yaml
```

Each customer can have any number of dashboards, distinguished by name.

## YAML grammar

```yaml
title: Production
sections:
  - title: Errors
    tiles:
      - kind: sparkline
        watch: error-rate-hourly
        size: md

      - kind: table
        watch: top-errors-hourly
        columns: [host, count]
        size: lg

  - title: Capacity
    tiles:
      - kind: counter
        watch: pending-jobs
        format: "%d jobs"
        threshold: { warn: 100, crit: 500 }
        size: sm

      - kind: gauge
        watch: cpu-usage
        threshold: { warn: 70, crit: 90, max: 100 }
        size: md
```

Top-level fields:

| Field      | Required | Notes                                           |
|------------|----------|-------------------------------------------------|
| `title`    | no       | Heading at the top of the dashboard.            |
| `sections` | yes\*    | List of grouped tile sections.                  |
| `tiles`    | yes\*    | Shorthand: a flat tile list = one untitled section. |

\* Use either `sections:` or top-level `tiles:`, not both.

## Tile kinds

| Kind        | Body                                                       | Required fields           |
|-------------|------------------------------------------------------------|---------------------------|
| `sparkline` | Time-series glyph from the watch's recent results.         | `watch`                   |
| `counter`   | Latest numeric value.                                      | `watch`                   |
| `gauge`     | Bar from 0 to `threshold.max` (default 100).               | `watch`                   |
| `table`     | Multi-row stdout parsed as whitespace-split columns.       | `watch`, `columns`        |

Tile fields:

| Field        | Applies to                | Notes                                                        |
|--------------|---------------------------|--------------------------------------------------------------|
| `kind`       | all                       | One of `sparkline`, `counter`, `gauge`, `table`.             |
| `watch`      | all                       | Name of an existing watch (must exist when the file is saved). |
| `size`       | all                       | `sm`, `md`, or `lg`. Default `md`. Hint, not pixel size.     |
| `format`     | counter                   | Optional `%d` / `%.2f` format applied to the value.          |
| `threshold`  | counter, gauge, sparkline | `warn`, `crit`, `max` numeric cutoffs.                       |
| `columns`    | table                     | Header labels; columns parse as whitespace-split fields.     |

The `size:` field is a hint, not a pixel measurement. Each renderer
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

Each section gets its own heading + grid. Sections are how you split a
dashboard into "Errors" / "Capacity" / "Throughput" rather than dumping
every tile into one strip.

If you don't need sections, use a flat top-level `tiles:` list:

```yaml
title: Quick view
tiles:
  - kind: counter
    watch: pending-jobs
  - kind: sparkline
    watch: error-rate
```

## Layout

Dashboards describe *intent*, not pixel-perfect layouts. The renderer
flows tiles row-major and chooses how many fit per row based on the
viewport and each tile's `size:` hint.

- **Browser**: CSS grid with 6 tracks on wide screens, collapsing to 2
  on tablet and 1 on phone. `sm` spans 2 tracks, `md` spans 3, `lg`
  spans 6.
- **Terminal**: greedy row packing using kind-specific minimum widths.
  Below ~60 columns the layout collapses to one tile per row.

There is no drag-and-drop and no per-tile pixel positioning. The order
in YAML is the order on screen.

## Navigation

Tile headers link to the watch's detail page (`/watch/<name>`) so
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
`columns:`, and every referenced watch exists. A bad file is rejected
at write time — your previous version stays intact.

## Authoring via the MCP server

Connect Claude Code (or any MCP host) to the weft MCP server and the
model can stage dashboards through the same review-and-apply pipeline
as scripts and crons:

| Tool                    | Effect                                                   |
|-------------------------|----------------------------------------------------------|
| `weft.dashboard_propose`| Stage a new or updated dashboard. Validates YAML + watch refs. Returns a unified diff against any existing version. |
| `weft.proposal_delete`  | Stage a delete with `kind: dashboard, name: <n>`.        |
| `weft.proposals_apply`  | Commit one or more staged proposals after human review.  |

Watch references are checked against the live watch list — the model
can't stage a dashboard pointing at a watch that doesn't exist yet.
