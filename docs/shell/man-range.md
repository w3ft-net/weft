---
id: man-range
title: range
shell:
  const: ManRange
  topics:
    - "man range"
    - "range --help"
  banner: "RANGE(1)"
---

## NAME

range — display or set the log query time range

## SYNOPSIS

```
range
range <start>..<end>
range <start>..
```

## DESCRIPTION

With no arguments, range displays the current query time window.

With a `<start>..<end>` argument, range sets the time window used
by all subsequent queries. If `<end>` is omitted, it defaults to
now. Relative specs are re-resolved against the server's clock on
every query — `range 1d..` always means "the last 24 hours".

Each side of the range can be:

### ABSOLUTE TIMESTAMP

A calendar date or instant. Missing components default to the
start of the period: an omitted time means `00:00:00`, an omitted
day means the 1st, an omitted month means January. An omitted
year defaults to the current year, so `01-15` in March means
current-year January.
Server-local timezone unless the literal carries one.

```
2026-04-12T10:30:00Z    full RFC3339 with timezone
2026-04-12T10:30:00     second resolution
2026-04-12T10:30        minute resolution
2026-04-12T10           hour resolution
2026-04-12              day resolution
2026-04                 month resolution
2026                    year resolution
04-12T10:30             current-year shorthand with time
04-12                   current-year shorthand, midnight
```

### RELATIVE DURATION

Distance back from "now". Re-anchored against the server's clock
on every query, so a long-lived shell pane always queries a current
window. Units are `m` (minutes), `h` (hours) and `d` (days);
seconds, weeks and combined forms such as `1h30m` are not accepted.

```
30m                     30 minutes ago
2h                      2 hours ago
7d                      7 days ago
```

### NAMED REFERENCES

```
now                     current time
today                   midnight today
yesterday               midnight yesterday
```

## EXAMPLES

```
range                   show current range
range 2h..              last 2 hours
range 30m..             last 30 minutes
range 7d..              last 7 days
range 1h..10m           from 1 hour ago to 10 minutes ago
range yesterday..today  all of yesterday
range yesterday..       yesterday to now
range 04-01..05-14      current-year span, Apr 1 00:00 up to
                        (excluding) May 14 00:00
range 05-14..           from May 14 midnight to now
range 05-14T09..05-14T17
                        business hours on May 14 (current year)
range 2026-04-01..2026-04-12
                        explicit-year date range
range 2026-04-12T10:00..2026-04-12T11:00
                        specific hour
```
