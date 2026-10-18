---
id: man-alert
title: alert
shell:
  const: ManAlert
  topics:
    - "man alert"
    - "alert --help"
  banner: "ALERT(1)"
---

## NAME

alert — evaluate conditions and dispatch notifications

## SYNOPSIS

```
<pipeline> | alert [<condition>] [--notify=<alias>]
                   [-k <field>] [-t|--tag <field>]
                   [--duration <dur>] [--average] [--msg <text>]

<pipeline> | alert (--if-zero <v> | --if-nonzero <v>) [--notify=<alias>]
```

## DESCRIPTION

Reads a value from stdin and evaluates it against a condition.
If the condition is met, writes an alert message to stderr and,
when `--notify` is supplied, dispatches the alias from `/etc/notify.yaml`
to its Slack, PagerDuty, webhook, and/or email targets.

If no condition is given, the default is "any": the alert fires whenever
stdin is non-empty. `alert` alone is a dry run that writes to stderr.

When the condition is not met, stdin is passed through to stdout
unchanged. When it is met, the alert message goes to stderr.

`alert` must be the last stage of its pipeline, and the last (or only)
command of a script.

If the pipeline ahead of `alert` fails (an unknown source, a bad stage),
the alert is not evaluated. The shell shows the error, and a cron run is
recorded with status `error` and opens no incident. A pipeline that runs
fine and prints nothing (a `grep` with no match) gives `alert` empty input,
and `absent` fires on it.

In a cron (`/cron.d/`), incidents are deduplicated on
(cron, condition). Later ticks during an open incident
update it and send no new page. When the condition clears, the
incident closes.

From the interactive shell, alert is the supported way to
test an alias end-to-end:

```
echo "hello" | alert --notify=ops
```

Without `--notify`, alert writes to stderr (dry-run mode).

## MULTI-ROW MODE

By default alert treats stdin as a single scalar value per tick.
With `-k`, each non-blank line of stdin is evaluated independently,
and `-k <field>` picks the value field. With `-t <field>` (a.k.a.
`--tag`), another field names the row; each distinct tag opens
and closes incidents, and dispatches notifications, independently.
Use `-t` whenever the pipeline can produce more than one row per
tick; rows without a tag share one incident.

`<field>` is one of:

- A 1-indexed whitespace column (`-k 2` → second whitespace-split
  field). Lines are split on whitespace; pre-format with awk/sed/jq
  so each row is one line of space-separated values.
- A `.`-prefixed JSON path (`-k .latency_ms`, `-k .req.host`). Each
  line is parsed as a single JSON value. Useful when the upstream
  is `jq` emitting JSONL.

A single alert may not mix the two forms across `-k` and `-t`.

## CONDITIONS

- `any` — fires whenever stdin is non-empty (the default when `--notify` is given)
- `above <N>` — current value is greater than N
- `below <N>` — current value is less than N
- `eq <N>` — current value equals N
- `neq <N>` — current value does not equal N
- `changed` — value differs from previous run
- `absent` — no input received in the window
- `rising <N>%` — value increased by N% compared to previous/window average
- `falling <N>%` — value decreased by N% compared to previous/window average

## COMPOSITE PREDICATES

A cron that needs to fire on the state of more than one cron composes
predicate builtins (`weft::is_above`, `weft::is_absent`, etc. — see
`man weft-funcs`) with bash `&&` `||` `!` inside a subshell. Ending the
subshell with `echo` makes it print something only when the whole
condition holds, and alert's default `any` condition fires on that:

```
# /cron.d/deploy-danger
# schedule: every 1m
(weft::is_above error-rate 50 && weft::is_absent canary-ping && echo fire) | alert --notify=critical
```

To combine conditions on the logs themselves, count each in one pass
over the cron's time range and alert on the result (see "Alerting
strategies").

`--if-zero <value>` fires when the value is "0" (the convention bash uses
for *success*, matching the right-hand interpretation of `&&` chains).
`--if-nonzero <value>` is the inverse. The two are mutually exclusive
with the stdin condition keywords above.

See the **Alerting strategies** manual page (`man alerting-strategies`) for the full pattern
catalogue (single-cron scalar, multi-row per-tag, composite,
aggregated-value).

## OPTIONS

- `--notify=<alias>` — dispatch to the named alias in `/etc/notify.yaml`. Without `--notify`, alert writes to stderr.
- `--if-zero <value>` / `--if-nonzero <value>` — fire based on a literal exit-code value (typically `"$?"` from the prior compound). Mutually exclusive with stdin condition keywords.
- `-k <field>` — multi-row mode: each line of stdin is one row, and `<field>` selects the numeric value. `<field>` is either a 1-indexed whitespace column (e.g. `2`) or a `.`-prefixed JSON path against the line parsed as JSON (e.g. `.req.latency_ms`).
- `-t <field>`, `--tag <field>` — multi-row mode: `<field>` names this row for per-tag incident dedup and per-tag notifications. Same field grammar as `-k`. Must agree with `-k` on positional vs JSON-path. Required when the pipeline can produce more than one row per tick.
- `--duration <dur>` — time window for sustained conditions. For above/below: condition must hold for ALL runs in the window. For rising/falling: compares against the window average. For absent: no non-empty output in the window. Units are s, m and h only: 5m, 1h, 30s (`1d` is rejected).
- `--average` — for above/below with `--duration`: compare the rolling average over the window against the threshold. It has no effect without `--duration`.
- `--msg <text>` — override the default alert message.

## EXAMPLES

```
# Fire if the range holds any ERROR line.
cat /log/myco/prod/api/main.log | grep ERROR | alert --notify=ops

# Fire when the range holds more than 50 errors.
cat /log/myco/prod/api/main.log | grep ERROR | wc -l | alert above 50 --notify=critical

# check_errors stands for a script of yours in /bin that prints one number.
check_errors | alert above 50 --duration 5m --average --notify=ops
check_errors | alert rising 100% --notify=critical

# Fire when the range holds no canary line at all.
cat /log/myco/prod/canary/main.log | alert absent --notify=ops

echo "integration test" | alert --notify=ops

# Multi-row positional: alert per host whose disk use is above 80%.
cat /log/myco/prod/api/disk.log \
    | jq -r '"\(.host) \(.pct)"' \
    | alert above 80 -k 2 -t 1 --notify=ops

# Multi-row JSON path: same idea with JSON rows. Each line of disk.log
# is already one row.
cat /log/myco/prod/api/disk.log \
    | alert above 80 -k .pct -t .host --notify=ops
```

In a cron, count over the cron's own time range. A row limit with
`head -n` or `tail -n` can silently truncate the window.

## SEE ALSO

`help`, `help alerts`, `help cron`, `man weft-funcs`, `man range`, `vi`, `cat`
