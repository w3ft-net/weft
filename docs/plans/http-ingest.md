# Plan: Generic HTTP ingest endpoint

A single JSON-over-HTTPS intake that anything not-weftd-and-not-CloudWatch can target: curl from a shell script, language SDKs, Vector sinks, Fluent Bit HTTP outputs, OpenTelemetry Collector's `otlphttp` logs exporter (with a thin translation), Lambda forwarders, webhook-style event sources.

## Why add this

Today the write path has exactly two shapes: weftd's gRPC `IngestStream` for agents, and the CloudWatch poller for logs already in AWS. Anything else — a Django app on Heroku, a Cloudflare Worker, a Raspberry Pi on someone's desk — has no supported way in. The adjacent `docs/plans/cloudwatch-lambda-forwarder.md` defines a CloudWatch-shaped `POST /api/ingest/cloudwatch`; this plan generalizes that idea to the primary ingest route.

Once the endpoint exists, every "but how do I use weft from Python?" question collapses to "POST NDJSON to this URL with your token." A thin Python SDK is ~50 lines on top of it.

## Architecture

```
client (curl / SDK / Vector / Fluent Bit / OTel exporter / Lambda)
  → POST https://ingest.w3ft.net/api/ingest
    Authorization: Bearer wft_...
    Content-Type: application/x-ndjson   (or application/json)
    Content-Encoding: gzip | deflate     (optional)
    body: one JSON object per line, or {"events": [...]} wrapper
  → coordinator HTTP handler
    → auth (reuse agent-token table)
    → schema lookup / auto-detect on first batch
    → ingest.Parser → RingBuffer → existing StartDrain path
```

No new transport between coordinator and shards. The endpoint is a source-side adapter whose output drops into the same drain-to-INGESTING-shard pipeline weftd's gRPC stream already feeds.

## Components to build

1. **`coordinator/service/ingest_http.go`** — new handler on the coordinator HTTP port.
   - Auth: reuse `AgentToken` lookup (see `coordinator/service/auth_test.go` for the existing pattern). The token binds to customer + namespace; the body's `app` field binds to the source within that namespace.
   - Body parsing: `application/x-ndjson` (one JSON object per line, streaming), `application/json` (single object), `application/json` with `{events: [...]}`. gzip + deflate decode.
   - Per-event shape: `{app, ts?, level?, message, ...extras}` where extras are merged into the payload. `ts` is RFC3339 or Unix ms; absent → ingest time.
   - Token-derived dimension stamping: if the token carries `env` (per `docs/plans/ingest-dimensions.md`), the handler sets the `env` column on every row before drain — same lookup the gRPC `IngestStream` path uses. No client-side env field is honored.
2. **OTel attribute normalization.** Body extras whose keys match the OpenTelemetry semantic-convention attribute names get promoted to the matching weft column instead of landing in payload JSON:
   - `service.name` → `app` (overrides any `app` field at top level if both are present)
   - `service.version` → `version`
   - `deployment.environment.name` → `env` (only if the token doesn't already carry an env; token wins)
   - `cloud.region` → `region`
   - `host.name` → `host` (the existing `instance` column)
   - `k8s.cluster.name` → `cluster`
   - `k8s.namespace.name` → `k8s_namespace`
   - `k8s.pod.name` → `pod`
   - `k8s.container.name` → `container`
   - `k8s.deployment.name` → `deployment` (canonical)
   - `aws.autoscaling.group.name` → `deployment` (no OTel canonical exists; reuse the deployment column)
   - `k8s.daemonset.name`, `k8s.statefulset.name` → `deployment` (fall-through when no Deployment is present; first-match-wins among the three k8s ancestors)
   - Unmapped OTel attributes (and everything else) land in payload like any other JSON field. The mapping table is the only OTel concession; no full semantic-convention parser, no resource-attribute envelope handling beyond key matching.
3. **Schema bootstrap** — first batch per `(customer, app)` goes through the same onboarding path weftd uses (`weftd.CheckSourceStatus` → `SubmitSample` → `WaitForConfiguration`). Subsequent batches stream directly to the drain.
4. **Rate limiting** — `rate.Limiter` keyed on token. Default 1000 events/sec per token, per-tier caps in `store/user.go`. Over-limit returns 429 with `Retry-After`.
5. **Metering** — byte and event counts feed the existing `usage_hourly` table alongside weftd and poll.
6. **`docs/manual/http-ingest.md`** — curl recipe, schema auto-detect note, per-tier QPS caps, gzip/ndjson examples, OTel attribute mapping table.
7. **Reference Python SDK** — `clients/python/weft/__init__.py`, ~50 lines: bearer-auth session, batching, gzip, retry. Ships to PyPI on the existing release pipeline (`.kevcons/ext/release.lua`).

## Cost and rate-limit posture

The `ingest.w3ft.net` hostname points at the coordinator ALB. A v1 limit of 1000 eps per token covers every realistic small-to-midsize app; bigger customers either run weftd (gRPC, no per-request overhead) or negotiate a higher cap. Over-budget bursts return 429 immediately rather than enqueue — no hidden tail.

## Estimated effort

- Handler + auth + body parsing: **1 day**
- OTel attribute normalization (table + tests): **0.5 day**
- Schema onboarding reuse: **0.5 day**
- Rate limit + metering: **0.5 day**
- Manual + curl recipe + OTel mapping table: **0.5 day**
- Python SDK: **0.5 day**

**Total: ~3.5 days** to first working version; another day for a Vector / Fluent Bit / OTel collector recipe and a second SDK (Node or Go).

## Non-goals

- **Not a query API.** Reads stay on gRPC and the existing `api/` REST surface.
- **Not OTLP-gRPC.** JSON over HTTPS only — every collector can emit JSON, and a second transport doubles the auth/ingest surface for no real gain.
- **Not asynchronous ack.** Response is a sync 200/4xx/5xx; clients do their own buffering on failure. Matches how HTTP SDKs already behave.
- **Not per-event ack IDs or exactly-once.** Best-effort at-least-once, dedup is the client's problem.
