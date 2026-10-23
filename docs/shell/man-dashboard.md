---
id: man-dashboard
title: dashboard
shell:
  const: ManDashboard
  topics:
    - "man dashboard"
    - "dashboard --help"
  banner: "DASHBOARD(1)"
web:
  section: dashboard
  category: command-reference
  order: 72
---

## NAME

dashboard — view a named tile layout in the terminal

## SYNOPSIS

{{include ../manual/dashboards.md#dashboard-synopsis}}

## DESCRIPTION

{{include ../manual/dashboards.md#opening-a-dashboard}}

Dashboards are YAML files under `/etc/dashboards/`. Create one with
`vi /etc/dashboards/<name>.yaml`; writing under `/etc` needs the
`configmgr` group.

## YAML GRAMMAR

{{include ../manual/dashboards.md#dashboard-yaml}}

## LAYOUT

{{include ../manual/dashboards.md#layout}}

## VALIDATION

{{include ../manual/dashboards.md#validation}}

## EXAMPLES

```
dashboard                   list dashboards
dashboard ops               open the "ops" dashboard (create it first)
vi /etc/dashboards/ops.yaml create or edit it
rm /etc/dashboards/ops.yaml delete it (its crons keep running)
```

## NOT SUPPORTED

{{include ../manual/dashboards.md#dashboard-limitations}}

## SEE ALSO

`help cron`, `man chart`, and [Dashboards](../manual/dashboards.md) for a walkthrough from one-off charts to dashboards
