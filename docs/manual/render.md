---
id: manual-render
title: Render
web:
  section: connect-render
  category: connect-source
  order: 37
---

## Connect a Render service

Weft accepts logs directly from Render's built-in **Log Streams** feature using
syslog over TLS. This page covers the Render-specific parts; for an overview, see [Connect a log source](connect-source.md).

### 1. Create a token

Create a namespaced token as described in [Connect a log
source](namespaces.md#creating-a-token). There's nothing special about Render
that requires its own token, but it's probably tidier if it's got its own.

### 2. Add the log stream in Render

In the Render dashboard, click **Integrations → Observability** in the sidebar.
Scroll down to **Log Streams**, add a stream, and configure a generic/custom
syslog destination. Paste in the token from Step 1, along with the endpoint
(`render.syslog.<your-domain>:6514`).

Render starts streaming within a few seconds. If you want nice, speedy lookups,
[approve a schema](connect-source.md#sampling-logs-for-a-schema). Render
schemas are per service, so it covers the web, cron and build files under it.

## Finding your logs

Weft maps Render services to log files as follows:

| Render source                       | weft file                                  | metavariables |
|---|---|---|
| `myservice` web instance `web-9nk6q` | `/log/<ns>/myservice/web.log`  | instance=9nk6q |
| `myservice` cron job `cron-stqq5`    | `/log/<ns>/myservice/cron.log`                   | instance=stqq5 |
| `myservice` build output `bld-4fk2b`   | `/log/<ns>/myservice/bld.log`                    | instance=4fk2b |

(The presence of the `<env>` segment depends on how you created your token.
See [Paths](query-logs.md#paths) for the layout rules.)

## Render metrics

If you have a Pro Plan, you can ship system metrics from Render to weft too.

### Add the metrics stream in Render

Create or recycle a [token](namespaces.md#creating-a-token). Then in the Render
dashboard, click **Integrations → Observability**, scroll to **Metrics
Stream**, and click **+ Add destination**. Choose **Custom** and enter:

| Field | Value |
|---|---|
| Endpoint | `https://<your-api-host>/api/ingest/otlp` |
| API key | your weft token |

### Mapping render metrics to log files in weft

Metrics are split into the files `(host|network|disk).log` in the directory for
the sending Render service according to the rules above. Weft ingests the
following fields:

| Render service | weft file | fields |
|---|---|---|
| `myservice`, CPU | `/log/<ns>/myservice/host.log` | `cpu.cores`, `cpu.time_s` |
| `myservice`, memory | `/log/<ns>/myservice/host.log` | `memory.used_mb`, `total_mb`, `pct`, `rss_mb`, `cache_mb` |
| `myservice`, network | `/log/<ns>/myservice/network.log` | `rx_bytes_total`, `tx_bytes_total` |
| `myservice`, persistent disk | `/log/<ns>/myservice/disk.log` | `used_gb`, `total_gb` |

Other metrics that don't map to values collected by weftd (HTTP request
counts and latency, Postgres and Key Value internals) are discarded.
