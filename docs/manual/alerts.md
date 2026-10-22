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
webhook, and/or email targets (see [Notifications](#notifications)). When the
condition does not hold, stdin passes through to stdout unchanged.

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

## Options

- `-v <field>`, `--value <field>` — column or JSON path holding the number to
  compare on each line. If unspecified, the value is the entire line.
- `-t <field>`, `--tag <field>` — column or JSON path by which alerts should be
  grouped. Combine with `-v` when you want to alert separately by host, for
  example.
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
cat /log/myco/prod/api/main.log | grep ERROR | alert --notify=ops

# Alert when the range holds more than 50 errors.
cat /log/myco/prod/api/main.log | grep ERROR | wc -l | alert above 50 --notify=critical

# check_errors is a script in /bin that prints a numeric
check_errors | alert above 50 --duration 5m --average --notify=ops
check_errors | alert rising 30% --notify=critical

# Alert when the range holds no canary line at all.
cat /log/myco/prod/canary/main.log | alert absent --notify=ops

echo "integration test" | alert --notify=ops

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

#FIXME simple example

If you want to use boolean logic to add multiple such predicates, just add one
more condition and echo if nothing's failed yet:#

```
# /cron.d/deploy-danger
# schedule: every 1m
(weft::is_above error-rate 50 && weft::is_absent canary-ping && echo fire) | alert --notify=critical
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

## Validations

Weft checks crons twice: when you save one, so a mistake is refused before it
costs you a night of missed alerts, and while it runs, so a problem that only
shows up with real data still reaches someone.

### When you save

A save is refused, with a message saying why, if:

| Check | What's refused |
|---|---|
| Schedule | A missing schedule line, or one outside the [accepted grammar](crons.md#scheduling), such as a time of day or a weekday other than Monday |
| Plan | A schedule faster than your plan allows, or one more cron than your plan includes |
| Syntax | A pipeline that doesn't parse |
| One alert, last | An `alert` that isn't the final stage of the cron's last command: in a loop, a conditional, a group, an `&&` chain, or before more work. A cron has one alert stage |
| One alias | More than one `--notify` on an alert. To page several places, point `--notify` at one `notify.yaml` alias with several targets, or write one cron per alert |
| Alert arguments | An unknown condition or option, a non-numeric threshold or duration, or `-v` and `-t` that mix a column number with a JSON path |
| Piped `weft::` functions | A `weft::` function, other than `weft::is_gt` and `weft::is_lt`, that sits after a `|`. They read stored history and ignore their input, so the upstream stage would be discarded. To test a piped value, use the comparators |
| Scripts | In `/bin` scripts, the same alert and `weft::` rules, plus `tail -f` and `range`, which have no place in a script that runs to completion |
| `notify.yaml` | A file that doesn't parse, an alias with no targets, a webhook-header placeholder with nothing stored behind it, or no `system` alias |

A few things are allowed but flagged. If a `head` or `tail` cap feeds a
counting stage, so the count would silently cover only part of the interval,
the save goes through with a warning. So does a pipeline with a stage that runs
after retrieval, since it costs something on every run.

### When it runs

| Situation | What happens |
|---|---|
| The pipeline fails before `alert` | The alert isn't evaluated. The run is recorded with status `error`, and no incident opens or closes |
| The value isn't a number for a numeric condition | `alert` reports `non-numeric value` and exits non-zero |
| A line has no tag under `-t` | That line is skipped, with a note in `stderr` |
| A notification can't be delivered | The run exits non-zero, and each following tick of the incident retries the targets that missed it |
| A predicate can't evaluate (no history yet, a non-numeric value, a bad window) | It is false, not an error; see [Predicates](crons.md#predicates). Only `weft::is_absent` says why on `stderr` |
| A `weft::` function is given a window past its longest | An error naming the limit, never a silently shorter window |
| A cron stops running | If a cron goes more than twice its interval without a run, weft notifies your [`system` alias](#the-system-alias). It does so once per episode, and one message per org even if many crons stopped together |

## Notifications

### Define a notification alias

When an `alert` fires in a cron, `--notify=<alias>` dispatches to a
destination defined in `/etc/notify.yaml`. The alias is a name;
it fans out to one or more targets across four service types — Slack,
PagerDuty, webhook, and email.

```
# /etc/notify.yaml
ops:
  slack:
    - "#alerts"
  pagerduty:
    - ops-daytime
  email:
    - "oncall@example.com"
```

Edit it with `vi /etc/notify.yaml`. See `help alerts` for the full
grammar (severities, webhook headers, per-target mappings).

### The system alias

Every org has an alias named `system`. It receives the alerts weft raises on
your behalf, as opposed to the ones your own `alert` stages raise. Today that
is a cron that has stopped running: if a cron goes more than twice its interval
without a run, weft notifies `system`.

It starts out pointing at the email address of the person who created the org:

```
system:
  email:
    - "you@example.com"
```

You can change its targets freely, to a Slack channel, a PagerDuty service, a
shared mailbox, or several of them, but you can't remove it. A save of
`/etc/notify.yaml` without a `system` alias is refused. Because ownership can't
be transferred, point it at somewhere that outlives any one person, such as a
team mailbox or a PagerDuty service.

Email delivery needs SMTP configured for your deployment, as for any email
target.

### Connecting Slack and PagerDuty

`notify.yaml` names destinations — the credentials behind them
live with your account. Connect them from the terminal with `weft
integration` or under **Account → Integrations** in the browser;
both do the same thing.

```
# PagerDuty — paste an integration (routing) key, give it a name.
# The name is what notify.yaml references under `pagerduty:`.
weft integration pagerduty add ops-daytime --key YOUR_ROUTING_KEY
weft integration pagerduty rm  ops-daytime

# Slack — authorize a workspace. Slack uses OAuth, so this opens a
# browser tab to grant access (like `weft login`); the CLI waits and
# confirms once the workspace is connected.
weft integration slack connect
weft integration slack rm

# See what's connected, plus the PagerDuty names notify.yaml can use.
weft integration list
```

A PagerDuty alias in `notify.yaml` must match a name you've registered
with `weft integration pagerduty add`. Slack channels are named
directly in `notify.yaml` (`"#alerts"`) once the workspace is
connected.

### Webhook and email need no account connection

Webhook URLs and email addresses carry no account-level credential.
They are written into `notify.yaml` per alias:

Webhook **header values** are the exception: an `Authorization` token
or a signing secret is a credential, so `cat /etc/notify.yaml` (and
`git log -p /etc/notify.yaml`) shows each one as `"<redacted>"`. To edit,
leave `"<redacted>"` where a value should stay unchanged, or write a real
value to replace it. A `"<redacted>"` on a webhook, header or alias that
has nothing stored is refused.

```
pages:
  webhook:
    - "https://hooks.example.com/weft"
  email:
    - "oncall@example.com"
```

Email delivery requires SMTP to be configured for your deployment.

### Testing an alias

```
echo "test fire" | alert --notify=ops
```

This sends to every target in the `ops` alias.

Interactive runs notify every time. Only crons have incidents, so nothing is
suppressed.

### Debouncing

A cron checks its condition on every tick. To avoid paging you on each one,
weft groups consecutive ticks during
which the condition holds. Each cron has at most one open incident at a time
(one per tag, with `-t`).

- The first tick where the condition holds opens an incident and sends the
  notification.
- Ticks while the incident is open send nothing more. The alert message still
  goes to the run's `stderr`, so it is in the run's recorded result.
- If a send fails (PagerDuty is unreachable, a Slack channel is gone), the run
  reports it and exits non-zero, and each following tick of the incident tries
  again. Delivery is tracked per target, so a retry goes only to the targets that
  missed it, not to the ones already told.
- The first tick where the condition doesn't hold closes the incident. Closing
  sends nothing, including to PagerDuty, where you resolve the page yourself.
- If the condition holds again after that, a new incident opens and you are
  notified again.

Incidents are bookkeeping for notification, not something you manage: there is
nothing to list, acknowledge or close by hand. A run that errors neither opens
nor closes one. Interactive runs have no cron, so they have no incidents and
notify every time.

## See also

- **Crons** — scheduling, result history, and the reference for the
  `weft::*` value funcs, predicates, and stdin comparators.
