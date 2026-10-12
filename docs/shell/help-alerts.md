---
id: help-alerts
title: Alerts
shell:
  const: HelpAlerts
  topics:
    - "help alerts"
web:
  section: alerting
  category: alert-visualize
  order: 70
---

## Alerts

An alert evaluates a value from stdin against a condition.
When the condition holds, `alert` writes to stderr — in a
cron, that stderr is what drives incidents and notifications.
When the condition does not hold, stdin passes through to stdout
unchanged; when it holds, the alert message is written instead.
`alert` must be the last stage of a pipeline, and the last (or only)
command of a script.

## Conditions (summary)

- `above <N>` — value greater than N
- `below <N>` — value less than N
- `eq <N>`, `neq <N>`
- `changed` — value differs from previous run
- `absent` — no input received in the window
- `rising <N>%` — increase over baseline
- `falling <N>%` — decrease over baseline
- `any` — fires when stdin is non-empty. With `--notify=<alias>` and no
  condition, `any` is the default; a bare `alert` is a usage error, so
  write `alert any` for a dry run.

See `man alert` for grammar, `--duration`, `--average`, `--msg`.

## Composite alerts (state across multiple crons)

To fire on the combined state of more than one cron, compose
predicate builtins with bash `&&` `||` `!` inside a subshell, end it with
an `echo`, and pipe that into alert, which fires when anything comes out:

```
(weft::is_above error-rate 50 && weft::is_absent canary-ping && echo fire) | alert --notify=critical
```

To combine conditions on the logs themselves, count both in one pass over
the cron's time range and alert on the result. This fires when errors
are above 50 and nothing came from the canary service:

```
cat /log/myco/prod/api/main.log /log/myco/prod/canary/main.log \
  | awk 'LEVEL=="ERROR"{e++} SERVICE=="canary"{c++} END{print (e>50 && c+0==0) ? 1 : 0}' \
  | alert above 0 --notify=critical
```

Keep row-limiting `head -n` / `tail -n` out of cron pipelines: they cap the
rows read and can silently truncate the window. (At the prompt, `tail -n
500 | awk 'LEVEL=="ERROR"' | wc -l` counts errors among the last 500
rows, while `grep ERROR | tail -n 5` gives the last five matches.)

The predicate family — `weft::is_above`, `weft::is_below`,
`weft::is_absent`, `weft::has_changed`, `weft::is_rising`,
`weft::is_falling`, `weft::is_outlier`, `weft::is_seasonal` — each
exits 0/1 and has a Lua mirror (`weft.is_above(...)` etc.) that
returns true/false. See **Alerting strategies** for the full
catalogue plus seasonal/outlier guidance and **Weft functions** for the
per-function reference.

## Single-row vs multi-row

By default alert treats stdin as one scalar per tick. With
`-k <field>`, each non-blank line is one row and `<field>` is
the value to compare. With `-t <field>`, another field names
the row for per-tag incident dedup. `<field>` is either a
1-indexed whitespace column (`-k 2`) or a `.`-prefixed JSON
path (`-k .latency_ms`); `-k` and `-t` must agree on which form.

```
# per-host disk alert (positional)
cat /log/myco/prod/api/metrics.log \
  | jq -r 'select(.type=="disk") | "\(.host) \(.pct)"' \
  | alert above 80 -k 2 -t 1 --notify=ops

# same alert against JSONL — no jq -r reformatting (jq -c keeps one row per line)
cat /log/myco/prod/api/metrics.log | jq -c 'select(.type=="disk")' \
  | alert above 80 -k .pct -t .host --notify=ops
```

## /etc/notify.yaml

`--notify=<alias>` looks up a destination from your notify.yaml
— a YAML map of alias name → per-service targets. Edit with:

```
vi /etc/notify.yaml
```

Each alias bundles one or more targets across four service types:
`slack:`, `pagerduty:`, `webhook:`, and `email:`. All are lists;
each entry is either a bare string or a mapping.

```
ops:
  slack:
    - "#alerts"
  pagerduty:
    - ops-daytime
  webhook:
    - "https://hooks.example.com/weft/ops"
  email:
    - "oncall@example.com"

critical:
  slack:
    - "#oncall"
    - "#pager"
  pagerduty:
    - name: primary
      severity: critical
  webhook:
    - url: "https://intake.example.com/alerts"
      headers:
        X-Signing-Secret: "change-me"
  email:
    - "oncall@example.com"
    - "lead@example.com"
```

**Slack** — channel names (`"#ops"`) or `{channel: "#name"}`.
**PagerDuty** — integration names (severity defaults to `error`),
or `{name: "...", severity: "..."}` (`critical`/`error`/`warning`/`info`).
**Webhook** — bare URL strings, or `{url: "...", headers: {...}}`.
POSTs a JSON payload with `v` (currently 2), `cron` (the cron name), `alias`, `ts_unix`, `body`.
Header values are credentials: `cat /etc/notify.yaml` shows them as
`"<redacted>"`. Leave a `"<redacted>"` in place to keep the stored value, or
write a new one to replace it.
**Email** — bare addresses, or `{address: "..."}`. Sends a plain-text
email with the cron name as subject (requires SMTP configuration).

Slack and PagerDuty credentials are configured with `weft integration`
(or under Account → Integrations in the browser); notify.yaml only
names destinations:

```
weft integration slack connect
weft integration pagerduty add ops --key <integration-key>
weft integration list
```

The PagerDuty integration name must match one you've registered there.
Webhook and email destinations need no account-level credentials — URLs
and addresses are per-alias.

## Testing an alias

```
echo "test fire" | alert --notify=ops
```

Interactive runs dispatch immediately; they don't open an incident
(incidents are a cron concept).
