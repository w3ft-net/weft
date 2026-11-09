# Plan: Render system metrics (OTLP ingest, truncated to host metrics)

Render's metrics streams push OpenTelemetry-formatted service metrics to a
customer-supplied endpoint with a bearer token. This plan adds a receiver for
that stream and keeps only **system metrics** (cpu, memory, network, disk).
Everything else in the stream is dropped, silently.

Status: Phase 0 (wire capture) done 2026-11-08 for a web service; Phase 3 (weftd
cumulative keys) shipped in weftd 0.33.0 (commit 756d6ee8). Phase 1 (adapter) and the route are
implemented; rate limiting and the manual page remain.

## Principle

**Ingest stores what the producer sent.** Gauges are stored as gauges.
Cumulative counters (`cpu.time`, network bytes) are stored as cumulative
values. The receiver keeps no state across samples and derives nothing. Turning
a counter into a rate is the consumer's job; Render's own docs say to "apply a
`rate()` function or similar in your observability provider".

## What Render documents

Source: render.com/docs/metrics-streams and /metrics-streams-reference.

- Pro plan or higher. A workspace admin adds a Custom destination under
  Integrations > Observability > Metrics Stream.
- "All metrics use OpenTelemetry JSON format." Auth is an API key as a bearer
  token in an `Authorization` header.
- No metrics for static sites, cron jobs or one-off jobs.
- Resource attributes: `service.name`, `service.id`, `service.instance.id`;
  optional `service.project`, `service.environment`.

## Wire capture results (2026-11-08, web service, Starter-size instance)

Receiver: throwaway HTTPS endpoint with a bearer token, base URL with no path.

- **Transport**: OTLP/HTTP, JSON body, `POST /v1/metrics`. Render appends
  `/v1/metrics` to the configured base URL. Not gRPC. Request bodies were not
  gzipped (`Accept-Encoding: gzip` is for our response). User agent
  `render-otel-exporter/1.0`. 200 with `{}` is accepted.
- **Connection**: one keep-alive connection carried every request from the
  stream (about 13 requests, one connection).
- **Cadence**: a sample every **15 seconds** per instance, delivered across
  several requests a few seconds apart. Timestamps are `timeUnixNano` as a
  string; values are `asDouble`.
- **A tick is split across requests.** One resource-metrics entry holds one
  metric or a pair, and the pairs are always the same: (`cpu.time`,
  `cpu.limit`) and (`memory.usage`, `memory.limit`). `memory.rss`,
  `memory.cache` and each network counter come alone. Entries for one
  timestamp arrive in different requests, and sometimes out of order. A
  stateless receiver cannot count on seeing a whole tick.
- **OTLP instrument types are wrong, so the name table must not trust them**:
  - `cpu.time` and both network counters are sent as `gauge` but are cumulative
    (e.g. `cpu.time` 831.0936, 831.0990, 831.1022, 831.1037 over 45 s).
  - `cpu.limit` is sent as `sum` with cumulative temporality and unit
    `seconds`, but is the CPU allotment: **0.5** for a 512 MiB instance. So
    `cpu.limit` is cores, as assumed.
  - `memory.limit` is 536870912 (bytes); `memory.cache` was 0.
- **Resource attributes**: `service.name`, `service.id`
  (`srv-...`), `service.instance.id` (`<service.id>-<suffix>`),
  `service.project`, `service.environment` (value was `Production`).
  Metric instances must match the log rows' instance (see Instance ids).
- **Persistent disk** (attached to the test web service):
  `render.service.disk.capacity` and `render.service.disk.usage`, both
  `gauge`, unit `bytes`, no data-point attributes. They arrive as their own
  resource with `service.id` and `service.instance.id` both set to the disk id
  (`dsk-dbokb5ou01pc73fhr4ag`), `service.name` the owning service's name
  (`render-capture-testsvc`), and the same project/environment. There is **no
  mount path** and no link to a compute instance. Observed values:
  capacity 10464022528, usage 28672.
- **Names and instance ids match the log rows.** Checked against the syslog
  ingest for the same service (`/log/render-test/render-capture-testsvc/web.log`):
  `$host` there is `j9fbv` and `k22qt`, exactly the suffixes of the metrics
  instance ids `srv-dac2h63m8hqs73e9h330-j9fbv` and `...-k22qt`, and the
  service name is identical. A redeploy (the disk attach) produced a new
  instance, `k22qt`, in both streams.
- **Not captured on purpose**: Postgres, Key Value, HTTP metrics. Their shapes
  are unverified and the adapter drops them (except any `disk.*` from a
  datastore, which would pass through the same name table).

## Row granularity (decided)

**One row per resource-metrics entry**, stateless. Render splits a tick across
requests, so the receiver never assembles ticks:

- the (`cpu.time`, `cpu.limit`) entry becomes a `cpu` row: `cores`, `time_s`
- the (`memory.usage`, `memory.limit`) entry becomes a `memory` row: `used_mb`,
  `total_mb`, `pct`
- `memory.rss`, `memory.cache`, `network.receive.bytes` and
  `network.transmit.bytes` each become their own row (`rss_mb`, `cache_mb`,
  `rx_bytes_total`, `tx_bytes_total`)

That is about 6 rows per instance per 15 s, against weftd's 1 per 30 s, and
queries see sparse rows: a field absent from a row reads as null. A metric
that arrives alone is never held back waiting for a partner.

## Scope

Kept (every service type that emits them):

| Render metric | Kind | Stored as |
|---|---|---|
| `service.cpu.limit` | value (sent as a cumulative `sum`) | `cpu.cores` |
| `service.cpu.time` | cumulative seconds (sent as a `gauge`) | `cpu.time_s` |
| `service.memory.usage` / `rss` / `cache` / `limit` | value | `memory.used_mb`, `rss_mb`, `cache_mb`, `total_mb`; `pct` = used / total |
| `service.network.receive.bytes` / `transmit.bytes` | cumulative bytes (sent as `gauge`) | `network.rx_bytes_total`, `tx_bytes_total` |
| `service.disk.usage` / `capacity` | value | `disk` rows: `used_gb` and `total_gb`. They arrive in separate entries, so they are separate rows and carry no `pct`. No `path` (Render does not send a mount path); the row's instance is the disk id suffix. Verified for a persistent disk on a web service; Postgres and Key Value are not captured, so their shape is unverified. |

Dropped: HTTP request metrics, Postgres and Key Value internals, disk I/O
counters, and anything not in the table.

## Architecture

A new `OTLPAdapter` on the existing `Adapter` interface (`api/adapter.go`):
`Auth` reads the bearer token, `ContentLimit` bounds the body, `Parse` returns
`LogRow`s. The decoder is generic OTLP; a table keyed by metric name decides
what is kept and how it maps. Render is the first table, so adding another
platform or OTel `system.*` names is a data change.

Tokens come from the agent-token store scoped by producer, as the Render syslog
listener does. Namespace and env come from the token; the adapter never sets
them from the payload.

Rows use weftd's kinds (`host`, `disk`, `network`) with one wider schema where
a key a producer cannot supply is null. weftd keeps computing its own
percentages and rates agent-side and now also emits the cumulative keys, so
one query works on both producers. Render rows carry only the cumulative keys:

| Key | weftd | Render |
|---|---|---|
| `cpu.cores`, `memory.*` | yes | yes |
| `cpu.pct`, `user`, `system`, `idle`, `iowait` | yes | null |
| `cpu.time_s` (cumulative) | yes (new) | yes |
| `network.*_bytes_sec` (rates) | yes | null |
| `network.*_bytes_total` (cumulative) | yes (new) | yes |

### Attribute mapping

Resource attributes map onto the reserved columns
(`docs/completed/ingest-dimensions.md`):

| Render attribute | weft column |
|---|---|
| `service.name` | `service` |
| `service.instance.id` | `instance` / `host`, via the Render producer's split (see below) |
| `service.project` | `cluster` |
| `service.id` | payload field `render_service_id` |
| `service.environment` | not mapped; `env` is token-bound and never set from payload |

**Instance ids reuse the Render log producer's code** (verified against real
log rows; see the capture results). `RenderProducer` already
turns `web-9nk6q` into instance `9nk6q` with `splitKindInstance` (split on the
last `-`), in `ingest/syslog/producer_render.go`. A metrics
`service.instance.id` of `srv-dac2h63m8hqs73e9h330-j9fbv` splits the same way
to `j9fbv`, so the adapter calls that function (exported or moved to a shared
Render helper) instead of reimplementing it, and logs and metrics for one
instance share `$host`. A disk's id (`dsk-dbokb5ou01pc73fhr4ag`) splits the same way, so a disk row's
instance is `dbokb5ou01pc73fhr4ag`; it is not a compute instance and will not
match any log row's `$host`.

`service.name` equals the service name the Render syslog producer derives from
the syslog HOSTNAME (verified), so a service's logs and metrics share
`<ns>/<service>`.

## Using the data

CPU busy is `delta(cpu.time_s) / (delta(ts) * cores)` per instance. Until the
shell has a counter-aware function, a cron can compute it. The jq and awk were
run in the shell against weft's own host metrics (using a non-counter field as
a stand-in) and the awk's reset handling was checked by hand; it has not run
against real Render rows. `$ts` renders as `2026-11-09 01:14:05.314348`, hence
the `fromdate`:

```
# /cron.d/render-cpu
# schedule: */5 * * * *
cat /log/myco/prod/api/host.log \
  | jq -r 'select(.cpu.time_s) | "\($ts | sub(" ";"T") + "Z" | fromdate) \($host) \(.cpu.time_s) \(.cpu.cores)"' \
  | awk '
      {
        t = $1; h = $2; c = $3; cores[h] = $4
        if (h in prevc) {
          d = c - prevc[h]
          if (d < 0) d = c              # counter reset
          used[h] += d
          span[h] += t - prevt[h]
        }
        prevc[h] = c; prevt[h] = t
      }
      END {
        for (h in used)
          if (span[h] > 0 && cores[h] > 0)
            printf "%.2f %s\n", used[h] / span[h] / cores[h] * 100, h
      }'
```

```
# /cron.d/render-cpu-sustained
# schedule: */5 * * * *
weft::avg render-cpu 15m | alert above 85 --notify=ops
```

For per-instance alerts, end the first cron with
`| alert above 90 -v 1 -t 2 --notify=ops` instead; alerts keep state per tag.

Memory and disk need none of this: they are plain values.

## Follow-up, not part of this plan

`weft::delta` and `weft::slope` ignore tags (they query by cron id only), so
they cannot do per-instance work, and `delta` has no counter-reset handling.
A tag selector on the raw-window aggregates plus an `increase`-style function
would let a cron store cumulative values directly and drop the awk above. That
is a shell/cron change that benefits any cumulative source; track it
separately.

## Phases

0. **Wire capture.** Done: web service and persistent disk. Receiver and its
   teardown list are below.
1. **Adapter.** Done: `api/adapter_otlp.go` (`OTLPAdapter`), tests in
   `api/adapter_otlp_test.go` against `api/testdata/render_otlp_metrics.jsonl`.
   While testing end to end it turned up a shared-path bug: enrichment resolves
   a log schema by service alone, so it re-parsed metric rows as log lines
   (overwriting level, payload and timestamp) and added them to the service's
   log sample. `enrichRows` and `accumulatePendingSamples` now skip rows whose
   adapter built the payload (`prebuilt` in `api/adapter.go`); this also
   covers Heroku's l2met metric rows, which had the same problem.
2. **Wiring.** Route `POST /api/ingest/otlp/v1/metrics` is registered in
   `api/handlers.go`; `api.w3ft.net` already fronts that mux, so there is no
   new listener, cert or Terraform. Auth is any weft agent token as the
   bearer. Rate limiting needs nothing new: every row goes through
   `IngestStream`, which already throttles per token
   (`coordinator/service/server.go`, `IngestTokenBytesPerSec`, 4 MiB/s with a
   16 MiB burst by default; see docs/ingestion-syslog.md, "Rate limiting"), and
   the HTTP route forwards with the caller's token. A request body is capped at
   1 MiB.
3. **weftd cumulative keys.** Committed in 756d6ee8 (version 0.33.0; the
   release is not cut yet). In `cmd/weftd/metrics.go`, add `cpu.time_s`
   (busy jiffies from `/proc/stat`, user + nice + system + irq + softirq +
   steal, divided by USER_HZ) and `network.rx_bytes_total` / `tx_bytes_total`
   (the counters `readNetDev` already reads) alongside the existing fields.
   Additive only. This is an agent change, so it needs a version bump and a
   release; the fleet self-updates from there. Independent of Phases 0 to 2
   and can ship first.
4. **Docs and live check.** A manual page: Render dashboard setup, the
   supported set, and what is dropped. No mention of warp. Verify with a real
   service: `cat /log/<ns>/<svc>/host.log`.

## Decisions on earlier open questions

- `service.project` maps to `cluster`.
- weftd keeps its percentages and rates and also emits cumulative values
  (`cpu.time_s`, `network.*_bytes_total`), so one query works on weftd hosts
  and Render services.
- Metrics outside the kept set are dropped with no error, counter or notice.
  The manual page must list what is kept and what is not.

## Capture infrastructure

Torn down 2026-11-08: instance `i-02f6f611fa99b3fbb` and security group
`sg-0fecfc9e8d5de43bd` (weft account). Still to remove in the management
account (zone `Z0125267195PS76VADCUE`): A records `render-metrics-capture.w3ft.net`
(52.14.89.9) and the stale `render-capture.w3ft.net` (18.116.60.71), and the
Custom metrics-stream destination in the Render workspace.

The fixture is `api/testdata/render_otlp_metrics.jsonl`: six real requests
(the connectivity test, one complete compute tick, a second instance, and the
disk metrics), one JSON object per line with `method`, `path`, `content_type`
and the parsed `body`. The bearer token is not in it. The receiver script was
not committed.

## Not in scope

HTTP request metrics, Postgres and Key Value internals, histograms, OTLP logs
and traces (Render sends none), OTLP/gRPC unless Phase 0 forces it.
