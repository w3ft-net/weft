---
id: manual-render
title: Connect a Render service
web:
  section: connect-render
  category: connect-source
  order: 37
---

## Connect a Render service

If your app runs on [Render](https://render.com), weft accepts logs directly
from Render's built-in **Log Streams** feature — no agent, no AWS account, no
schema file. Render's only off-platform log path is syslog over TLS, not
HTTP, so instead of a drain URL (like Heroku) you point Render at a
hostname:port and it opens a long-lived TLS connection.

You don't run weftd on Render. Render services can't sidecar a long-lived
agent process, and Render already aggregates every service's output into its
own Log Streams pipeline — the same one Datadog, Better Stack, and other
observability vendors plug into.

### 1. Mint a Render-scoped token

Minting a token **for Render** (rather than a plain namespace token) also
reveals the endpoint hostname you'll paste into Render's log-stream config.

From the dashboard: **Namespaces → your namespace → Create token**, choose
**Render** in the **For** dropdown, and click **Create token**. The token is shown only once, so copy it now. The endpoint is the same
for every Render token, so you can always look it up again later.

From the CLI:

```
weft token create --namespace myco --label render --for render
Token created: wft_a1b2c3d4e5f6...
Label: render
For:   render
Endpoint: render.syslog.w3ft.net:6514
Paste this token and endpoint into that platform's log-stream config.
```

Copy the token immediately — it's only shown once.

### 2. Add the log stream in Render

In the Render dashboard, open your service and go to **Settings → Log
Streams**, add a stream, and configure a generic/custom syslog destination:

- **Endpoint** — the `render.syslog.w3ft.net:6514` string from step 1
- **Token** — the `wft_...` token from step 1

Render only recognizes a short list of named providers (Datadog, Loggly,
Better Stack, …) by matching the endpoint hostname; anything else — including
weft's endpoint — falls back to Render's generic syslog behavior: plain TLS
syslog with the token prepended as a literal string before every line.
That's expected — it's the same mechanism Render uses for New Relic, and
it's why the token travels inside the TLS session instead of in a header.

Render starts streaming within a few seconds. Confirm rows are landing:

```
weft> ls /log/myco/myservice
cron.log
web.log

weft> tail /log/myco/myservice/web.log
```

### How Render services map to pseudofiles

Render's service slug becomes the weft service name (see **Namespaces**
for how every producer chooses its service name); the process type
(`web`, `cron`, and `bld` for build-step output) becomes
`kind`; the trailing instance id is stamped on `instance` for per-instance
filtering without splitting the pseudofile:

| Render source                       | weft pseudofile                                  |
|---|---|
| `myservice` web instance `web-9nk6q` | `/log/<ns>/myservice/web.log` (`instance=9nk6q`) |
| `myservice` cron job `cron-stqq5`    | `/log/<ns>/myservice/cron.log`                   |
| `myservice` build output `bld-...`   | `/log/<ns>/myservice/bld.log`                    |

The tokens in step 1 carry no env, so the paths above have no env
segment. If your token has an env bound, the path picks up that segment:
`/log/myco/prod/myservice/web.log`. See **Namespaces** for the layout rules.

### Rate limits

Ingest is capped per token, not per connection — a service that reconnects
doesn't reset its budget. The default sustained rate is well above any real
Render service's log volume; if you're sustained above it, rows are delayed
(never dropped) until the budget refills. Contact us if you need a higher
sustained rate.

### Troubleshooting

- **No rows appearing** — check the log stream's status in Render's
  dashboard; a failing status there usually means the token or endpoint
  string was mistyped. Revoke and recreate the token from **Tokens** if you're
  unsure it's still valid.
- **Rows land under an unexpected service name** — weft trusts Render's
  service slug as-is. If two Render services legitimately share a name
  (separate Render teams sharing one weft namespace), mint separate tokens
  in separate namespaces to keep them apart.
- **Build logs (`bld`) missing** — build output only streams while a build is
  actually running; trigger a deploy to see it.
- **Don't want to paste a bearer token into Render's UI directly** — run
  [Vector](https://vector.dev) yourself (as a Render private service, or on
  your own host) to receive the syslog stream and re-forward it to weft's
  HTTP ingest endpoint instead, keeping the weft token off Render's servers
  entirely. Ask us for the Vector config.
