---
id: manual-heroku
title: Heroku
web:
  section: heroku
  category: connect-source
  order: 35
---

## Connecting a Heroku app

Weft accepts logs directly from Heroku's HTTPS log-drain protocol. This page
covers the Heroku-specific parts; for an overview, see [Connect a log
source](connect-source.md).

### 1. Create a token

Create a namespaced token as described in [Connect a log
source](namespaces.md#creating-a-token).  There's nothing special about Heroku
that requires its own token, but it's probably tidier if it's got its own.

### 2. Add the drain

For each Heroku app you want to ingest, run `heroku drains:add` with the
token embedded in the weft drain URL. Your token goes in the URL like so:

```
heroku drains:add "https://api.w3ft.net/api/ingest/heroku/wft_a1b2c3d4e5f6..." -a myapp
heroku drains:add "https://api.w3ft.net/api/ingest/heroku/wft_a1b2c3d4e5f6..." -a sister-app
```

Heroku starts delivering within a few seconds. Then confirm rows are landing
and, if you want nice, speedy lookups, [approve a
schema](connect-source.md#sampling-logs-for-a-schema). Heroku schemas are per
service, so it covers the web, cron and build files under it.

### How dynos and procs map to log files

The Heroku app name is the weft service, and the process type from the
procid (`web`, `worker`, `router`) is the log kind.
(See **Namespaces** for how every producer chooses its service name.)
The full procid (`web.1`, `web.2`) is stamped on `instance`, so cross-dyno
aggregation stays cheap and a single `tail /log/.../web.log` covers every web
dyno at once.

#FIXME match the Render table columns
| Heroku source           | weft pseudofile                                        |
|---|---|
| `myapp web.1`, `web.2`  | `/log/<ns>/<env>/myapp/web.log`   (`instance=web.1`, etc.) |
| `myapp worker.1`        | `/log/<ns>/<env>/myapp/worker.log`                     |
| `myapp router`          | `/log/<ns>/<env>/myapp/router.log`                     |
| `myapp heroku-postgres` | `/log/<ns>/<env>/myapp/heroku-postgres.log`            |
| Platform events (deploys, restarts, scales) | `/log/<ns>/<env>/myapp/platform.log` |

(The presence of the `<env>` segment depends on how you created your token.
See [Paths](query-logs.md#paths) for the layout rules.)

### Metric lines auto-route to `metrics.log`

Heroku's `runtime-heroku-metrics` labs feature (and most add-ons) emit
l2met-shaped lines like:

```
source=web.1 sample#memory_total=512.00MB sample#memory_rss=487.21MB sample#load_avg_1m=0.42
```

Weft detects these and routes them to `<appname>/metrics.log`, with the
key/value pairs converted to JSON (numeric where the value is
numeric, with trailing units like `MB` kept on a sibling
`<key>_unit` field). The original line also flows into the dyno's
log file, so `grep memory_total /log/.../web.log` matches it.

```
# Enable the metric stream on a Heroku app (one-time)
heroku labs:enable "runtime-heroku-metrics" -a myapp

# Then read it like any other JSON log source
weft sh
$ tail /log/myco/prod/myapp/metrics.log | jq 'select(.memory_total > 400)'
```
