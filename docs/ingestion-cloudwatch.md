# Ingestion: CloudWatch

## Overview

CloudWatch logs are ingested by push, not by polling: the customer deploys a
small CloudFormation stack in their own AWS account that forwards a
CloudWatch Logs subscription filter through a Kinesis Data Firehose
delivery stream to an HTTPS endpoint we host. No agent, no Lambda binary of
ours to build or release, and no cross-account IAM role into weft's
account — everything that runs lives in the customer's account, fully
AWS-managed. Sub-second tailing for hosts you control still goes through
[weftd](ingestion-weftd.md); this path is for CloudWatch-only sources
(Lambda, ECS, anything already writing to CloudWatch Logs).

An earlier design polled `FilterLogEvents` on an interval from a
coordinator-assumed cross-account role. That path has been fully retired —
see "Design history" below — except for one narrow reuse: `FilterLogEvents`
still backs one-time shard-failure backfill (below), which is a bounded
historical pull rather than live ingestion.

## Architecture

```
Customer's CloudWatch Log Group (per app, any region)
   │  put-subscription-filter (destination = Firehose delivery stream ARN,
   │  role = IAM role in the customer's account granting CW Logs firehose:PutRecord)
   ▼
Kinesis Data Firehose delivery stream (customer's account — AWS-managed, no code)
   │  destination: generic HTTP endpoint → https://api.w3ft.net/api/ingest/cloudwatch
   │  auth: X-Amz-Firehose-Access-Key header = the customer's wft_... namespace token
   │  on repeated failure: S3 backup bucket (customer's account, 30-day expiry)
   ▼
api process: POST /api/ingest/cloudwatch → CloudWatchAdapter (api/adapter_cloudwatch.go)
   — decodes Firehose's request envelope, resolves each event's AWS log
     group name to a weft service, builds a raw LogRow per event
   ▼
Shared ingest pipeline (api/adapter.go's IngestHandler — the same path
Heroku log drains use):
   enrichRows() — cache-aside schema lookup fills Level/Fields/TraceId/
     PayloadJson per row once the source has a configured schema
   → gRPC IngestStream to the coordinator → shard.Ingest
```

Everything above the `api process` line is a CloudFormation stack the
customer deploys once per AWS account/region they want to connect — see
"Customer onboarding" below.

## Data model

A `log_groups` row (`store.LogGroup`) is the unit of configuration: one row
per (namespace, service), scoped to a customer via its namespace.
`source_type = kinesis` identifies a CloudWatch-push source (as opposed to
`weftd` or `direct`). The row starts `StatusPending` (no schema yet) and
becomes `StatusConfigured` once a human approves a schema — see "Onboarding
a source" below.

`$CUSTOMER` is never exposed as a user-facing meta-variable. The coordinator
applies it automatically to every query based on the authenticated user's
token — a row-level security boundary, not something the ingestion path
needs to think about.

## Customer onboarding

Per AWS account/region the customer wants to connect:

1. Register the source: `weft source add NS/NAME --log-group /aws/lambda/my-function` (or the dashboard's "+ Add Kinesis source"). This only records the AWS log-group→service mapping — `StatusPending`, no schema yet.
2. Deploy the CloudFormation template (`deploy/weft-cloudwatch-firehose/template.yaml`) against the account/region owning that log group, passing the namespace's agent token and the log group name(s). This provisions:
   - The Firehose delivery stream, pointed at `https://api.w3ft.net/api/ingest/cloudwatch`
   - The IAM role CloudWatch Logs assumes to call `firehose:PutRecord`/`PutRecordBatch`
   - The subscription filter linking the log group to the stream
   - A required S3 bucket for failed-delivery backup (30-day expiry)
3. Once real events start flowing, `CloudWatchAdapter` accumulates a sample from live traffic (there's nothing to poll on demand — the first sample comes from whatever the log group actually emits).
4. `weft source configure <namespace>/<service>` (or the dashboard's pending-source panel) reviews the accumulated sample and approves a schema, same wizard weftd onboarding uses. The source is now `StatusConfigured` and ingesting.

Multiple AWS accounts/regions per customer are expected — one CloudFormation stack per account/region, each with its own subscription filters.

## Wire format

Firehose's HTTP endpoint delivery POSTs `{requestId, timestamp, records:
[{data: "<base64>"}]}`, up to 10,000 records or 64 MiB per request. Each
record's decoded `data` is the standard CloudWatch Logs subscription
envelope — `CONTROL_MESSAGE` records (Firehose's periodic connectivity
check) are skipped; `DATA_MESSAGE` records carry `logEvents[]`, one row per
event. Records arrive gzip-compressed by default (matching a classic
subscription filter's Kinesis/Lambda destination) — `CloudWatchAdapter`
tries gunzip first, falling back to raw bytes if the magic header doesn't
match. The handler responds `200` with `{requestId, timestamp}` echoing the
request; any other status is treated as retriable and Firehose retries with
backoff, same `requestId` across attempts.

## Schema detection and enrichment

A source's schema (`ingest.Schema`, stored as `log_groups.schema_yaml`) is
approved once, during onboarding, via the same interactive detection wizard
weftd sources use — see [ingestion-weftd.md](ingestion-weftd.md) for the
detection heuristics (JSON coverage, key analysis, prefix pattern, field
separator).

Once configured, `CloudWatchAdapter` doesn't parse rows itself — a shared
`enrichRows` step (`api/adapter.go`) sits between "adapter built a raw
`LogRow`" and "row goes out over `IngestStream`," doing a cache-aside
lookup (Valkey, falling back to Postgres) of the service's configured
schema and filling `Level`/`Fields`/`TraceId`/`PayloadJson`. The same
enrichment step backs Heroku log drains and Render/syslog ingestion — one
parsing path for every push-based ingest source, not something specific to
CloudWatch.

## Re-ingestion on shard failure

When a shard fails and a replacement launches (see
[shard-lifecycle.md](shard-lifecycle.md)), the replacement re-ingests its
time window using `FilterLogEvents` against the source's configured AWS
credentials for that window — a one-time bounded historical pull, not live
ingestion, so polling is the right tool here even though it isn't used for
live tailing.

## Design history

An earlier design (`docs/plans/cloudwatch-lambda-forwarder.md`, retired)
proposed a customer-deployed Lambda function we'd build and version. Before
that shipped, a poll-based path (`coordinator/poller`, since removed)
assumed a cross-account IAM role and called `FilterLogEvents` on an
interval — the only CloudWatch ingestion actually running in production for
a while. Both were retired in favor of the Firehose push design above once
research confirmed it matches how other log SaaS vendors (New Relic, Sumo
Logic, Splunk) handle third-party CloudWatch ingestion: subscription filter
→ Firehose → generic HTTP endpoint, with zero custom compute on either
side. Full design rationale and vendor research: `docs/plans/cloudwatch-kinesis-subscription.md`.

## Open questions

- **Cross-region:** if a customer has log groups in multiple regions, they deploy one CloudFormation stack per region — there's no cross-region ingest path today.
- **Subscription filter limits:** CloudWatch allows max 2 subscription filters per log group. If the customer already uses one (e.g., for their own Datadog/Splunk), they're competing for the second slot.
- **Schema versioning:** when an app's log format changes (new fields, different prefix), how do we handle schema migration? Today: `weft source resample` pulls a fresh sample and re-runs the wizard; the schema swaps in place with no versioning of old data.
