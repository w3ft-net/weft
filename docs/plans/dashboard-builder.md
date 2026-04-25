# Plan: Dashboard builder

Multiple named dashboards composed of tiles, where each tile references an existing watch. Not a "tile = arbitrary query" engine — that route collides with the "don't add a stats DSL, use watches" direction. Tiles reuse the watch primitive that already exists.

## Why add this

Today's Dashboard page (`frontend/src/pages/Dashboard.tsx`) is a fixed strip: one list of watch sparklines, one list of functions, one ingestion-status card. One dashboard, no layout choice, no per-environment split. Every customer with more than a handful of watches ends up scrolling through a mixed bag of prod and staging and one-off experiment watches.

The fix isn't a full BI product. It's two things: (1) let the customer name more than one dashboard, (2) let them pick which watches go on which dashboard and in what order. That's enough to recover the "prod ops dashboard vs. staging sanity dashboard vs. on-call overview" use case that everyone wants.

## Architecture

Dashboards are shell-editable YAML files under `/etc/dashboards/`:

```yaml
# /etc/dashboards/prod.conf
title: Production
grid: [3]                          # three columns

tiles:
  - kind: sparkline
    watch: error-rate-hourly
    span: 1                        # one grid column

  - kind: counter
    watch: pending-jobs
    format: "%d jobs"
    threshold: { warn: 100, crit: 500 }

  - kind: table
    watch: top-errors-hourly       # multi-row watch stdout → table
    columns: [host, count]

  - kind: sparkline
    watch: disk-pct
    span: 2                        # takes two grid columns
```

Grid is row-major; each tile takes `span` columns; rows auto-wrap. No drag-and-drop positioning in v1 — the YAML is the layout.

Four tile kinds cover the observed demand:

- **sparkline** — time-series chart of the watch's numeric result (what today's dashboard already does).
- **counter** — single-number readout of `weft.latest(watch)` with optional `warn`/`crit` coloring.
- **gauge** — numeric current-value with a 0-max range and a colored arc.
- **table** — parses the watch's stdout as whitespace-split rows, renders as a data table. Relies on the multi-row alert convention the manual already teaches.

All tile rendering goes through the same chart code `Watch.tsx` already has. The backend surface is "list dashboards for this customer" + "read a dashboard" — no new compute.

## Components to build

1. **`store.Dashboard`** — `{customer_id, name, yaml_body, updated_at}`. Name unique per customer.
2. **Shell filesystem integration** — `/etc/dashboards/` is a real path in `shellfs/`. `ls /etc/dashboards`, `vi /etc/dashboards/prod.conf`, `rm /etc/dashboards/stale.conf` all work. Save-time validation checks that every referenced `watch:` exists, `kind:` is known, and the YAML parses.
3. **API**:
   - `GET /api/dashboards` — list by name.
   - `GET /api/dashboards/{name}` — returns parsed + validated dashboard spec plus the current values for every referenced watch in one round-trip.
4. **Frontend**:
   - `Dashboards.tsx` route with sidebar of dashboard names.
   - Tile components: `SparklineTile`, `CounterTile`, `GaugeTile`, `TableTile` — each takes a watch name + the common tile config.
   - Grid layout from the parsed `grid:` + per-tile `span:`.
5. **Default dashboard** — on first login, seed a `/etc/dashboards/default.conf` that mirrors today's strip so existing users see no regression.
6. **Docs** — `docs/manual/dashboards.md` with YAML grammar, tile recipes, and a "migrate from the default dashboard" note.

## Deliberately simple choices

- **Layout is YAML, not drag-and-drop.** Drag-and-drop adds a lot of frontend state management to save a medium amount of typing. Defer until a customer asks.
- **Tile data source is a watch, never a raw pipeline.** Keeps the compute path narrow — the coordinator never re-executes a pipeline on dashboard load, just reads cron results. Matches `docs/plans/ingest-dimensions.md` philosophy.
- **No template variables, no tile-level time-range picker.** Each watch's schedule defines its bucket; the dashboard takes what the watch emits. A per-tile time range would mean re-querying on dashboard load, which we deliberately avoid.
- **No cross-tile linking / click-through.** Each tile is static. Clicking "drill into the error-rate chart" takes you to the existing `Watch.tsx` detail page — no new surface.

## Estimated effort

- `Dashboard` store model + shellfs integration + validation: **0.75 day**
- API handlers + round-trip response shape: **0.5 day**
- Four tile components (re-using `Watch.tsx` chart code): **1.5 days**
- Grid layout + sidebar nav: **0.5 day**
- Default dashboard seed + migration: **0.25 day**
- Docs + tests: **0.75 day**

**Total: ~4 days** for a shippable v1.

## Non-goals

- **No multi-query tiles.** Tile = one watch; overlaying two watches on one chart is a feature request for later.
- **No widget marketplace / plugin system.** Four built-in kinds, that's it.
- **No global time-range picker on the dashboard.** Time scope is the watch's history depth, controlled by plan retention.
- **No tile-level alert status.** If the watch has an alert condition, the tile can color-code the current value using the same threshold config; there's no separate "has-firing-incident" badge on the tile in v1.
- **No sharing a dashboard read-only with non-team-members.** Same scope rule as saved queries — team-visible, not public.
