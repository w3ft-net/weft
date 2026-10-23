---
id: manual-alerts
title: Alerts
shell:
  const: HelpAlerts
  topics:
    - "help alerts"
    - "man alerts"
web:
  section: alerts
  category: analysis-alerting
  order: 51
---

The `alert` function generates notifications based on stdin and other
arguments. This page provides a reference for the function, a cookbook of
alerting strategies, and how to route notifications.

## Synopsis

```
<pipeline> | alert [<condition>] [--notify=<alias>]
                   [-v <field>] [-t|--tag <field>]
                   [--duration <dur>] [--average] [--msg <text>]

<pipeline> | alert (--if-zero <v> | --if-nonzero <v>) [--notify=<alias>]
```

## How alerts work

`alert` reads a value from stdin and evaluates it against a condition. If the
condition holds, it writes the alert message to stderr and, when `--notify` is
supplied, dispatches the alias from `/etc/notify.yaml` to its Slack, PagerDuty,
webhook, and/or email targets (see [Notifications](#notifications)). Its value
and tag flags can serve as a shorthand for shaping what [gets persisted in a
cron](crons.md#cron-result-persistence).

With no condition the default is `any`: the alert fires whenever stdin is
non-empty. `alert` alone is a dry run that writes to stderr.

`alert` must be the last stage of its pipeline, and the last (or only) command
of a script.

## Conditions

- `any` — fires whenever stdin is non-empty (the default when no condition is given)
- `above <N>` — current value is greater than N
- `below <N>` — current value is less than N
- `eq <N>` — current value equals N
- `neq <N>` — current value does not equal N
- `changed` — value differs from previous run
- `absent` — no input received in the window
- `rising <N>%` — value increased by N% compared to previous/window average
- `falling <N>%` — value decreased by N% compared to previous/window average
- `matches <regex>` — value matches a regular expression (RE2)

## Options

- `-v <field>`, `--value <field>` — column or JSON path holding the number to
  compare on each line. If unspecified, the whole input must parse as a float,
  unless the condition is `any`, `absent` and `changed`.
- `-t <field>`, `--tag <field>` — column or JSON path by which alerts should be
  grouped. Combine with `-v` when you want to alert separately by host, for
  example.

In a cron, `-v` and `-t` obviate the need for the [`# value:`/`# tag:`
headers](crons.md#extracting-values-and-tags-from-json) a cron without `alert` would use. A cron that
alerts on tagged lines or JSON must give `-v`; without it, output will fail
[runtime type checks](crons.md#runtime-analysis) and automatically be disabled.
If the stdin to `alert` is [typed as text](crons.md#declaring-text-output), the condition
must be one of `any`, `absent`, `changed` and `matches`.

- `--duration <dur>` — time window for sustained conditions. For
  `above`/`below`, condition must hold for all runs in the window. For
  `rising`/`falling`, compares against the window average. For `absent`, no
  non-empty output in the window. Units are s, m and h only.
- `--average` — for `above`/`below` with `--duration`, compare the rolling
  average over the window against the threshold. No effect without
  `--duration`.
- `--msg <text>` — override the default alert message.
- `--notify=<alias>` — dispatch to the named alias in `/etc/notify.yaml` (see
  [Notifications](#notifications)). Without `--notify`, alert writes to stderr.
- `--if-zero <status>` / `--if-nonzero <status>` — fire based on a shell exit
  status instead of a stdin value. Pass the status of the command just before
  `alert` as `"$?"`. `--if-zero` fires when the status is `0`, `--if-nonzero` is
  the inverse. Mutually exclusive with the condition keywords. Useful in longer
  scripts.

## Examples

```
# Alert if the range holds any ERROR line.
cat /log/myco/prod/api/main.log | grep ERROR | wc -l | alert above 0 --notify=ops

# Alert when the range holds more than 50 errors.
cat /log/myco/prod/api/main.log | grep ERROR | wc -l | alert above 50 --notify=critical

# check_errors is a script in /bin that prints a numeric
check_errors | alert above 50 --duration 5m --average --notify=ops
check_errors | alert rising 30% --notify=critical

# Alert when the range holds no canary line at all.
cat /log/myco/prod/canary/main.log | wc -l | alert eq 0 --notify=ops

# Alert when a lease changes hands: a text cron, where lease-holders is a
# /bin script printing "<holder> <lease>" lines.
#   # schedule: */2 * * * *
#   # type: text
lease-holders | alert changed -v 1 -t 2 --notify=ops

# Page when any host boots a release candidate; the cron still stores every
# host's build.
#   # schedule: */5 * * * *
#   # type: text
cat /log/myco/prod/api/main.log | grep 'starting version' \
    | awk '{v[HOST]=$NF} END {for (h in v) print v[h], h}' \
    | alert matches '-rc[0-9]*$' -v 1 -t 2 --notify=ops

# One page if any host's disk use is above 80%.
cat /log/myco/prod/api/disk.log \
    | awk '{print HOST, $5}' \
    | alert above 80 -v 2 --notify=ops

# Alert per host whose disk use is above 80%.
cat /log/myco/prod/api/disk.log \
    | awk '{print HOST, $5}' \
    | alert above 80 -v 2 -t 1 --notify=ops

# Same idea with JSON rows
cat /log/myco/prod/api/disk.log | jq -c '{host: $host, pct}' \
    | alert above 80 -v .pct -t .host --notify=ops
```

### Layering crons for richer alerts

A cron run, and thereby an alert reads only the logs from its own schedule
interval: a `*/5` cron sees the last five minutes. However, you can leverage
the stored results of other crons using the `weft::*` predicates.

The simplest layer reads another cron's history with an aggregate and pipes
the number to `alert`:

```
# /cron.d/error-rate-hourly
# schedule: every 5m
weft::avg error-rate 1h | alert above 50 --notify=ops
```

If you want to use boolean logic to add multiple such predicates, just add one
more condition and echo if nothing's failed yet:

```
# /cron.d/deploy-danger
# schedule: every 1m
(weft::is_above error-rate 50 && weft::is_absent canary-ping && echo 1) | alert --notify=critical
```

If your love of bash is less than it should be, you may want to stuff a Lua
script in `/bin` and pipe that to `alert`.

```lua
#!/usr/bin/env lua
local rate = 0
if not weft.is_absent("error-rate") then
  rate = tonumber(weft.latest("error-rate")) or 0
end
local canary_dead = weft.is_absent("canary-ping")
local in_maintenance = not weft.is_absent("maintenance-flag")

if rate > 50 and canary_dead and not in_maintenance then
  io.stderr:write(string.format("composite fire: rate=%.1f canary_dead=%s\n",
    rate, tostring(canary_dead)))
  print(1)
else
  print(0)
end
```

We'll write that script to `/bin/deploy_danger`, and then we can write this for
our alerting cron:

```
# schedule: every 1m
deploy_danger | alert above 0 --notify=critical
```

## Static analysis

In addition to the checks [run for every cron](crons.md#static-analysis), there are some specific checks run related to crons that contain `alert` invocations:

* **Alert positioning** -- `alert` can only be invoked once per cron, and it
  has to be the last thing executed 
* **Multiple aliases** -- `alert` will bail if you provide `--notify` more than
  once (if you want several targets for an alert, you can configure your alias
with as many as you like)
* **Bad arguments** -- all conditions and flags must be known and stdin,
  positional arguments, and flag arguments must typecheck correctly, and `-v`
and `-t` must not mix numeric field positions with a JSON path
* **One schema** -- a cron that alerts must declare its value and tagging with
  `-v`/`-t`, not `# value:`/`# tag:` headers
* **Types** -- on a [cron with non-numeric
  output](crons.md#declaring-text-output), the condition must be `any`,
`absent`, `changed` or `matches`, and `matches` must not receive numeric input

## Notifications

### Define a notification alias

When `alert` executes in a cron, `--notify=<alias>` dispatches to a destination
defined in `/etc/notify.yaml`. One alias specifies one or more targets across
four service types — Slack, PagerDuty, webhook, and email.

```
ops:
  slack:
    - "#alerts"
  pagerduty:
    - ops-daytime
  email:
    - "oncall@example.com"
```

Edit it `/etc/notify.yaml` with vim in the weft shell.  Weft refuses to save
`/etc/notify.yaml` if:

* the file doesn't parse
* an alias has no targets
* a webhook `url` or header refers to a [`${secrets.NAME}`](#webhook-secrets) you haven't defined
* you removed the `system` alias

### The system alias

Every org has an alias named `system`. It receives the alerts weft raises, as
opposed to the ones your own `alert` stages raise. For example, weft pages
`system` when a cron stops executing successfully, or when it disables a cron
after [runtime analysis failures](crons.md#runtime-analysis). Weft initially
populates it with the email address of the person who created the org:

```
system:
  email:
    - "you@example.com"
```

You can change its target to any service type. You just can't remove the
top-level key.

### Connecting Slack and PagerDuty

`notify.yaml` names destinations — the credentials behind them live with your
account. Connect them  under **Account → Integrations** in the browser or from
the terminal with `weft integration`.

```
# PagerDuty
weft integration pagerduty add ops-daytime --key YOUR_ROUTING_KEY
weft integration pagerduty rm  ops-daytime

# Slack
weft integration slack connect
weft integration slack rm
```

A PagerDuty alias in `notify.yaml` must match a name you've registered with
`weft integration pagerduty add`. Slack channels are named directly in
`notify.yaml` (`"#alerts"`) once the workspace is connected, and you'll have to
add weft to those channels for it to notify you.

### Webhooks

A webhook target is either a bare URL or a mapping with a `url` and optional
`headers`:

```
pages:
  webhook:
    - "https://hooks.example.com/weft"
    - url: "https://api.example.com/alerts"
      headers:
        Authorization: "Bearer ${secrets.pages_token}"
        X-Team: ops
```

When an alert opens an incident, weft sends each webhook target one HTTP `POST`
whose body is JSON:

```
{
  "v": 2,
  "cron": "disk-use[web-1]",
  "alias": "pages",
  "ts_unix": 1792677600,
  "body": "disk-use[web-1]: 91.0 > 80.0"
}
```

| Field | Value |
|---|---|
| `v` | The version of the weft webhooks protocol. |
| `cron` | The name of the cron that alerted. With `alert -t`, the tag is appended in brackets, as above, so each tag has its own name. |
| `alias` | The `notify.yaml` alias that routed the alert |
| `ts_unix` | When weft sent the request, in seconds since the epoch |
| `body` | The alert message: what the alert prints to `stderr`, or your `--msg` if you gave one. At most 1024 bytes, with `... (truncated)` appended when it was cut |

You bring your own HTTP headers, with the exception of `Content-Type:
application/json`, which you can override should you need to.

A webhook succeeds when it answers with a status below 300 within the
[delivery timeout](#key-behaviors). Anything else is a failed delivery and
is retried on the following ticks of the incident.

#### Webhook secrets

If you need to use a token in an auth header for a webhook but would rather not
shove a secret somewhere broadly readable, store it as a webhook secret instead
and refer to it by name:

```
weft integration webhook-secret set my_token --value s3cr3t
```

You can then interpolate it with `${secrets.my_token}` in a webhook's `url` or
in a header value, as in the example above. Weft substitutes the value when it
sends the request.  Error messages, logs, catting `/etc/notify.yaml` etc. show
`${secrets.pages_token}` instead of the value.

Secret names must be `[\w_-]*`. You can't write `notify.yaml` if it refers to a
secret you haven't defined, and `weft integration webhook-secret rm <name>`
will fail if an alias still refers to it.


### Email

Email addresses carry no account-level credential. They are written into
`notify.yaml` per alias:

```
pages:
  email:
    - "oncall@example.com"
```

### Testing an alias

```
echo "test fire" | alert --notify=ops
```

This sends to every target in the `ops` alias.

Interactive runs notify every time. Only crons have incidents, so nothing is
suppressed.

### Key behaviors

A cron checks its condition on every tick. To avoid pagestorming you, weft
groups consecutive ticks during which the condition holds. A cron will only
notify once per entrance into a failed state (or per tag if specified).  If a
send fails (PagerDuty is unreachable, a Slack channel is gone), the run reports
it and exits non-zero, and each following tick of the incident tries again.
Delivery is tracked per target, so a retry goes only to the targets that missed
it, not to the ones already told.  The first tick where the condition doesn't
hold sets it to green. It won't autoresolve in PagerDuty when it does so.

Each target gets 10 seconds to accept a notification, regardless of service.  A
target that doesn't is flagged as failed, and three consecutive failed
deliveries disable the cron. 
