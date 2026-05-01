# Plan: HTTP ingest — generic JSON + vendor adapters

A single coordinator-side ingest pipeline (auth → schema bootstrap → drain → shard) shared by an adapter-pluggable family of HTTP endpoints. Three adapters at v1: a generic JSON/ndjson endpoint for SDKs and `curl`, a Heroku log-drain endpoint for customers running on the Heroku platform, and (already specified in [`cloudwatch-lambda-forwarder.md`](cloudwatch-lambda-forwarder.md)) a CloudWatch subscription-filter endpoint.

## Why

Today the write path has two shapes — weftd's gRPC `IngestStream` for hosts the customer controls, and the CloudWatch poller for AWS-resident logs. Anything else has no supported route in: a Django app on Heroku, a Cloudflare Worker, a Raspberry Pi, an OpenTelemetry collector, a webhook source. A Heroku-hosted customer in particular *cannot* run weftd (dynos are ephemeral and can't sidecar) and isn't on AWS, so neither existing path applies.

A generic `POST /api/ingest` covers the curl/SDK/collector cases. A Heroku-specific endpoint at the same plumbing covers the Heroku case. The plumbing is identical; only the wire-format parser and the auth shape differ. Building both simultaneously costs little extra over building either alone, because the shared 80% is what matters.

## Architecture

```
client                                                    adapter
─────                                                     ────────
SDK / curl / Vector / Fluent Bit / OTel  → /api/ingest                    → JSONAdapter
heroku drains:add … (Heroku Logplex)     → /api/ingest/heroku/{token}     → HerokuAdapter
CloudWatch subscription filter           → /api/ingest/cloudwatch         → CloudWatchAdapter
                                                  │
                                                  ▼
                                       Adapter.Auth(req)  ── token lookup, customer/namespace/env binding
                                       Adapter.Parse(body)── decode wire format, emit []*pb.LogRow
                                                  │
                                                  ▼
                                       shared pipeline:
                                         - schema bootstrap on first batch per (customer, app)
                                         - env stamp from token (per ingest-dimensions.md)
                                         - rate limit (events/sec, bytes/sec) per token
                                         - usage metering (events + bytes in)
                                         - RingBuffer → existing StartDrain → INGESTING shard
```

No new transport between coordinator and shards; the adapters are source-side translators whose output drops into the same drain pipeline weftd's gRPC stream already feeds.

## Adapter interface

```go
type Adapter interface {
    // Auth resolves the request to a token. Different adapters use
    // different conventions: JSONAdapter reads `Authorization: Bearer`,
    // HerokuAdapter reads the {token} URL path component, CloudWatch
    // reads Bearer too (the Lambda forwarder controls the header).
    Auth(req *http.Request) (token string, err error)

    // Parse decodes the body into LogRows. Adapters do not stamp
    // customer/namespace/env — the shared handler does that after Auth
    // resolves the token, so the adapter never has to know which fields
    // are token-derived.
    Parse(body io.Reader, headers http.Header) ([]*pb.LogRow, error)

    // ContentLimit caps body size pre-decompression for this endpoint.
    // 1 MB default; CloudWatch and Heroku declare their own (they have
    // platform-side caps already, no point being more permissive).
    ContentLimit() int64
}
```

The shared handler (`coordinator/service/ingest_http.go`) wires it up:

```go
func makeIngestHandler(a Adapter, deps ...) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
        token, err := a.Auth(req)
        if err != nil { writeAuthErr(w, err); return }
        info, err := lookupToken(token)        // existing agent_tokens path
        if err != nil { writeAuthErr(w, err); return }

        if !rateLimit.Allow(token) { write429(w); return }

        rows, err := a.Parse(io.LimitReader(req.Body, a.ContentLimit()), req.Header)
        if err != nil { writeBodyErr(w, err); return }

        for _, r := range rows {
            r.Customer = info.Customer
            if info.Env != "" { r.Env = info.Env }
            r.App = enforceNamespace(info.Namespace, r.App)   // same check IngestStream uses
            // schema bootstrap on first batch per (customer, app)
        }
        coord.Ingest(rows)
        meter.Add(token, len(rows), bodyBytes)
        w.WriteHeader(http.StatusNoContent)
    })
}
```

## JSONAdapter — generic SDK / curl / collector intake

Body shapes accepted:

- `application/x-ndjson` — one JSON object per line, streamed.
- `application/json` — single object, or `{"events": [...]}`.
- `Content-Encoding: gzip` / `deflate` decoded.

Per-event keys:

- `app` — required; bound to a sub-app within the token's namespace.
- `ts` — RFC3339 or Unix ms; absent → ingest time.
- `level`, `message` — surface as `level` and `raw`.
- Anything else — merged into payload JSON.

Auth: `Authorization: Bearer wft_…`.

A thin Python SDK (~50 lines: bearer-auth session, batching, gzip, retry) ships alongside on the existing release pipeline (`.kevcons/ext/release.lua`).

## HerokuAdapter — Heroku log drain intake

Customer flow:

```
$ weft token create --namespace myco --env prod
issued: wft_…

$ heroku drains:add "https://drain.w3ft.net/api/ingest/heroku/wft_…" -a myapp
$ heroku drains:add "https://drain.w3ft.net/api/ingest/heroku/wft_…" -a sister-app
```

One token per namespace. Customers can put many Heroku apps behind one token the same way a non-Heroku customer puts many services in one namespace — that's their grouping decision. Separate envs need separate tokens (token binds env at mint time), which is the only constraint.

Wire format: Heroku's HTTPS drain protocol — RFC5424 syslog frames in octet-counted multi-frame bodies (RFC 6587 framing). One POST carries up to ~100 frames; ~10 KB per frame; bodies are typically well under the 1 MB cap. Auth via the URL path component (`{token}`); Heroku doesn't speak Bearer headers.

Per-frame field extraction:

| RFC5424 field | LogRow field | Notes |
|---|---|---|
| `timestamp` | `ts`        | already RFC3339 |
| `appname`   | (split key) | Heroku app, e.g. `myapp` |
| `procid`    | (split key) | dyno or platform component, e.g. `web.1`, `router`, `heroku-postgres`, `api` |
| `msgid`     | dropped     | always `-` from Heroku |
| `message`   | `raw`       | the actual line |
| `priority`  | dropped     | facility/severity not used |

**Sub-app split on procid.** Each (appname, procid) pair becomes its own weft `app`:

| Heroku source           | weft `app`              |
|---|---|
| `myapp web.1`           | `myapp/web`             |
| `myapp web.2`           | `myapp/web`             |
| `myapp worker.1`        | `myapp/worker`          |
| `myapp router`          | `myapp/router`          |
| `myapp api`             | `myapp/platform`        | (Heroku platform events: deploys, restarts, scales) |
| `myapp heroku-postgres` | `myapp/heroku-postgres` |

Dyno number (`.1`, `.2`) is dropped from the app name and stamped on `instance` instead, so cross-dyno aggregation stays cheap. The pseudofile layout for a customer reads naturally:

```
weft> ls /log/myco/prod/myapp
heroku-postgres.log
metrics.log              ← l2met auto-routing, see below
platform.log
router.log
web.log
worker.log
```

**l2met metric line auto-routing.** Heroku's runtime-heroku-metrics labs feature emits log lines in l2met format:

```
source=web.1 dyno=heroku.123.uuid sample#memory_total=512.00MB sample#memory_rss=487.21MB sample#memory_pgpgin=...
```

The HerokuAdapter detects lines matching `\bsource=\S+\b.*?\b(sample|measure|count|bucket)#` and rewrites them to `app=<heroku-app>/metrics`, with the key/value pairs parsed into payload JSON (number-typed where the value is a number). The `source=` value populates `instance`. Mirrors the existing weftd-side metrics pseudofile shape so `cat /log/myco/prod/myapp/metrics.log | jq '.memory_total > 400'` works without custom client awk.

Heroku Postgres / Redis / etc. add-ons emit similarly-structured metric lines and route to their own `<app>/<addon>` sub-paths, which the procid-based split already produces.

## CloudWatchAdapter — see [`cloudwatch-lambda-forwarder.md`](cloudwatch-lambda-forwarder.md)

That plan defines the wire format (CloudWatch Logs subscription event JSON), the customer-deployed Lambda, and the Bearer-token auth shape. Once this plan lands, the existing CloudWatch handler is recast as `CloudWatchAdapter` with no behaviour change — it slots into the same `Adapter` interface and shares the same auth/rate-limit/metering plumbing the other two adapters use.

## Components to build

1. **`coordinator/service/ingest_http.go`** — shared handler factory taking an `Adapter`. Wires auth, token lookup, schema bootstrap, env stamp, namespace check, drain hand-off, rate limit, metering. Thin glue.
2. **`coordinator/service/adapter_json.go`** — JSONAdapter: ndjson + JSON shapes, gzip/deflate, Bearer header auth.
3. **`coordinator/service/adapter_heroku.go`** — HerokuAdapter: RFC5424 octet-counted framing parser, per-frame extraction, l2met line detection, procid sub-app split, URL-path token auth.
4. **`coordinator/service/adapter_cloudwatch.go`** — recast of the existing CloudWatch path into the Adapter interface (mostly file move + signature change).
5. **Schema bootstrap reuse** — first batch per `(customer, app)` goes through the existing onboarding flow (`weftd.CheckSourceStatus` → `SubmitSample` → `WaitForConfiguration`); subsequent batches stream straight to drain. Same pattern weftd uses today.
6. **Rate limit + bytes-in metering** — `rate.Limiter` keyed on token, default 1000 events/sec per token, per-tier caps in `store/user.go`. Bytes-in count feeds `usage_hourly` alongside the event count, on the same code path weftd's gRPC stream uses for byte accounting. Over-limit returns 429 with `Retry-After`.
7. **`docs/manual/http-ingest.md`** — generic curl recipe, gzip/ndjson examples, per-tier QPS caps.
8. **`docs/manual/heroku.md`** — two-command customer flow (`weft token create` + `heroku drains:add`), the procid → sub-app mapping table, the l2met → metrics auto-routing note, the "you don't run weftd on Heroku" framing.
9. **Reference Python SDK** — `clients/python/weft/__init__.py`, ~50 lines wrapping the JSON endpoint. Bearer-auth session, batching, gzip, retry. Ships to PyPI on the existing release pipeline.

## Cost and rate-limit posture

`ingest.w3ft.net` (and aliases `drain.w3ft.net` for Heroku) point at the coordinator ALB. v1 limit of 1000 events/sec per token is generous for small-to-midsize apps; bigger customers either run weftd (gRPC, no per-request overhead) or negotiate a higher cap.

Bytes-in is metered on every adapter, in the same `usage_hourly` row weftd's gRPC stream already updates. A customer's monthly bill reflects total ingress regardless of which adapter delivered the bytes — no asymmetry between weftd and HTTP paths. Heroku's drain protocol does platform-side retries on 5xx, so transient 429s are absorbed; 4xx (auth, parse) are terminal and surface in `heroku drains` as delivery failures.

Body-size caps per adapter (pre-decompression):

| Adapter     | Cap  | Notes                                             |
|---|---|---|
| JSON        | 1 MB | typical SDK batch is under 100 KB                 |
| Heroku      | 1 MB | Heroku caps drain payloads at ~1 MB; matching cap |
| CloudWatch  | 6 MB | Lambda payload limit; matches forwarder defaults  |

## Estimated effort

- Shared `ingest_http.go` handler factory + Adapter interface + token/rate/meter glue: **1 day**
- JSONAdapter + tests: **0.5 day**
- HerokuAdapter (RFC5424 parser + procid split + l2met detection) + tests: **1.5 days**
- CloudWatchAdapter recast (mostly file moves, behaviour unchanged): **0.5 day**
- Schema bootstrap reuse: **0.5 day**
- Rate limit + bytes-in metering: **0.5 day**
- Manual: http-ingest + heroku pages: **1 day**
- Python SDK: **0.5 day**

**Total: ~5.5 days** for all three adapters and the supporting docs/SDK. The Go SDK is defined separately in [`go-sdk.md`](go-sdk.md) — same destination endpoint, no new work here.

## Non-goals

- **Not a query API.** Reads stay on gRPC and the existing REST surface.
- **Not OTLP-gRPC.** JSON over HTTPS only — every collector can emit JSON, and a second transport doubles the auth surface for no gain.
- **Not OTLP semantic-convention attribute promotion.** Mapping `service.name` / `cloud.region` / `k8s.pod.name` etc. onto reserved columns was previously sketched here and was deliberately dropped from `docs/completed/ingest-dimensions.md`. Customers shipping OTel-flavored payloads put those keys in payload JSON like any other field; if they want column-fast filtering on those values, they translate at the collector before sending.
- **Not asynchronous ack.** Sync 200/4xx/5xx; clients buffer on failure. Matches how every HTTP SDK already behaves.
- **Not per-event ack IDs or exactly-once.** Best-effort at-least-once. Heroku does its own retries; SDKs do theirs; weft accepts duplicates and lets users dedupe at query time when it matters.
- **No client-set `customer` or `env`.** Both are derived from the token. JSONAdapter ignores any top-level `customer`/`env` fields; HerokuAdapter has no equivalent fields to ignore.
- **No vendor adapter beyond what's listed here.** Render, Fly.io, Vercel, etc. each get their own narrow adapter when a customer asks. The Adapter interface keeps the cost of adding one low (~1 day), but we don't pre-build them.
