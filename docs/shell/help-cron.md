---
id: help-cron
title: Crons
shell:
  const: HelpCron
  topics:
    - "help cron"
---

## Crons

A cron runs a pipeline on a schedule. Each line of its stdout is
stored as `<value> [<tag>]`: a number, optionally followed by a tag
such as a host name. Output in any other shape disables the cron, as
do three runs in a row that time out. Writes to stderr are
treated as alert firings (if the cron has a `| alert` stage — see
`help alerts`).

- `vi /cron.d/<namespace>/<name>` — create or edit a cron
- `cat /cron.d/<namespace>/<name>` — view a cron definition
- `ls /cron.d` — list a directory for each namespace you can read;
  `ls -l /cron.d/<namespace>/` lists its crons
- `chart <namespace>/<name>` — chart its results over the current range (`man chart`)
- `mv /cron.d/<name> /cron.d/<namespace>/<name>` — move a cron into a
  namespace, keeping its history
- `rm /cron.d/<namespace>/<name>` — delete a cron (stops the schedule)

## Cron file format

```
# schedule: every 1m
cat /log/myco/prod/api/main.log | grep ERROR | wc -l | alert above 50 --notify=ops
```

`# schedule:` takes `every <duration>` or a five-field expression such
as `*/5 * * * *`. Notification targets come from the `alert` stage's
`--notify=<alias>`. Each run reads only the logs from its own
schedule interval, so count with no row limit: `head -n` or `tail -n`
on the input can cut the interval short. Trimming the output, as in
`uniq -c | sort -rn | head -20`, is fine.

A cron that prints JSON names its fields with `# value: .pct` and
`# tag: .host` lines under `# schedule:`; one that alerts uses
`alert -v`/`-t` instead. `# type: text` stores text values rather
than numbers. Full rules: `man crons`.

## Accessing past results

`weft::latest`, `weft::avg`, `weft::max`, `weft::min`, `weft::sum`,
`weft::stddev`, and the rest of the `weft::*` family read a cron's
history; pass the cron as `<namespace>/<name>`, e.g.
`weft::latest myco/error-rate` (a bare name is an error). Windows
are durations like `30m`, `1h` or `1d` (aggregates reach back 24 hours).
The predicates (`weft::is_above`, `weft::is_outlier`, etc.) return
exit codes for composing alerts. Full reference: `man crons`.

```
echo $(weft::avg myco/error-rate 1h) | alert above 50 --notify=ops
```

(Here `myco/error-rate` is one of your own crons.)

## Tier limits

{{TIER_LIMITS}}
