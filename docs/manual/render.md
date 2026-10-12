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
from Render's built-in **Log Streams** feature. It needs no agent, no AWS
account and no schema file. Render's off-platform log path is syslog over TLS,
so you point Render at a hostname:port and it opens a long-lived TLS
connection.

weftd is not part of a Render setup. Render services cannot run a sidecar
agent, and Render aggregates every service's output into its own Log Streams
pipeline, the same one Datadog, Better Stack, and other observability vendors
use.

### 1. Mint a Render-scoped token

Minting a token **for Render** also reveals the endpoint hostname you'll paste
into Render's log-stream config.

From the dashboard: **Namespaces → your namespace → Create token**, choose
**Render** in the **For** dropdown, and click **Create token**. The token is
shown only once, so copy it now. The endpoint is the same for every Render
token, so you can look it up again later.

From the CLI:

```
weft token create --namespace myco --label render --for render
Token created: wft_a1b2c3d4e5f6...
Label: render
For:   render
Endpoint: render.syslog.w3ft.net:6514
Paste this token and endpoint into that platform's log-stream config.
```

Copy the token immediately. It is shown only once.

### 2. Add the log stream in Render

In the Render dashboard, open your service and go to **Settings → Log
Streams**, add a stream, and configure a generic/custom syslog destination:

- **Endpoint** — the `render.syslog.w3ft.net:6514` string from step 1
- **Token** — the `wft_...` token from step 1

Render recognizes a short list of named providers (Datadog, Loggly,
Better Stack, …) by matching the endpoint hostname. Any other endpoint,
including weft's, uses Render's generic syslog behavior: plain TLS
syslog with the token prepended as a literal string before every line.
Render uses the same mechanism for New Relic. The token travels inside the TLS
session.

Render starts streaming within a few seconds. To confirm rows are landing:

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
filtering within one pseudofile:

| Render source                       | weft pseudofile                                  |
|---|---|
| `myservice` web instance `web-9nk6q` | `/log/<ns>/myservice/web.log` (`instance=9nk6q`) |
| `myservice` cron job `cron-stqq5`    | `/log/<ns>/myservice/cron.log`                   |
| `myservice` build output `bld-...`   | `/log/<ns>/myservice/bld.log`                    |

The tokens in step 1 carry no env, so the paths above have no env
segment. If your token has an env bound, the path picks up that segment:
`/log/myco/prod/myservice/web.log`. See **Namespaces** for the layout rules.

### Rate limits

Ingest is capped per token. A service that reconnects keeps the same budget.
Above the default sustained rate, rows are delayed (never dropped) until the
budget refills. Contact us if you need a higher sustained rate.

### Troubleshooting

- **No rows appearing** — check the log stream's status in Render's
  dashboard; a failing status there usually means the token or endpoint
  string was mistyped. Revoke and recreate the token from **Tokens** if you're
  unsure it's still valid.
- **Rows land under an unexpected service name** — weft uses Render's
  service slug as-is. If two Render services share a name (separate Render
  teams sharing one weft namespace), mint separate tokens in separate
  namespaces to keep them apart.
- **Build logs (`bld`) missing** — build output streams while a build is
  running. Trigger a deploy to see it.
- **To keep the bearer token out of Render's UI** — run
  [Vector](https://vector.dev) (as a Render private service, or on your own
  host) to receive the syslog stream and re-forward it to weft's HTTP ingest
  endpoint. The weft token then stays off Render's servers. Ask us for the
  Vector config.
