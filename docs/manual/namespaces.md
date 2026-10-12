---
id: manual-namespaces
title: Namespaces
web:
  section: namespaces
  category: getting-started
  order: 10
---

## Create a namespace

Namespaces group related log sources. A typical setup has one namespace per
environment (e.g. `production`, `staging`) or per team.

Go to **Namespaces** in the sidebar and click **Create namespace**. Choose a
short, lowercase name — it becomes part of token identifiers and CLI scoping.

```
# From the shell
namespace create production
```

## Log path layout

Every log stream is addressed by a pseudofile path under `/log/`.
The namespace is the first segment; the rest of the path names the
service and stream kind, with an optional env segment for tokens
that carry one:

```
/log/<namespace>/<service>/<kind>.log          env-less token
/log/<namespace>/<env>/<service>/<kind>.log    env-bearing token
```

- **`<namespace>`** — the grouping you created above. A single
  `/log/<namespace>.log` is a namespace-wide wildcard (every
  service and kind under it).
- **`<service>`** — the source name a producer reports for itself;
  surfaces as `SERVICE` in queries.
- **`<env>`** — present only for tokens minted with `--env`. It
  slots in immediately after the namespace, so an env-bearing path
  has four segments and an env-less one has three. A four-segment
  path reads exactly one env. A three-segment path reads that
  service across every env, the env-less stream included, and
  `ls /log/<namespace>/<service>` lists the kinds any of them
  carry — so a reader token spanning several envs never has to
  guess.
- **`<kind>`** — the stream: `main` for ordinary application logs
  (the default a producer ships unless it says otherwise), plus
  `heartbeats`, `metrics`, and `runtime` for services that emit
  telemetry. It is always present.

### Where the service name comes from

Each producer decides the service name; weft never rewrites it.

| Producer | `<service>` | `<kind>` |
|---|---|---|
| weftd | The source's `service:` setting. If omitted: the systemd `unit:` name without `.service`, else the log file's name without its extension. | `main` unless the source sets a kind |
| Heroku | The Heroku app name | The dyno process type (`web`, `worker`, ...); platform events go to `platform` |
| Render | The Render service slug | The process type (`web`, `cron`, `bld`, ...) |
| CloudWatch | The service you registered for the log group with `weft source add --service`. Events from unregistered log groups are dropped. | `main` |

A namespace can hold env-less and env-bearing streams side by side
as different tokens reauth over time. Env, region, deployment,
host, and pod are query-time dimensions (`awk 'ENV == "prod"'`),
not extra path segments — see **Meta-variables** for the full set.
