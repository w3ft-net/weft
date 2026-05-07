# Plan: Dashboard builder

Multiple named dashboards composed of tiles, where each tile references an existing watch. Not a "tile = arbitrary query" engine — that route collides with the "don't add a stats DSL, use watches" direction. Tiles reuse the watch primitive that already exists.

## Why add this

Today's Dashboard page (`frontend/src/pages/Dashboard.tsx`) is a fixed strip: one list of watch sparklines, one list of functions, one ingestion-status card. One dashboard, no layout choice, no per-environment split. Every customer with more than a handful of watches ends up scrolling through a mixed bag of prod and staging and one-off experiment watches.

The fix isn't a full BI product. It's two things: (1) let the customer name more than one dashboard, (2) let them pick which watches go on which dashboard and in what order. That's enough to recover the "prod ops dashboard vs. staging sanity dashboard vs. on-call overview" use case that everyone wants.

## Architecture

Dashboards are shell-editable YAML files under `/etc/dashboards/`:

```yaml
# /etc/dashboards/prod.yaml
title: Production

sections:
  - title: Errors
    tiles:
      - kind: sparkline
        watch: error-rate-hourly
        size: md

      - kind: table
        watch: top-errors-hourly   # multi-row watch stdout → table
        columns: [host, count]
        size: lg

  - title: Capacity
    tiles:
      - kind: counter
        watch: pending-jobs
        format: "%d jobs"
        threshold: { warn: 100, crit: 500 }
        size: sm

      - kind: sparkline
        watch: disk-pct
        size: md
```

The YAML describes *intent*, not layout. Tiles carry a `size:` hint (`sm`/`md`/`lg`) and the renderer decides how many fit per row at the current viewport. `sections:` group tiles under a heading; ordering within a section is preserved. A flat `tiles:` list at the top level is shorthand for one untitled section.

**Browser** uses CSS grid auto-flow. Each section is a `<section>` with an `<h2>` and its own grid; size hints map to `col-span` at the current breakpoint (e.g. `sm`=1, `md`=2, `lg`=3 of a 6-track grid on wide screens; everything collapses to 1 column on narrow).

**Terminal** packs left-to-right, wrapping when remaining width drops below the tile kind's minimum. Below a width threshold (TBD, ~120 cols) the layout collapses to single column. Sections render as a heading + rule, then tiles, then a blank line — and serve as natural pagination boundaries.

No drag-and-drop, no per-tile positioning. The author orders tiles in YAML; the renderer flows them.

Four tile kinds cover the observed demand:

- **sparkline** — time-series chart of the watch's numeric result (what today's dashboard already does).
- **counter** — single-number readout of `weft.latest(watch)` with optional `warn`/`crit` coloring.
- **gauge** — numeric current-value with a 0-max range and a colored arc.
- **table** — parses the watch's stdout as whitespace-split rows, renders as a data table. Relies on the multi-row alert convention the manual already teaches.

All tile rendering goes through the same chart code `Watch.tsx` already has. The backend surface is "list dashboards for this customer" + "read a dashboard" — no new compute.

### Terminal: charts at a fraction of the screen

The chart renderer already accepts arbitrary `(w, h)` — `chart/builtin.go` just happens to pass the full terminal size today. What's missing is a *layout host* that allocates sub-rectangles for each tile and asks the chart package to render into them.

For v1 the host is dumb: walk the section's tiles in order, ask each for a desired width based on its `size:` hint and a kind-specific minimum (sparkline ≥ 24 cols, counter ≥ 12, gauge ≥ 16, table = sum of column widths), pack them into rows, and emit each tile's render output side-by-side via line-wise interleaving into scrollback. No panel framework, no live redraw of the whole dashboard — `chart --follow` keeps working as today for a single chart, and the dashboard view re-renders on demand.

## Components to build

1. **`store.Dashboard`** — `{customer_id, name, yaml_body, updated_at}`. Name unique per customer.
2. **Shell filesystem integration** — `/etc/dashboards/` is a real path in `shellfs/`. `ls /etc/dashboards`, `vi /etc/dashboards/prod.yaml`, `rm /etc/dashboards/stale.yaml` all work. Save-time validation checks that every referenced `watch:` exists, `kind:` is known, `size:` is one of the allowed hints, and the YAML parses.
3. **API**:
   - `GET /api/dashboards` — list by name.
   - `GET /api/dashboards/{name}` — returns parsed + validated dashboard spec plus the current values for every referenced watch in one round-trip.
4. **Frontend**:
   - `Dashboards.tsx` route with sidebar of dashboard names.
   - `DashboardSection` component renders an `<h2>` and a CSS-grid container.
   - Tile components: `SparklineTile`, `CounterTile`, `GaugeTile`, `TableTile` — each takes a watch name + the common tile config.
   - Auto-flow layout from per-tile `size:` mapped to `col-span` at the current breakpoint.
5. **Terminal layout host** — new `dashboard/render.go` (or similar) that walks the parsed spec and produces scrollback output: per-section heading + rule, then row-packed tiles using the `(w, h)` API the chart package already exposes. Single-column fallback below a width threshold.
6. **Docs** — `docs/manual/dashboards.md` with YAML grammar, tile recipes, sections.

## Deliberately simple choices

- **YAML describes intent, not layout.** No `grid:`, no `span:`, no pixel coords. Each renderer (browser / terminal) decides the actual layout from the tile's `size:` hint and the available viewport. Drag-and-drop adds a lot of frontend state management to save a medium amount of typing. Defer until a customer asks.
- **Tile data source is a watch, never a raw pipeline.** Keeps the compute path narrow — the coordinator never re-executes a pipeline on dashboard load, just reads cron results. Matches `docs/plans/ingest-dimensions.md` philosophy.
- **No template variables, no tile-level time-range picker.** Each watch's schedule defines its bucket; the dashboard takes what the watch emits. A per-tile time range would mean re-querying on dashboard load, which we deliberately avoid.
- **No cross-tile linking / click-through.** Each tile is static. Clicking "drill into the error-rate chart" takes you to the existing `Watch.tsx` detail page — no new surface.
- **Terminal layout is best-effort flow, not a real TUI.** Single-column fallback when the terminal is too narrow for a tile's minimum. No panel framework, no live full-dashboard redraw — the bubbletea direction was already rejected for the shell.

## Estimated effort

- `Dashboard` store model + shellfs integration + validation: **0.75 day**
- API handlers + round-trip response shape: **0.5 day**
- Four tile components (re-using `Watch.tsx` chart code): **1.5 days**
- Browser auto-flow layout + sections + sidebar nav: **0.5 day**
- Terminal layout host (row-packing, single-column fallback): **1 day**
- Docs + tests: **0.75 day**

**Total: ~5 days** for a shippable v1.

## Non-goals

- **No multi-query tiles.** Tile = one watch; overlaying two watches on one chart is a feature request for later.
- **No widget marketplace / plugin system.** Four built-in kinds, that's it.
- **No global time-range picker on the dashboard.** Time scope is the watch's history depth, controlled by plan retention.
- **No tile-level alert status.** If the watch has an alert condition, the tile can color-code the current value using the same threshold config; there's no separate "has-firing-incident" badge on the tile in v1.
- **No sharing a dashboard read-only with non-team-members.** Same scope rule as saved queries — team-visible, not public.
- **No auto-seeded default dashboard.** Empty state in the Dashboards page tells the user to run `vi /etc/dashboards/<name>.yaml`. Seeding a default at signup means seeding an empty dashboard (no watches yet); seeding lazily on first list is invisible magic that surprises users when they delete it.
