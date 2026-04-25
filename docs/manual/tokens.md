---
id: manual-tokens
title: Tokens
web:
  section: tokens
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

Tokens with env produce three-level pseudofile paths
(`/log/myco/prod/api.log`); tokens without env produce the original
two-level layout (`/log/myco/api.log`). A namespace can hold a mix
during a migration. See **Query your logs** for how `$ENV` surfaces
in awk and jq.
