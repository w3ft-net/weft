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
now.

Each side of the range can be:

### ISO TIMESTAMP (any resolution)

```
2026-04-12T10:30:00Z    full RFC3339
2026-04-12T10:30        minute resolution
2026-04-12              day resolution
2026-04                 month resolution
```

### RELATIVE DURATION

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
range 2026-04-01..2026-04-12
                        specific date range
range 2026-04-12T10:00..2026-04-12T11:00
                        specific hour
```
