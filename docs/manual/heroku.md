---
id: manual-heroku
title: Connect a Heroku app
web:
  section: connect-heroku
  category: connect-source
  order: 35
---

## Connect a Heroku app

If your app runs on Heroku, weft accepts logs directly from Heroku's HTTPS
log-drain protocol. It needs no agent, no AWS account and no schema file. You
mint a token, point a drain at weft, and dynos start flowing within seconds.

weftd is not part of a Heroku setup. Dynos are ephemeral and cannot run a
sidecar agent, and Heroku aggregates every dyno's stdout/stderr into
Logplex. The drain endpoint consumes that stream.

### 1. Mint a token

Each token is scoped to one existing namespace (create the namespace
first) and, optionally, a single env.
From the dashboard, **Namespaces → your namespace → Create token**, or from
the CLI:

```
weft token create --namespace myco --env prod --label heroku
Token created: wft_a1b2c3d4e5f6...
Label: heroku
Env:   prod (stamped on every row from this token)
Store this token somewhere safe. It cannot be retrieved again.
```

Copy the token immediately. It is shown only once. One token can carry
many Heroku apps. Env is bound at mint time, so each env needs its own token.

### 2. Add the drain

For each Heroku app you want to ingest, run `heroku drains:add` with the
token embedded in the URL path. Heroku's drain protocol has no
`Authorization` header, so the token goes in the URL.

```
heroku drains:add "https://api.w3ft.net/api/ingest/heroku/wft_a1b2c3d4e5f6..." -a myapp
heroku drains:add "https://api.w3ft.net/api/ingest/heroku/wft_a1b2c3d4e5f6..." -a sister-app
```

Heroku starts delivering within a few seconds. To confirm the rows are
landing:

```
weft> ls /log/myco/prod/myapp
heroku-postgres.log
platform.log
router.log
web.log
worker.log

weft> tail /log/myco/prod/myapp/web.log
```

### How dynos and procs map to pseudofiles

The Heroku app name is the weft service, and the process type from the
procid (`web`, `worker`, `router`) is the log kind.
(See **Namespaces** for how every producer chooses its service name.)
The full procid (`web.1`, `web.2`) is stamped on `instance`, so cross-dyno
aggregation stays cheap and a single `tail /log/.../web.log` covers every web
dyno at once.

| Heroku source           | weft pseudofile                                        |
|---|---|
| `myapp web.1`, `web.2`  | `/log/<ns>/<env>/myapp/web.log`   (`instance=web.1`, etc.) |
| `myapp worker.1`        | `/log/<ns>/<env>/myapp/worker.log`                     |
| `myapp router`          | `/log/<ns>/<env>/myapp/router.log`                     |
| `myapp heroku-postgres` | `/log/<ns>/<env>/myapp/heroku-postgres.log`            |
| Platform events (deploys, restarts, scales) | `/log/<ns>/<env>/myapp/platform.log` |

The `<env>` segment is present because the token in step 1 was minted with
`--env prod`. A token minted without `--env` gives the shorter form,
`/log/<ns>/myapp/web.log`. See **Namespaces** for the layout rules.

### Metric lines auto-route to `metrics.log`

Heroku's `runtime-heroku-metrics` labs feature (and most add-ons) emit
l2met-shaped lines like:

```
source=web.1 sample#memory_total=512.00MB sample#memory_rss=487.21MB sample#load_avg_1m=0.42
```

Weft detects these and routes them to `<appname>/metrics.log`, with the
key/value pairs lifted into payload JSON (numeric where the value is
numeric; trailing units like `MB` are kept on a sibling
`<key>_unit` field). The original line also flows through the dyno's
pseudofile, so `grep memory_total /log/.../web.log` matches it.

```
# Enable the metric stream on a Heroku app (one-time)
heroku labs:enable "runtime-heroku-metrics" -a myapp

# Query it like any other JSON log source
weft> tail /log/myco/prod/myapp/metrics.log | jq 'select(.memory_total > 400)'
```

### Troubleshooting

- **No rows appearing** — run `heroku drains -a myapp` and check the
  `Last error` column. A `401` means the token in the URL is wrong or
  has been revoked; revoke it and mint a new one (see **Tokens**), then
  re-add the drain. A `404` means the drain URL is malformed — most
  often a trailing slash or a missing `/api/ingest/heroku/` prefix.
- **Rows land under the wrong service** — Heroku uses `appname=heroku`
  for platform-emitted events (deploys, scales, restarts). Weft maps
  those to `<your-app>/platform`. Other unexpected rows usually come from an
  add-on with a new procid; they appear under
  `/log/<ns>/<env>/<your-app>/<procid>.log` automatically.
- **Drains showing intermittent 429s** — the default cap is 1000
  events/sec per token. Heroku's drain protocol retries on 5xx and 429
  with backoff, so transient bursts are absorbed. For sustained rates
  above the cap, split into more tokens or talk to us about a higher
  limit.
