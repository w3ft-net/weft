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
(`render.syslog.w3ft.net:6514`).

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

Render's **Metrics Streams** feature pushes your services' system metrics to a
destination you choose. Weft accepts it over OpenTelemetry (OTLP/HTTP with a
JSON body) and keeps the system metrics: CPU, memory, network and disk. Metrics
Streams needs a Pro workspace or higher, and Render sends nothing for static
sites, cron jobs or one-off jobs.

### Add the metrics stream in Render

Use a [token](namespaces.md#creating-a-token); the one you made for logs works.
In the Render dashboard, click **Integrations → Observability**, scroll to
**Metrics Stream**, and click **+ Add destination**. Choose **Custom** and enter:

| Field | Value |
|---|---|
| Endpoint | `https://api.w3ft.net/api/ingest/otlp` |
| API key | your weft token |

Render adds `/v1/metrics` to the endpoint itself, so leave the path off. Samples
arrive every 15 seconds per instance.

### Metrics land in `host.log`, `network.log` and `disk.log`

Metrics go under the same service as that service's logs, as the kinds
`host`, `network` and `disk`. A compute instance has the same `$host` in its
metrics as in its logs, and the service's Render project is its `$cluster`.

| Render service | weft file | fields |
|---|---|---|
| `myservice`, CPU | `/log/<ns>/myservice/host.log` | `cpu.cores`, `cpu.time_s` |
| `myservice`, memory | `/log/<ns>/myservice/host.log` | `memory.used_mb`, `total_mb`, `pct`, `rss_mb`, `cache_mb` |
| `myservice`, network | `/log/<ns>/myservice/network.log` | `rx_bytes_total`, `tx_bytes_total` |
| `myservice`, persistent disk | `/log/<ns>/myservice/disk.log` | `used_gb`, `total_gb` |

Render sends each metric on its own schedule, so one sample is spread over
several rows and a row only holds the fields that arrived together. A field a
row doesn't have reads as `null`, so filter for the one you want:

```
weft sh
$ tail /log/myco/myservice/host.log | jq 'select(.memory.pct) | {host: $host, pct: .memory.pct}'
```

`cpu.cores` is the instance's CPU allowance (0.5 on a 512 MB instance, for
example). Memory `pct` is the share of the instance's memory limit. A disk row
has either `used_gb` or `total_gb`, and no mount path: Render reports a disk
under its own id, which is that row's `$host`.

### CPU time and network totals are running counters

`cpu.time_s`, `rx_bytes_total` and `tx_bytes_total` are cumulative, counting
from when the instance started, like Render's own metrics. They are stored as
Render sends them, so to see CPU use or traffic over time you work out the
change between samples. A cron can do it per instance. This one reports each
instance's CPU as a percentage of its allowance, over the cron's window:

```
# /cron.d/render-cpu
# schedule: */5 * * * *
cat /log/myco/myservice/host.log \
  | jq -r 'select(.cpu.time_s) | "\($ts | sub(" ";"T") + "Z" | fromdate) \($host) \(.cpu.time_s) \(.cpu.cores)"' \
  | awk '
      {
        t = $1; h = $2; c = $3; cores[h] = $4
        if (h in prevc) {
          d = c - prevc[h]
          if (d < 0) d = c              # the instance restarted
          used[h] += d
          span[h] += t - prevt[h]
        }
        prevc[h] = c; prevt[h] = t
      }
      END {
        for (h in used)
          if (span[h] > 0 && cores[h] > 0)
            printf "%.2f %s\n", used[h] / span[h] / cores[h] * 100, h
      }' \
  | alert above 90 -v 1 -t 2 --notify=ops
```

Each point is tagged with its instance, so you get one alert per instance, and
a chart of the cron draws one line per instance. Memory needs none of this,
since it isn't a counter:

```
cat /log/myco/myservice/host.log \
  | jq -c 'select(.memory.pct) | {host: $host, pct: .memory.pct}' \
  | alert above 90 -v .pct -t .host --notify=ops
```

### What weft keeps

Weft keeps CPU, memory, network and disk usage. The rest of what Render's
stream carries is dropped when it arrives, without an error: HTTP request
counts and latency, and the Postgres and Key Value internals (connections,
checkpoints, replication lag and so on).
