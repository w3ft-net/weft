# Plan: CloudWatch Lambda forwarder

An opt-in, push-based alternative to today's poll-based CloudWatch ingestion. Customer deploys a small Lambda in their AWS account that receives CloudWatch Logs subscription events and HTTPS-POSTs them to weft. Parallels the Datadog Forwarder model.

## Why add this

Today we offer exactly one CloudWatch story: weft assumes a cross-account role and polls `FilterLogEvents` every N seconds. The strengths (no customer-owned infra, no subscription-filter slot consumed, cheap on the customer's AWS bill) and weaknesses (30s+ latency floor, weft outages stall ingestion) are covered in `docs/ingestion-cloudwatch.md`.

Two customer segments are not well-served by polling:

1. **Latency-sensitive** — alert-to-page inside 5 seconds. weftd solves this if the customer controls the host; for Lambda, Fargate, API Gateway, RDS, etc. they don't.
2. **Multi-vendor cohabitation but push-preferring** — already on Datadog/Splunk via subscription filters, comfortable running a second forwarder Lambda, wants weft to behave the same way.

A Lambda-forwarder path covers both.

## Architecture

```
CloudWatch Log Group
  → Subscription filter (customer-owned)
    → weft-forwarder Lambda (customer-owned)
      → HTTPS POST /api/ingest/cloudwatch (ingest.w3ft.net)
        → Coordinator: auth, decompress, parse, route to shard
```

Bearer token (existing `wft_...` namespace-scoped token) authenticates the Lambda → coordinator POST. Same token mechanism weftd already uses; no new auth surface.

## Components to build

1. **`cmd/weft-forwarder/`** — new Go binary.
   - `GOOS=linux GOARCH=arm64`, `provided.al2023` runtime. ~5 MB zip, ~30 MB memory.
   - Handler accepts `events.CloudwatchLogsEvent`, decodes the gzip+base64 `AWSLogs.Data` payload, batches events, POSTs to the coordinator.
   - Retries with jitter on 5xx, surfaces 4xx as terminal (drops to DLQ per customer config).

2. **`POST /api/ingest/cloudwatch`** on the coordinator — new HTTP handler (~150 lines).
   - Bearer auth using existing namespace-scoped token (`api/handlers.go` pattern).
   - Accepts the decompressed envelope `{owner, logGroup, logStream, logEvents[]}`.
   - Maps `logGroup` → `app` via existing `log_groups` table (same schema as poll path).
   - Reuses `ingest.Parser` + shard routing logic from `coordinator/poller/poller.go:132-211`.

3. **CloudFormation template** — `deploy/weft-forwarder/template.yaml` (~60 lines).
   - Parameters: `WeftToken`, `WeftEndpoint` (default `ingest.w3ft.net`), `LogGroupNames` (list), `ForwarderVersion`.
   - Resources: `AWS::Lambda::Function` (references our S3-hosted zip), `AWS::IAM::Role`, one `AWS::Logs::SubscriptionFilter` per log group, optional `AWS::SQS::Queue` as DLQ.
   - "Launch Stack" URL with prefilled parameters from the weft UI.

4. **Release pipeline** — extend `.kevcons/ext/release.lua` with a `weft-forwarder` target.
   - Builds the zip, uploads to `s3://weft-releases-220664822695/weft-forwarder/v<ver>/weft-forwarder-arm64.zip`.
   - Updates a `latest/` pointer the CF template defaults to.

5. **Docs + UI**:
   - Split the existing "Connect a CloudWatch source" section in `frontend/src/pages/Docs.tsx` into two subsections: "Poll-based (no AWS infra)" and "Push-based (Lambda forwarder)".
   - Include a decision matrix calling out the customer-side cost delta (see below).
   - "Launch Stack" button on the source setup screen.

## Cost comparison (customer-side, at 10 GB/day ≈ 300 GB/mo)

| Line item | Poll | Lambda |
|---|---|---|
| `FilterLogEvents` data scanned ($0.005/GB) | ~$1.50 | $0 |
| Lambda invocations (~600k/mo @ $0.20/M) | $0 | $0.12 |
| Lambda GB-sec compute | $0 | $0.25 |
| **Data transfer OUT to internet ($0.09/GB)** | **$0** | **~$27.00** |
| Subscription filter slot (limit 2/log-group) | 0 | 1 |
| **Customer total / month** | **~$1.50** | **~$27** |

The $27/mo egress line dominates; it scales linearly to ~$250/mo at 3 TB, ~$2,500/mo at 30 TB. **Default recommendation should remain polling.** Lambda is for customers who have specifically named latency as the requirement and accepted the egress exposure.

Weft's own AWS bill is nearly identical either way — the transport cost shifts entirely between customer-paid scans (poll) and customer-paid egress (Lambda).

## Open design questions

- **Rate limiting.** A compromised token in customer-owned Lambda can ingest at whatever rate CloudWatch pushes. Need per-token ingest rate limits on `POST /api/ingest/cloudwatch` before GA, plus a fast `token revoke` path. (Useful for weftd auth hardening too, so not pure overhead.)
- **DLQ ownership.** If weft is down, CloudWatch retries Lambda invocations twice, then drops. Standard fix: SQS DLQ on the Lambda, owned by the customer. Keep optional in the CF template; default to "on" with a 14-day retention.
- **Schema bootstrap.** Poll path auto-detects schema from the first poll's sample. Lambda path has nothing running on the weft side until the first event arrives. Simplest: detect on first batch received, same as the weftd onboarding flow (`api/sources.go:36-54`), then queue subsequent batches until the customer confirms.
- **Metering visibility.** Customers need to see their egress bill forecasted in weft's UI *before* clicking Launch Stack. Surface "Expected AWS cost at your current log volume: $X/mo" on the Lambda path entry point. Requires sampling their log group volume first, which the poll path can do — so the UX flow is "poll first, optionally upgrade to Lambda."

## Estimated effort

- Lambda binary: 0.5 day.
- Coordinator HTTP endpoint + token auth reuse: 0.5 day.
- CloudFormation template + Launch Stack URL plumbing: 0.5 day.
- Release pipeline extension: 0.25 day.
- Docs + UI rework: 0.5 day.
- Rate-limit + DLQ + metering UX: 1 day.

**Total: ~3 days** to first working version suitable for a small pilot. Another 1-2 days before opening to self-serve signups.

## Non-goals

- Not replacing the poll path. Polling stays the default and documented-first option.
- Not supporting non-CloudWatch AWS sources via this Lambda (S3 events, Kinesis, etc.) in v1. Scope it tightly; those are separate sources with separate parsers.
- Not offering a Firehose-based variant. Firehose adds a third option with its own buffer/batch semantics and doesn't meaningfully beat the Lambda path on latency or cost.
