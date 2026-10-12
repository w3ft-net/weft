---
id: manual-tokens
title: Tokens
web:
  section: tokens
  category: getting-started
  order: 20
---

## Create a token

Tokens authenticate weftd agents. Each token is scoped to exactly one
namespace, and that namespace must already exist; naming a missing one
fails with `namespace X not found — create it first`. Namespace names
containing a comma, whitespace, `/`, `+`, `*`, `?` or `[`, or starting
with `-`, are invalid, so `--namespace a,b` is rejected.
Go to **Namespaces**, select your namespace, and click **new token**.

Copy the token immediately; it is shown once. It looks like
`wft_a1b2c3d4e5f6...`. If you lose it, revoke it and create a new one:
`weft token list` shows your tokens and `weft token revoke --id ID`
revokes one.

```
# Store the token in your weftd config
token: wft_a1b2c3d4e5f6...
```

### Binding env to a token

When creating a token you can optionally bind a deployment environment
(`prod`, `staging`, `qa`, …). Every row from an agent using that token
gets the env value stamped on the row before it is stored. The agent
cannot change it, so a token bound to `prod` always writes rows tagged
`prod`.

The token-create form has an Env field; from the CLI:

```
weft token create --namespace myco --label api-prod --env prod
```

Rows from an env-bound token land at the four-segment path
`/log/myco/<env>/<service>/<kind>.log`, for example
`/log/myco/prod/api/main.log`. A token with no env lands at the
three-segment path `/log/myco/<service>/<kind>.log`. Reading the
three-segment path `/log/myco/api/main.log` covers that service across
every env. The `<kind>` segment is always present and defaults to
`main`; telemetry streams use other kinds (`metrics.log`, and
`heartbeats.log` or `runtime.log` when the agent emits them). See **Query your logs** for
how `ENV` surfaces in awk and jq.
