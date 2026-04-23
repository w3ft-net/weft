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
