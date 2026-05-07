---
id: man-dashboard
title: dashboard
shell:
  const: ManDashboard
  topics:
    - "man dashboard"
    - "dashboard --help"
  banner: "DASHBOARD(1)"
---

## NAME

dashboard — view a named tile layout in the terminal

## SYNOPSIS

```
dashboard
dashboard <name>
```

## DESCRIPTION

With no arguments, lists available dashboards.

With a name, enters an interactive full-screen view. Arrow keys
move focus between tiles; Enter zooms the focused tile to full
screen; Esc exits zoom or the view.

Dashboards are YAML files under `/etc/dashboards/`. Create one
with `vi /etc/dashboards/<name>.yaml`.

## YAML GRAMMAR

```
title: Production
sections:
  - title: Errors
    tiles:
      - kind: sparkline
        watch: error-rate
        size: md
      - kind: counter
        watch: pending-jobs
        threshold: { warn: 100, crit: 500 }
        format: "%d jobs"
        size: sm
      - kind: table
        watch: top-errors
        columns: [host, count]
        size: lg
  - title: Capacity
    tiles:
      - kind: gauge
        watch: disk-pct
        threshold: { warn: 80, crit: 95, max: 100 }
```

A flat `tiles:` list (no `sections:`) is shorthand for one untitled
section. You cannot mix `sections:` and top-level `tiles:`.

## TOP-LEVEL KEYS

- `title` — optional heading at the top of the dashboard.
- `sections` — list of tile groups, each with an optional `title` and a `tiles` list.
- `tiles` — shorthand for a single untitled section.

## SECTION KEYS

- `title` — optional heading rendered above the section's tiles.
- `tiles` — required list of tile entries.

## TILE KEYS

- `kind` — one of `sparkline`, `counter`, `gauge`, `table`.
- `watch` — name of an existing watch (must exist when saved).
- `size` — `sm`, `md`, or `lg`. Default `md`. A layout hint, not a pixel size.
- `format` — (counter only) printf-style format, e.g. `"%d jobs"`, `"%.1f%%"`.
- `threshold` — (counter, gauge, sparkline) `warn`, `crit`, and `max` cutoffs.
- `columns` — (table only) header labels; rows are whitespace-split from the watch stdout.

## THRESHOLD KEYS

- `warn` — value at or above this renders yellow.
- `crit` — value at or above this renders red.
- `max` — (gauge only) upper bound of the bar. Default 100.

## EXAMPLES

```
dashboard ops               open the "ops" dashboard
vi /etc/dashboards/ops.yaml edit it
```

## SEE ALSO

help, help cron, man chart
