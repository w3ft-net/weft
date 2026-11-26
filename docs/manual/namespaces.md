---
id: manual-namespaces
title: Namespaces & tokens
web:
  section: namespaces
  category: getting-started
  order: 10
---

## Namespaces

Namespaces group related log sources. We usually think if namespaces as buckets
for a single application, but it's your bucket, so do what you want. Namespaces
are as granular as permission grants get, so keep that in mind when laying out
your log sources. A namespace also owns its [crons](crons.md), and
every one comes with a read group, `<namespace>-r`, and a write group,
`<namespace>-w` (see [Teams & permissions](team.md)).

## Creating a namespace

To create a new namespace in your browser, go to **Namespaces** in the sidebar
and click **new**. Choose something easy to type for the name — it'll be in
the path of every log file you configure under it. 

Or, from the CLI:

```
weft namespace create babbage
```

To remove one, `weft namespace remove babbage`. It refuses while the namespace
still has sources or live tokens, and lists them; `--force` removes the
namespace and everything in it. Logs already stored age out under normal
retention.

## Tokens

Weft uses a simple token-based authentication for all log producers, whether
that be weftd, Render, Heroku, or Kinesis.  Each token is scoped to exactly one
namespace, but you can have as many as you like within a namespace. In fact, we
encourage it! You can optionally tag your tokens with an environment name,
allowing you to keep a staging, sandbox, and prod environment under the same
namespace but keep their logs separate.

You can always view your amassed sea of tokens under each namespace in the
browser, or in your terminal with `weft token list`. Creating a token needs the
namespace's write group.

## Creating a token

To create a token in the browser, go to **Namespaces**, select the namespace
you want the token to be in, and click **new token**. You can optionally bind a
environment tag (`prod`, `staging`, `qa`, etc.) and a label. What's the label
for? You tell me! It's nice to have options. The environment will have log file
path significance, whereas the label does not (but you can still filter on it).

From the CLI:

```
weft token create babbage --label api-prod --env prod
```

The token is only shown once regardless of where you create it, so put it
somewhere safe! If you lose it, revoke it and create a new one -- either by
clicking "revoke" in the browser or with `weft token revoke --id <id>` on the
command line.
