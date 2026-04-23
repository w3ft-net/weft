---
id: help-alerts
title: Alerts
shell:
  const: HelpAlerts
  topics:
    - "help alerts"
web:
  section: alerting
  order: 70
---

## Alerts

An alert evaluates a value from stdin against a condition.
When the condition holds, `alert` writes to stderr — in a
watch, that stderr is what drives incidents and notifications.
Stdin always passes through to stdout unchanged (tee behavior).

## Conditions (summary)

- `above <N>` — value greater than N
- `below <N>` — value less than N
- `eq <N>`, `neq <N>`
- `changed` — value differs from previous run
- `absent` — no input received in the window
- `rising N%` — increase over baseline
- `falling N%` — decrease over baseline
- (no keyword) — "any"; fires when stdin is non-empty

See `man alert` for grammar, `--duration`, `--average`, `--msg`.

## Single-row vs multi-row

By default alert treats stdin as one scalar per tick. With
`-k <N>`, each non-blank line is one row and field N (1-indexed,
whitespace-split) is the value. With `-t <N>`, another field
names the row for per-tag incident dedup.

```
# per-host disk alert
tail /log/shard.log | jq -r '"\(.host) \(.pct)"' \
  | alert above 80 -k 2 -t 1 --notify=ops
```

## /etc/notify.conf

`--notify=<alias>` looks up a destination from your notify.conf
— a YAML map of alias name → per-service targets. Edit with:

```
vi /etc/notify.conf
```

Each alias bundles one or more Slack channels and/or PagerDuty
integrations. `slack:` and `pagerduty:` are always lists; each entry
is either a bare string or a mapping.

```
ops:
  slack:
    - "#alerts"
  pagerduty:
    - ops-daytime

critical:
  slack:
    - "#oncall"
    - "#pager"
  pagerduty:
    - name: primary
      severity: critical
```

Slack entries are channel names (strings), or `{channel: "#name"}`
mappings. PagerDuty entries are integration names (strings, severity
defaults to `error`), or `{name: "...", severity: "..."}` where severity
is one of `critical`, `error`, `warning`, `info`.

Slack and PagerDuty credentials are configured under Account →
Integrations; notify.conf only names destinations. The PagerDuty
integration name must match one you've registered there.

## Testing an alias

```
echo "test fire" | alert --notify=ops
```

Interactive runs dispatch immediately; they don't open an incident
(incidents are a cron-watch concept).
