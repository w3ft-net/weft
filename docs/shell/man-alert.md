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
```

## DESCRIPTION

Reads a value from stdin and evaluates it against a condition.
If the condition is met, writes an alert message to stderr and,
when `--notify` is supplied, dispatches the alias from `/etc/notify.yaml`
to its Slack and/or PagerDuty targets.

If no condition is given, the default is "any": the alert fires
whenever stdin is non-empty.

Always passes stdin through to stdout unchanged (tee behavior).

In a cron watch (`/cron.d/`), incidents are deduplicated on
(watch, condition). Subsequent ticks during an open incident
update it but don't re-page. When the condition clears, the
incident closes.

From the interactive shell, alert is the supported way to
test an alias end-to-end:

```
echo "hello" | alert --notify=ops
```

Without `--notify`, alert only writes to stderr — that is the
dry-run mode.

## MULTI-ROW MODE

By default alert treats stdin as a single scalar value per tick.
With `-k`, each non-blank line of stdin is evaluated independently,
and `-k <field>` picks the value field. With `-t <field>` (a.k.a.
`--tag`), another field names the row; each distinct tag opens
and closes incidents, and dispatches notifications, independently.
Use `-t` whenever the pipeline can produce more than one row per
tick — otherwise multiple rows collapse into the same incident.

`<field>` is one of:

- A 1-indexed whitespace column (`-k 2` → second whitespace-split
  field). Lines are split on whitespace; pre-format with awk/sed/jq
  so each row is one line of space-separated values.
- A `.`-prefixed JSON path (`-k .latency_ms`, `-k .req.host`). Each
  line is parsed as a single JSON value. Useful when the upstream
  is `jq` emitting JSONL.

A single alert may not mix the two forms across `-k` and `-t`.

## CONDITIONS

- `any` — fires whenever stdin is non-empty (default)
- `above <N>` — current value is greater than N
- `below <N>` — current value is less than N
- `eq <N>` — current value equals N
- `neq <N>` — current value does not equal N
- `changed` — value differs from previous run
- `absent` — no input received in the window
- `rising <N>%` — value increased by N% compared to previous/window average
- `falling <N>%` — value decreased by N% compared to previous/window average

## OPTIONS

- `--notify=<alias>` — dispatch to the named alias in `/etc/notify.yaml`. Without `--notify`, alert only writes to stderr.
- `-k <field>` — multi-row mode: each line of stdin is one row, and `<field>` selects the numeric value. `<field>` is either a 1-indexed whitespace column (e.g. `2`) or a `.`-prefixed JSON path against the line parsed as JSON (e.g. `.req.latency_ms`).
- `-t <field>`, `--tag <field>` — multi-row mode: `<field>` names this row for per-tag incident dedup and per-tag notifications. Same field grammar as `-k`. Must agree with `-k` on positional vs JSON-path. Required when the pipeline can produce more than one row per tick.
- `--duration <dur>` — time window for sustained conditions. For above/below: condition must hold for ALL runs in the window. For rising/falling: compares against the window average. For absent: no non-empty output in the window. Examples: 5m, 1h, 30s.
- `--average` — for above/below with `--duration`: compare against the rolling average over the window instead of requiring every individual run to match.
- `--msg <text>` — override the default alert message.

## EXAMPLES

```
tail -n 500 myapp.log | awk '$LEVEL == "ERROR"' | alert --notify=ops
check_errors | alert above 50 --notify=critical
check_errors | alert above 50 --duration 5m --average --notify=ops
check_errors | alert rising 100% --notify=critical
cat /log/canary.log | tail -1 | alert absent --duration 10m --notify=ops
echo "integration test" | alert --notify=ops

# Multi-row positional: alert per host whose disk use is above 80%.
tail /log/shard.log | jq -r 'select(.type=="disk") | "\(.host) \(.pct)"' \
    | alert above 80 -k 2 -t 1 --notify=ops

# Multi-row JSON path: same idea, no `jq -r` re-formatting required.
tail /log/shard.log | jq 'select(.type=="disk")' \
    | alert above 80 -k .pct -t .host --notify=ops
```

## SEE ALSO

`help`, `help alerts`, `vi`, `cat`
