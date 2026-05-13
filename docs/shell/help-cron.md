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
- `ls /cron.d` — list existing watches; `ls -l /cron.d/` adds a sparkline column
- `chart <name>` — chart recent results (`man chart`)
- `rm /cron.d/<name>` — delete a watch (stops the schedule)

## Watch file format

```
# schedule: every 1m          (or cron: */5 * * * *)
# notify: ops                 (alias from /etc/notify.yaml)
cat /log/api.log | grep ERROR | wc -l
```

## Accessing past results

`weft::latest`, `weft::avg`, `weft::max`, `weft::min`, `weft::sum`,
`weft::stddev`, and the rest of the `weft::*` family read this
watch's history. There's also a parallel family of predicates
(`weft::is_above`, `weft::is_outlier`, etc.) that return exit codes
for composing alerts. Full reference: `man weft`.

```
echo $(weft::avg error-rate 1h) | alert above 50 --notify=ops
```

## Tier limits

{{TIER_LIMITS}}
