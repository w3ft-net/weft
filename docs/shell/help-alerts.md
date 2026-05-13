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

## Composite alerts (state across multiple watches)

To fire on the combined state of more than one watch, compose
predicate builtins with bash `&&` `||` `!` and hand the result
to alert via `--if-zero "$?"`:

```
weft::is_above error-rate 50 && weft::is_absent canary-ping
alert --if-zero "$?" --notify=critical
```

The predicate family — `weft::is_above`, `weft::is_below`,
`weft::is_absent`, `weft::has_changed`, `weft::is_rising`,
`weft::is_falling`, `weft::is_outlier`, `weft::is_seasonal` — each
exits 0/1 and has a Lua mirror (`weft.is_above(...)` etc.) that
returns true/false. See `docs/manual/alerting-strategies.md` for
the full catalogue plus seasonal/outlier guidance and
`docs/manual/weft-funcs.md` for the per-function reference.

## Single-row vs multi-row

By default alert treats stdin as one scalar per tick. With
`-k <field>`, each non-blank line is one row and `<field>` is
the value to compare. With `-t <field>`, another field names
the row for per-tag incident dedup. `<field>` is either a
1-indexed whitespace column (`-k 2`) or a `.`-prefixed JSON
path (`-k .latency_ms`); -k and -t must agree on which form.

```
# per-host disk alert (positional)
tail /log/shard.log | jq -r '"\(.host) \(.pct)"' \
  | alert above 80 -k 2 -t 1 --notify=ops

# same alert against JSONL — no jq -r reformatting
tail /log/shard.log | jq 'select(.type=="disk")' \
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
        X-Signing-Secret: "s3cret"
  email:
    - "oncall@example.com"
    - "paul@example.com"
```

**Slack** — channel names (`"#ops"`) or `{channel: "#name"}`.
**PagerDuty** — integration names (severity defaults to `error`),
or `{name: "...", severity: "..."}` (`critical`/`error`/`warning`/`info`).
**Webhook** — bare URL strings, or `{url: "...", headers: {...}}`.
POSTs a JSON payload with `v`, `watch`, `alias`, `ts_unix`, `body`.
**Email** — bare addresses, or `{address: "..."}`. Sends a plain-text
email with the watch name as subject (requires SMTP configuration).

Slack and PagerDuty credentials are configured under Account →
Integrations; notify.yaml only names destinations. The PagerDuty
integration name must match one you've registered there. Webhook
and email destinations need no account-level credentials — URLs
and addresses are per-alias.

## Testing an alias

```
echo "test fire" | alert --notify=ops
```

Interactive runs dispatch immediately; they don't open an incident
(incidents are a cron-watch concept).
