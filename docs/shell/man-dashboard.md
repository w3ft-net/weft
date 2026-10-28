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

{{include ../manual/dashboards.md#dashboard-synopsis}}

## DESCRIPTION

Dashboards are YAML files under `/etc/dashboards/`. Create one with
`vi /etc/dashboards/<name>.yaml`; writing under `/etc` needs the
`configmgr` group.

## YAML GRAMMAR

{{include ../manual/dashboards.md#dashboard-yaml}}

## LAYOUT

{{include ../manual/dashboards.md#layout}}

## EXAMPLES

```
dashboard                   list dashboards
dashboard ops               open the "ops" dashboard (create it first)
vi /etc/dashboards/ops.yaml create or edit it
rm /etc/dashboards/ops.yaml delete it (its crons keep running)
```
## SEE ALSO

`help cron`, `man chart`, and [Dashboards](../manual/dashboards.md) for a walkthrough from one-off charts to dashboards
