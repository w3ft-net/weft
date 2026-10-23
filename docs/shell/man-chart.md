---
id: man-chart
title: chart
shell:
  const: ManChart
  topics:
    - "man chart"
    - "chart --help"
  banner: "CHART(1)"
web:
  section: chart
  category: command-reference
  order: 71
---

## NAME

chart — render a chart in the terminal, from a pipeline or a cron

## SYNOPSIS

{{include ../manual/dashboards.md#synopsis}}

## DESCRIPTION

{{include ../manual/dashboards.md#how-chart-draws}}

## OPTIONS (pipe form)

{{include ../manual/dashboards.md#chart-options-for-pipelines}}

## OPTIONS (cron form)

{{include ../manual/dashboards.md#chart-options-for-crons}}

## EXAMPLES

{{include ../manual/dashboards.md#chart-examples}}

## NOT SUPPORTED

{{include ../manual/dashboards.md#chart-limitations}}

## SEE ALSO

`help cron`, `man dashboard`, `man range`, and [Dashboards](../manual/dashboards.md) for a walkthrough from one-off charts to dashboards
