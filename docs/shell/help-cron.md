---
id: help-cron
title: Watches
shell:
  const: HelpCron
  topics:
    - "help cron"
---

## Watches

A watch runs a pipeline on a schedule. Its stdout is recorded
as a cron result; writes to stderr are treated as alert firings
(if the watch has a `| alert` stage — see `help alerts`).

- `vi /cron.d/<name>` — create or edit a watch
- `cat /cron.d/<name>` — view a watch definition
- `ls /cron.d` — list existing watches
- `rm /cron.d/<name>` — delete a watch (stops the schedule)

## Watch file format

```
# schedule: every 1m          (or cron: */5 * * * *)
# notify: ops                 (alias from /etc/notify.conf)
cat /log/api.log | grep ERROR | wc -l
```

## Accessing past results

- `weft.latest <watch>` — most recent value
- `weft.avg <watch> <duration>` — rolling average over window
- `weft.max <watch> <duration>` — window maximum
- `weft.min <watch> <duration>` — window minimum
- `weft.sum <watch> <duration>` — window sum
- `weft.stddev <watch> <duration>` — window standard deviation

Example:

```
echo $(weft.avg error-rate 1h) | alert above 50 --notify=ops
```

## Tier limits

{{TIER_LIMITS}}
