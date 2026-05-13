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
log-drain protocol — no agent, no AWS account, no schema file. You mint a
token, point a drain at weft, and dynos start flowing within seconds.

You don't run weftd on Heroku. Dynos are ephemeral and can't sidecar a
long-lived agent, and Heroku already aggregates every dyno's stdout/stderr
into Logplex for you. The drain endpoint consumes that stream as-is.

### 1. Mint a token

Each token is scoped to a single namespace and (optionally) a single env.
From the dashboard, **Namespaces → your namespace → Create token**, or from
the CLI:

```
weft token create --namespace myco --env prod --label heroku
issued: wft_a1b2c3d4e5f6...
```

Copy the token immediately — it's only shown once. One token can carry
many Heroku apps; separate envs need separate tokens because env is bound
at mint time.

### 2. Add the drain

For each Heroku app you want to ingest, run `heroku drains:add` with the
token embedded in the URL path. Heroku's drain protocol doesn't support
`Authorization` headers, so the token rides in the URL.

```
heroku drains:add "https://api.w3ft.net/api/ingest/heroku/wft_a1b2c3d4e5f6..." -a myapp
heroku drains:add "https://api.w3ft.net/api/ingest/heroku/wft_a1b2c3d4e5f6..." -a sister-app
```

Heroku starts delivering within a few seconds. Confirm the rows are
landing:

```
weft> ls /log/myco/myapp
heroku-postgres.log
platform.log
router.log
web.log
worker.log

weft> tail /log/myco/myapp/web.log
```

### How dynos and procs map to pseudofiles

Each `(appname, procid)` pair from Heroku becomes its own weft sub-app.
The dyno number (`.1`, `.2`) is dropped from the app name and stamped on
`instance` instead, so cross-dyno aggregation stays cheap and a single
`tail /log/.../web.log` covers every web dyno at once.

| Heroku source           | weft pseudofile                                   |
|---|---|
| `myapp web.1`, `web.2`  | `/log/<ns>/myapp/web.log`         (`instance=web.1`, etc.) |
| `myapp worker.1`        | `/log/<ns>/myapp/worker.log`                       |
| `myapp router`          | `/log/<ns>/myapp/router.log`                       |
| `myapp heroku-postgres` | `/log/<ns>/myapp/heroku-postgres.log`              |
| Platform events (deploys, restarts, scales) | `/log/<ns>/myapp/platform.log` |

If your token has an env bound, the path picks up that segment:
`/log/myco/prod/myapp/web.log`. See **Namespaces** for the layout rules.

### Metric lines auto-route to `/metrics`

Heroku's `runtime-heroku-metrics` labs feature (and most add-ons) emit
l2met-shaped lines like:

```
source=web.1 sample#memory_total=512.00MB sample#memory_rss=487.21MB sample#load_avg_1m=0.42
```

Weft detects these and rewrites them to `<appname>/metrics`, with the
key/value pairs lifted into payload JSON (numeric where the value is
numeric; trailing units like `MB` are preserved on a sibling
`<key>_unit` field). The original line still flows through the dyno's
pseudofile too, so a literal `grep memory_total /log/.../web.log` keeps
working.

```
# Enable the metric stream on a Heroku app (one-time)
heroku labs:enable "runtime-heroku-metrics" -a myapp

# Query it like any other JSON log source
weft> tail /log/myco/myapp/metrics.log | jq 'select(.memory_total > 400)'
```

### Troubleshooting

- **No rows appearing** — run `heroku drains -a myapp` and check the
  `Last error` column. A `401` means the token in the URL is wrong or
  has been revoked; reissue it from **Tokens** and re-add the drain. A
  `4xx parse error` means the drain URL is malformed — most often a
  trailing slash or a missing `/api/ingest/heroku/` prefix.
- **Rows land but `app` looks wrong** — Heroku reuses `appname=heroku`
  for platform-emitted events (deploys, scales, restarts). Weft maps
  those to `<your-app>/platform`. Anything else where `app` looks
  unexpected is usually an add-on whose procid weft hasn't seen before;
  it'll show up under `/log/<ns>/<your-app>/<procid>.log` automatically.
- **Drains showing intermittent 429s** — the default cap is 1000
  events/sec per token. Heroku's drain protocol retries on 5xx and 429
  with backoff, so transient bursts are absorbed; if you're sustained
  above the cap, split into more tokens or talk to us about a higher
  limit.
