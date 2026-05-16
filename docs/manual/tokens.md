---
id: manual-tokens
title: Tokens
web:
  section: tokens
  category: getting-started
  order: 20
---

## Create a token

Tokens authenticate weftd agents. Each token is scoped to a single namespace.
Go to **Namespaces**, select your namespace, and click **Create token**.

Copy the token immediately — it's only shown once. It looks like
`wft_a1b2c3d4e5f6...`

```
# Store the token in your weftd config
token: wft_a1b2c3d4e5f6...
```

### Binding env to a token

When creating a token you can optionally bind a deployment environment
(`prod`, `staging`, `qa`, …). Every row from an agent using that token
gets the env value stamped on the row before it reaches a shard — the
agent has no say in it, which means a leaked production token can't
secretly write rows tagged as staging.

The token-create form has an Env field; from the CLI:

```
weft token create --namespace myco --label api-prod --env prod
```

Every row picks up the bound env at ingest time, so any query the
token authenticates implicitly filters to that env — no path
contortions required. Plain apps land at the three-segment layout
(`/log/myco/api/main.log`); telemetry streams reuse that shape with
the kind segment swapped (`/log/myco/api/metrics.log`,
`/log/myco/api/heartbeats.log`, `/log/myco/api/runtime.log`); a
token spanning multiple envs disambiguates with the four-segment
form (`/log/myco/<env>/<service>/<kind>.log`). The `<kind>` segment
is always present and defaults to `main`. See **Query your logs** for how `$ENV`
surfaces in awk and jq.
