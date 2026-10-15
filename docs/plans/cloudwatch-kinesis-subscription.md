# CloudWatch push ingestion via Kinesis Data Firehose

**Supersedes the earlier `cloudwatch-lambda-forwarder.md` plan and this
doc's own first draft.** Both are gone; this is the only push-based
CloudWatch design going forward. See "Revision history" at the bottom
for why.

## Why

Today's only live CloudWatch ingestion path is `coordinator/poller/poller.go`
polling `FilterLogEvents` on an interval. `docs/ingestion-cloudwatch.md`
always specified subscription-filter-based push as the target
architecture — lower latency, no rate-limit/pagination handling on our
side, CloudWatch handles backpressure — but nothing push-based was ever
built. Concrete costs of staying on polling, confirmed while reading the
code for the poller-reconciler fix:

- **Latency floor** = `PollInterval` (customer-configured, default 30s).
- **Availability coupling** — if the coordinator is down, polling loses
  the interval entirely; a subscription filter into a durable AWS
  service keeps buffering independently of whether anything downstream
  is consuming it.
- **Wasted STS load** — `ingest.FetchCloudWatch` builds a fresh
  `stscreds.AssumeRoleProvider` + `aws.NewCredentialsCache` on every
  poll tick, per log group, and throws the cache away when the call
  returns.
- **No reconcile loop** — `Poller.Start()` reads `log_groups` once at
  boot; config or lease changes only take effect on restart
  (`coordinator-chaos-tests.md` Tests 2/7).

This plan does not touch `FetchCloudWatch`'s other job — one-time
shard-failure backfill (`docs/ingestion-cloudwatch.md`, "Re-ingestion on
shard failure") — where polling is explicitly fine because it's a
bounded historical pull, not live tailing.

## What we got wrong the first time, and the actual research

The first draft of this plan proposed a **cross-account Kinesis Data
Stream with a custom consumer we'd operate** (AWS's
["Cross-account cross-Region log data sharing using Kinesis Data Streams"](https://docs.aws.amazon.com/AmazonCloudWatch/latest/logs/CrossAccountSubscriptions-Kinesis.html)
pattern), asserted as "the standard, non-Datadog path" without actually
checking what log SaaS vendors do. That assertion was wrong. Checked
against real vendor docs:

- **Datadog** — subscription filter → customer-owned Lambda forwarder →
  HTTPS POST to Datadog's intake API. ([Splunk's own writeup of the general shape confirms Lambda-forwarder is the Datadog pattern](https://www.splunk.com/en_us/blog/tips-and-tricks/how-to-ingest-any-log-from-aws-cloudwatch-logs-via-firehose.html))
- **New Relic** — subscription filter → **Kinesis Data Firehose** →
  Firehose's built-in HTTP endpoint delivery, straight to New Relic's
  Logs API. ([Stream logs using Kinesis Data Firehose](https://docs.newrelic.com/docs/logs/forward-logs/stream-logs-using-kinesis-data-firehose/))
- **Sumo Logic** — same shape: subscription filter → Firehose → Firehose
  HTTP endpoint delivery to a Sumo-hosted URL. Their docs are explicit
  that this "eliminates the need for creating separate log processors or
  forwarders such as AWS Lambda functions." ([AWS Kinesis Firehose for Logs Source](https://www.sumologic.com/help/docs/send-data/hosted-collectors/amazon-aws/aws-kinesis-firehose-logs-source/))
- **Splunk** — same shape again: subscription filter → Firehose → HTTP
  Event Collector, with Firehose's native CloudWatch-decompression
  feature doing what used to require an inline Lambda transform. ([How to ingest any log from CloudWatch Logs via Firehose](https://www.splunk.com/en_us/blog/tips-and-tricks/how-to-ingest-any-log-from-aws-cloudwatch-logs-via-firehose.html))

3 of 4 vendors checked use **Firehose → generic HTTP endpoint delivery**,
not a raw cross-account Kinesis Data Stream with a self-hosted consumer.
That's a fully AWS-managed resource in the *customer's* account (no
compute of ours to build, ship, or version), delivering directly to an
HTTPS endpoint we host — a code-free analog of the Lambda-forwarder
idea, and the reason that plan is being retired rather than kept
alongside this one. AWS confirms "generic HTTP endpoint" is a first-class
Firehose destination type, not restricted to named partners:
[HttpEndpointDestinationConfiguration](https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/aws-properties-kinesisfirehose-deliverystream-httpendpointdestinationconfiguration.html).

Also confirmed directly against AWS's subscription-filter docs
([Log group-level subscription filters, Example 3](https://docs.aws.amazon.com/AmazonCloudWatch/latest/logs/SubscriptionFilters.html#FirehoseExample)):
a subscription filter can target a Firehose delivery stream **directly**
— no intermediate Kinesis Data Stream needed, and (unlike the Kinesis
Data Streams cross-account case) the log group and the Firehose stream
can live in the *same* account, which is exactly the vendor pattern:
the Firehose stream runs in the *customer's* account, not ours.

## Architecture

```
Customer's CloudWatch Log Group (per app, any region)
   │  put-subscription-filter (destination = Firehose delivery stream ARN,
   │  role = IAM role in the customer's account granting CW Logs firehose:PutRecord)
   ▼
Kinesis Data Firehose delivery stream (customer's account — AWS-managed, no code)
   │  destination: generic HTTP endpoint → https://api.w3ft.net/api/ingest/cloudwatch
   │  (api.w3ft.net, not ingest.w3ft.net — the latter is the coordinator's
   │  gRPC ALB for weftd/loom-producer; HTTP-pushed adapters (Heroku,
   │  CloudWatch) live on the api service, per http-ingest.md)
   │  auth: X-Amz-Firehose-Access-Key header = our existing wft_... namespace token
   │  on repeated failure: optional S3 backup bucket (customer's account)
   ▼
api process: POST /api/ingest/cloudwatch → CloudWatchAdapter (api/adapter_cloudwatch.go)
   — Auth: X-Amz-Firehose-Access-Key against the namespace token (same
     s.store.ValidateAgentToken every adapter uses — no new auth surface)
   — Parse: decode Firehose's request envelope (see "Wire format" below),
     resolve each record's AWS logGroup → our service (open question,
     below — the one adapter-specific step left), build LogRow{Service,
     Kind, Instance, Raw, TsUnixNs} per event — no schema parsing here
   ▼
Shared IngestHandler pipeline (api/adapter.go — same path Heroku already
uses, unmodified for this plan):
   enrichRows() — cache-aside schema lookup (Valkey, falling back to
     Postgres) fills Level/Fields/TraceId/PayloadJson per row, keyed on
     row.Service — see "Shared ingest-time enrichment" below
   → gRPC IngestStream to the coordinator → ring buffer → StartDrain →
     ShardProvider.IngestingShard → shard.Ingest
   — replies with Firehose's required {requestId, timestamp} JSON success
     body (IngestHandler's own response needs a small Firehose-specific
     override here — its current 204-No-Content default doesn't match
     Firehose's required envelope; see Components to build)
```

Everything above the `api process` line is a CloudFormation stack the
customer deploys once (we provide the template + a "Launch Stack"
button), same delivery mechanism as `http-ingest.md`'s other adapters.
There is no Lambda binary of ours to build, release, or ask customers to
upgrade — Firehose is entirely AWS-operated.

Earlier drafts of this diagram had the handler talking directly to
`ShardProvider.IngestingShard` the way `coordinator/poller/poller.go`
does, and doing its own `ingest.Parser` call inline — modeled on the
poller because that was the only code that did both routing and
parsing. That turned out to be unnecessary: the generic HTTP-adapter
pipeline (`api.Server.IngestHandler`, used by `HerokuAdapter` today)
already terminates at the exact same `ShardProvider.IngestingShard →
shard.Ingest` call, just asynchronously via the coordinator's ring
buffer + drain loop instead of a synchronous direct call — and, as of
this session, it now does schema-based parsing too (see below).
CloudWatchAdapter can be a completely ordinary `Adapter` implementation,
living in the `api` process like Heroku, with no special-cased routing
or parsing of its own.

## Wire format (verified against AWS's HTTP endpoint delivery spec)

Full spec: [Understand HTTP endpoint delivery request and response specifications](https://docs.aws.amazon.com/firehose/latest/dev/httpdeliveryrequestresponse.html).
Load-bearing details for the handler:

- **Auth**: `X-Amz-Firehose-Access-Key` carries whatever value we set
  when configuring the delivery stream — reuse the existing `wft_...`
  namespace-scoped token, same mechanism `api/handlers.go` already
  checks elsewhere. No new auth surface.
- **Request body**: `{requestId, timestamp, records: [{data: "<base64>"}]}`,
  up to 10,000 records or 64 MiB (pre-compression) per request. `data`
  is base64; **open question below** on whether the decoded bytes are
  still gzip-compressed CloudWatch envelope JSON (matching what
  `ingest.Parser` already expects from the poller path) or plain JSON —
  AWS's own native "decompress CloudWatch Logs" toggle is documented as
  available only for **S3 and Splunk** destinations, not generic HTTP
  endpoints, which points to "still gzipped, decode it ourselves" but
  needs confirming against a real payload during the pilot.
- **Response**: HTTP 200, `Content-Type: application/json`, no
  `Content-Encoding`, body `{requestId, timestamp}` echoing the request's
  `requestId`. Any other status is treated as retriable (Firehose retries
  with exponential backoff, same `requestId` across attempts — a natural
  dedup key if we need one).
- **Timeout**: Firehose gives the endpoint 3 minutes per request.

## Shared ingest-time enrichment (built ahead of this plan)

While scoping this plan's handler, we found and closed a gap that
affects every HTTP/syslog-pushed ingest path, not just CloudWatch — worth
documenting here since `CloudWatchAdapter` is the first *new* consumer of
this machinery rather than something it needs to build itself.

**The gap.** Two independent facts, neither obvious in isolation:

1. The coordinator's `IngestStream` gRPC handler never applies a
   service's schema to a row — it trusts `Level`/`Fields`/`TraceId`/
   `PayloadJson` to already be set, by design (`weftd-client-side-parsing.md`:
   "if the agent didn't parse it, it doesn't get parsed"). Only weftd
   (client-side, before the row is ever sent) and the CloudWatch poller
   (`poller.go:130,161`, before this plan existed) ever called
   `ingest.Parser`.
2. Every other ingest path — `HerokuAdapter` (`api/adapter_heroku.go`)
   and `SyslogServer`/Render (`api/syslog_server.go`) — built rows
   directly from their own wire format and never called a parser either.
   `HerokuAdapter`'s code even had a comment, "let the schema parser
   fill it later," that nothing ever fulfilled — Heroku-sourced rows
   have been `Level`/`Fields`/`TraceId`/`PayloadJson`-empty in prod
   since that adapter shipped.

So a customer with a configured schema got parsed rows from CloudWatch
poll and weftd, but permanently raw, level-less, untraceable rows from
Heroku and Render — an inconsistency across otherwise-equivalent ingest
paths that a new push-based CloudWatch path would have either inherited
(if built like Heroku) or fixed only for itself (if it parsed inline the
way the poller does), leaving Heroku/Render still broken either way.

**The fix — a shared enrichment core, applied uniformly.**
`api/adapter.go` (`resolveParser`, `applyParsed`, `enrichRow`,
`enrichRows`) is a small parsing layer that sits between "adapter/producer
built a raw `LogRow`" and "row goes out over `IngestStream`":

- `enrichRows` — called from `IngestHandler` (`api/adapter.go`) right
  after `Adapter.Parse`, for every batched HTTP ingest path (Heroku
  today; JSON and CloudWatch once built). Memoizes one `*ingest.Parser`
  per distinct `row.Service` within the request.
- `enrichRow` — called from `SyslogServer.handleConn` per frame (Render
  today; any future syslog-speaking platform). No per-request batch to
  memoize against (a connection can live for hours), so this leans
  entirely on the cache described next.
- Both overlay only `Level`/`Fields`/`TraceId`/`PayloadJson`/`TsUnixNs`
  onto the row the adapter/producer already built — `Service`/`Kind`/
  `Instance` (wire-format-derived, more specific than a schema's generic
  defaults) are never touched. A service with no configured schema
  passes through unchanged — not a new failure mode, the same behavior
  Heroku/Render had before this existed.

**The cache — shared Valkey, not per-process.** A naive per-row
`FindLogGroup` Postgres lookup is fine for Heroku's small batched POSTs
but not for a syslog connection forwarding a line at a time for hours,
and multiple `api`+`coordinator` nodes in prod means a per-process cache
would leave each node independently stale. `store/schema_cache.go`
(`SchemaCache`) is a cache-aside layer against the same shared Valkey/
ElastiCache instance `coordinator` already uses for tail `-f` fan-out
(`coordinator/tail_fanout_redis.go`), but a separate key space (`schema:
{orgID}:{namespace}:{service}`) and no pub/sub — a plain `DEL` on every
successful schema write (`store.AddLogGroup`, `store.ConfigureLogGroup`)
is sufficient since there's no longer-lived local tier to also evict.
Nil-safe throughout: a `*store.Store` with no cache attached (tests,
`weft-admin`, `weft-migrate`, or the `api` process before this session's
`--redis-addr` plumbing lands) transparently falls through to Postgres
on every call, identical to pre-cache behavior. `store.FindLogGroupSchema`
is the cache-aside read `enrichRow`/`enrichRows` call; TTLs (5min
positive, 30s negative) are a safety net for a missed invalidation, not
the correctness mechanism.

**Prod readiness caveat.** ElastiCache Valkey was "infra written, not
yet applied" as of `coordinator-horizontal-scaling.md`'s 2026-09-17
entry — confirm it's actually live before relying on it; degrades safely
either way (`--redis-addr` unset on `api` ⇒ cache stays nil ⇒ pure
Postgres fallback).

**What this means for `CloudWatchAdapter`:** once built as an ordinary
`Adapter` (see Architecture above), it gets `Level`/`Fields`/`TraceId`/
`PayloadJson` parsing for free from `enrichRows` — full parity with
today's poll-mode CloudWatch — without writing or calling `ingest.Parser`
itself. The only piece still specific to CloudWatch is resolving each
event's AWS `logGroup` name to a weft `service` (see Open questions).

## Components to build

1. **CloudFormation template** (`deploy/weft-cloudwatch-firehose/template.yaml`)
   — **landed**, not yet published to `weft-public` or live-deployed
   against a real account. Customer-deployed, one stack per AWS
   account/region combo they want to connect:
   - `SubscriptionFilterRole`: IAM role trusted by `logs.amazonaws.com`
     with the `ArnLike`/`aws:SourceArn` confused-deputy condition —
     Principal and condition operator copied verbatim from AWS's own
     documented example ("Subscription filters with Amazon Data
     Firehose") after an earlier draft got both wrong (region-qualified
     principal, `StringLike` instead of `ArnLike`) — granting
     `firehose:PutRecord`/`PutRecordBatch` on the stream below.
   - `DeliveryStream` (`AWS::KinesisFirehose::DeliveryStream`) with
     `HttpEndpointDestinationConfiguration` pointed at
     `https://api.w3ft.net/api/ingest/cloudwatch`, `AccessKey: !Ref WeftToken`,
     AWS's minimum buffering hints (1 MB / 60s, lowest latency
     allowed), `CloudWatchLoggingOptions` on (its own `FirehoseLogGroup`/
     `FirehoseLogStream`, separate from the customer's app log groups —
     lets them see in the console whether Firehose is reaching
     `api.w3ft.net`), and `S3Configuration` with `S3BackupMode:
     FailedDataOnly`. **Not actually optional**, despite the plan's
     original "optional S3 bucket" framing: `S3Configuration` is a
     *required* property of `HttpEndpointDestinationConfiguration` per
     AWS's CFN schema (confirmed via the live template reference, not
     assumed) — Firehose always needs somewhere to put records the
     endpoint rejects, for any non-S3-native destination type. The
     `FailedDeliveryBucket` this template creates has a 30-day
     lifecycle-expiry rule so it can't grow unbounded.
   - `SubscriptionFilters`: one `AWS::Logs::SubscriptionFilter` per
     entry in `LogGroupNames`, created by a small custom-resource
     Lambda (`SubscriptionFilterFunction`, inline Python, no external
     package) rather than CloudFormation's native `Fn::ForEach` — that
     transform's logical-ID substitution requires alphanumeric-only
     values, which real CloudWatch Logs group names (`/aws/lambda/...`)
     routinely violate, and getting that wrong would only surface as a
     failed customer deploy, not something worth risking untested.
     The Lambda runs only at stack create/update/delete (never touches
     a log line — the plan's "no compute of ours in the data path"
     claim is about the CloudWatchAdapter/data flow above, not
     deploy-time CFN plumbing) and ships inline with the template, so
     it carries none of the retired Lambda-forwarder design's
     ongoing-release burden.
   - Parameters: `WeftToken` (NoEcho), `LogGroupNames` (comma-delimited
     list) — matches the plan's original spec. The "Launch Stack" UI
     button with prefilled parameters (Components to build item 3,
     docs+UI) is still open.
   - Validated with `aws cloudformation validate-template` (structural
     check against live AWS CloudFormation — required properties,
     intrinsic functions, valid resource types; doesn't catch runtime
     logic bugs) — passes, requires `CAPABILITY_IAM` as expected. Not
     yet deployed end-to-end against a real customer-shaped account;
     that's the live pilot item below.
   - No in-repo CFN template to pattern-match against — the only
     existing customer-side-CFN precedent (`README.md:51-58`,
     `docs/ingestion-cloudwatch.md:28`) is a template hosted externally
     at `weft-public.s3.amazonaws.com/cfn/weft-log-reader.yaml`
     (currently 404 — likely aspirational/never actually published),
     not checked into this repo. This new template follows that same
     hosting convention (published to `weft-public`, not source-tree)
     once it's ready to publish, not an in-repo file to copy from.

2. **`api/adapter_cloudwatch.go`: `CloudWatchAdapter`** — **landed.** A
   genuinely ordinary `Adapter` (`api/adapter.go`) now that the shared
   enrichment core exists (see above):
   - `Auth`: reads `X-Amz-Firehose-Access-Key`.
   - `Parse`: base64-decodes + tries gunzip (falling back to the raw
     bytes when they aren't a valid gzip stream — see "Gzip-or-not on
     the wire" below, still unresolved, so this handles both branches
     rather than blocking on the pilot) each record, unmarshals the
     CloudWatch subscription envelope JSON, skips `CONTROL_MESSAGE`
     records (Firehose's periodic connectivity check), and for each
     `DATA_MESSAGE`'s `logEvents[]` builds a bare
     `LogRow{Service, Kind: "main", Raw: message, TsUnixNs}` — no
     `ingest.Parser` call, `enrichRows` (already wired into
     `IngestHandler`) does that afterward. Malformed records/events are
     skipped individually rather than failing the whole request, same
     policy `HerokuAdapter` uses for malformed frames.
   - `ContentLimit`: Firehose's own cap, 64 MiB pre-compression.
   - `logGroup → service` resolution: `Adapter.Parse` grew a third
     parameter, `ac AdapterContext{OrgID, Namespace, Store}`, built by
     `IngestHandler` from the already-validated token (`HerokuAdapter`
     ignores it). `store.FindLogGroupByAWSName` does the lookup, scoped
     to `(orgID, namespace, log_group_name)`; unresolved log groups
     have their events dropped (not routed to `service=""`), matching
     the "no configured schema" pass-through philosophy elsewhere in
     this doc. Plain Postgres, memoized per-request in `Parse` (a map
     keyed on AWS log group name) — no Valkey cache-aside built yet,
     per the deferral below.
   - Firehose's response envelope: `Adapter.Parse` also returns an
     opaque `ack any` (`HerokuAdapter` returns `nil`); an adapter that
     needs a non-default response implements `ResponseWriter { WriteSuccess(w http.ResponseWriter, ack any) }`,
     which `IngestHandler` type-asserts for in place of its default
     `204`. `CloudWatchAdapter.WriteSuccess` writes `200` +
     `{requestId, timestamp}`, echoing the `ack`'s `RequestID`. Applies
     even when a request produces zero rows (an all-`CONTROL_MESSAGE`
     batch still needs `200`, not the default-204 empty-rows shortcut).
   - Route: `POST /api/ingest/cloudwatch` (`api/handlers.go`), unguarded
     by a rate limit for now — `LimitIngest` (see "Rate limiting / DLQ
     ownership" below) is shared infra Heroku doesn't have either yet.
   - Tests: `api/adapter_cloudwatch_test.go` — auth, gzip vs. plain
     decode in one request, `CONTROL_MESSAGE` handling, unresolved-
     log-group drop, 401 on missing access key.
   - Not built yet: the Valkey-backed `logGroup → service` cache
     (deferred per the original note below — no load evidence yet) and
     the CloudFormation template (Components to build item 1).

3. **Docs + UI** — update `docs/ingestion-cloudwatch.md` from
   "intended, not built" to describing this as the live push path;
   "Launch Stack" button on the source setup screen; update
   `docs/plans/http-ingest.md`'s `CloudWatchAdapter` section to point
   here instead of the retired forwarder doc.

## Non-goals

- Not touching `FetchCloudWatch`'s shard-failure backfill use — polling
  stays correct there; it's a one-time bounded historical pull, not live
  ingestion, so it was never in scope for this cutover.
- Not building a raw Kinesis Data Stream + custom consumer. Firehose's
  managed HTTP delivery makes that unnecessary complexity for this use
  case (no lease-per-shard, no checkpoint table, no consumer process to
  operate).

**Superseded non-goal:** an earlier draft of this section said poll mode
would be retired gradually, log-group by log-group, rather than in one
cutover, and that the poller's missing-reconcile-loop gap would stay
tracked separately rather than being fixed. Both are moot — see the
2026-09-23 "poll-mode fully ripped out" revision-history entry below:
`coordinator/poller` was confirmed to have zero live users and deleted
outright, so there was no reconcile-loop gap left to fix and no gradual
migration to sequence.

## Open questions

- **Gzip-or-not on the wire — resolved with high confidence, pending
  live confirmation.** Records arrive gzip-compressed by default, same
  as a subscription filter's classic Kinesis/Lambda destination — not
  auto-decompressed by Firehose for HTTP-endpoint destinations (that
  toggle is S3/Splunk-only, per AWS's `writing-with-cloudwatch-logs-
  decompression.html`, matching the reasoning that first raised this
  question). Evidence: OpenTelemetry Collector's `awsfirehosereceiver`
  (`open-telemetry/opentelemetry-collector-contrib`) — a production
  receiver built for exactly this pattern (CloudWatch Logs →
  subscription filter → Firehose → generic HTTP endpoint) — decodes
  base64 then checks the gzip magic header (`0x1f 0x8b`), decompressing
  only if present; recent PRs there (#50841, #38445) fix a *double*-
  decompression bug specifically on this path, confirming it's
  real-world-exercised, not theoretical. Dash0's CloudWatch-via-Firehose
  docs state the same plainly, no destination-type carve-out.
  `decodeRecord` (`api/adapter_cloudwatch.go`) already matches this
  exactly — base64 → try gunzip (magic-header failure falls back to raw
  bytes) — landed ahead of this research, not written to match it. Note
  a *separate* knob exists and must not be conflated: Firehose's
  `HttpEndpointDestinationConfiguration.RequestConfiguration.ContentEncoding: GZIP`
  gzips the *entire outer HTTP POST body* (sets `Content-Encoding: gzip`
  on the request itself) — orthogonal to the inner per-record `data`
  field's own gzip state; the CloudFormation template (Components to
  build item 1) should leave it unset unless a specific reason to
  enable it comes up. Still worth a live payload during the pilot as a
  confirmation, not a discovery step — the code shouldn't need to
  change either way.
- **Cost model — resolved.** A classic CloudWatch Logs subscription
  filter pointed at a Firehose delivery stream ARN is billed under
  Firehose's **Direct PUT** ingestion tier — CloudWatch Logs itself
  calls `PutRecordBatch` via the granted IAM role, which is the
  textbook Direct PUT case — not the "Vended Logs" tier (that tier is
  for AWS's newer native log types like VPC Flow Logs / Route 53
  resolver logs delivered through a separate CloudWatch→Firehose
  integration, priced far higher). Direct PUT rate:
  **$0.029/GB ingested** for the first 500 TB/month (tiered discounts
  beyond that), no 5KB rounding. No separate charge found for
  HTTP-endpoint delivery itself (vs. S3) on the current pricing page.
  Source: https://aws.amazon.com/firehose/pricing/.
- **Cutover mechanism — decided and landed.** Added a 4th
  `log_group_source_type` enum value, `kinesis`, alongside the existing
  `cloudwatch`, `weftd`, `direct` (`store/schema.sql`'s enum, Go constant
  `store.SourceTypeKinesis` in `store/source.go`). Chosen over reusing
  `cloudwatch` + a bool flag: one column stays the single source of
  truth for "how is this log group ingested," matching the existing
  `LogGroupSourceType` switch sites — a second flag column would make
  every such switch silently wrong until updated. Migration
  `store/migrations/076_log_group_source_type_kinesis.sql`
  (`ALTER TYPE log_group_source_type ADD VALUE IF NOT EXISTS 'kinesis'`)
  is in; poll-mode rows are unaffected until a row is explicitly
  migrated to `kinesis`.
- **Ingest-time schema enrichment (Level/Fields/TraceId/PayloadJson) —
  resolved, and generalized beyond this plan.** See "Shared ingest-time
  enrichment" above — `CloudWatchAdapter` gets this for free from
  `enrichRows`, the same path `HerokuAdapter` now uses (previously
  didn't). Also closed the equivalent gap for Render/syslog
  (`SyslogServer` now calls `enrichRow` per frame). Backed by a shared
  Valkey cache-aside (`store/schema_cache.go`) rather than a per-process
  cache, since prod runs multiple `api`+`coordinator` nodes.
- **Rate limiting / DLQ ownership** — mechanism confirmed, no open
  design question: `api.RouteLimit` (`api/ratelimit.go:19-29`) is a
  small const iota + `budgetFor` switch backed by
  `golang.org/x/time/rate`, keyed on `(route, ip)`
  (`api/ratelimit.go:71-74`); adding `LimitIngest` is the same shape as
  the 4 existing rules (`LimitLogin`, `LimitDevicePoll`, `LimitAnonAuth`,
  `LimitStripeWebhook`) plus new RPM/burst fields on `config.Runtime`.
  `http-ingest.md` already names this `LimitIngest` — just not
  implemented yet. DLQ is simpler now: Firehose's own S3
  failed-delivery backup, no customer-owned SQS queue to spec.
- **Schema bootstrap** — mechanism confirmed, no open design question:
  mirror `handleAgentSample` (`api/sources.go`, `POST /api/agent/sample`)
  — validate the request, resolve the namespace, then run
  `ingest.Detect(req.Lines)` on the first batch to auto-detect a schema,
  same as the weftd onboarding flow. A customer finishing that flow sees
  `enrichRows` pick up the new schema on their very next ingested line,
  not after a cache TTL — `ConfigureLogGroup` actively invalidates the
  Valkey cache entry on write (see "Shared ingest-time enrichment"), the
  30s negative-cache TTL only bounds a *missed* invalidation.
- **Adapter-level `logGroup` → `service` resolution — resolved and
  landed.** `Adapter.Parse` gained `ac AdapterContext{OrgID, Namespace,
  Store}`; `store.FindLogGroupByAWSName` does the lookup. See
  Components to build item 2.
- **Firehose's success-response envelope vs. `IngestHandler`'s fixed
  204 — resolved and landed.** `Adapter.Parse` now also returns an
  opaque `ack any`; the optional `ResponseWriter` interface
  (`WriteSuccess(w http.ResponseWriter, ack any)`) lets
  `CloudWatchAdapter` override the default 204 with Firehose's required
  `200` + `{requestId, timestamp}`. See Components to build item 2.

## Estimated effort

Prerequisite work (cutover enum + migration, the shared Valkey schema
cache, `enrichRow`/`enrichRows`, Render/syslog parity) is done — landed
ahead of `CloudWatchAdapter` itself since the gap it closed affected
Heroku and Render regardless of whether this plan ever shipped.
`CloudWatchAdapter` itself (envelope decode, `logGroup → service`
resolution, Firehose response-envelope override, tests) is also done —
see Components to build item 2. The CloudFormation template (IAM roles,
Firehose stream, per-log-group subscription filters via a custom
resource, required S3 backup) is also done and passes
`validate-template` — see Components to build item 1. Remaining:

- Live pilot against one real log group (end-to-end validation — the
  gzip question is resolved and the template validates, but neither
  has been exercised against a real deployed stack yet): ~0.5d
- Rate-limit plumbing (`LimitIngest`, same shape as the 4 existing
  `RouteLimit` rules): ~0.5d
- Docs + UI ("Launch Stack" button, updated ingestion doc, publish the
  template to `weft-public`): ~0.5d

**Total: ~1.5d** remaining to a working pilot on one log group — down
from the original ~4d now that the shared enrichment/cache/routing
work, `CloudWatchAdapter`, and the CloudFormation template are all
done, and less than the retired plan's ~3d-Lambda-plus-6.25d-Kinesis-
Streams estimates combined, since there's no custom compute of ours to
build, release, or version in the actual log-data path either side —
the CFN template's own deploy-time-only Lambda doesn't carry that
burden (see Components to build item 1).

## Revision history

- **Draft 1** (retired): cross-account Kinesis Data Streams + a
  coordinator-hosted custom consumer, modeled on AWS's org-level
  cross-account log-sharing docs. Real AWS pattern, but not what any
  checked log SaaS vendor actually runs for third-party ingestion —
  retired once vendor research came back.
- **`cloudwatch-lambda-forwarder.md`** (retired, deleted): customer
  deploys a Lambda binary we build/release/version. Superseded by this
  plan once Firehose's generic-HTTP-endpoint delivery was confirmed to
  cover the same push use case with zero custom compute on either side.
- **This draft**: subscription filter → customer-owned Firehose stream
  → our HTTP endpoint. Current plan of record.
- **2026-09-23**: while scoping the handler's parsing/routing, found and
  fixed a pre-existing gap affecting every HTTP/syslog-pushed ingest
  path (Heroku's rows have been `Level`/`Fields`/`TraceId`/`PayloadJson`-
  empty in prod since that adapter shipped; Render/syslog had the same
  gap). Landed ahead of `CloudWatchAdapter` itself as shared
  infrastructure: `store.SourceTypeKinesis` + migration 076; `store/
  schema_cache.go` (shared Valkey cache-aside for `log_groups.schema_yaml`,
  since prod runs multiple `api`+`coordinator` nodes); `store.ConfigureLogGroup`
  (consolidating an inline `gorm.Updates()` that used to live directly
  in `api/sources.go`, finishing a stale comment's already-intended
  design); `api/adapter.go`'s `enrichRow`/`enrichRows`, wired into both
  `IngestHandler` (Heroku, and `CloudWatchAdapter` once built) and
  `SyslogServer` (Render). See "Shared ingest-time enrichment" above.
- **2026-09-23 (later same day): `CloudWatchAdapter` landed.**
  `api/adapter_cloudwatch.go` — envelope decode (base64 + gunzip-or-
  plain-fallback, per the still-open gzip question), `CONTROL_MESSAGE`
  skip, `logGroup → service` resolution, Firehose's `{requestId,
  timestamp}` response envelope. Required extending `Adapter.Parse` to
  `(body io.Reader, headers http.Header, ac AdapterContext) (rows,
  ack any, err error)` and adding the optional `ResponseWriter`
  interface to `IngestHandler` — both resolved exactly as sketched in
  Components to build item 2 below, `HerokuAdapter` updated to match
  (ignores `ac`, returns a nil `ack`). New `store.FindLogGroupByAWSName`
  (plain Postgres, no cache — see the deferral note). Route: `POST
  /api/ingest/cloudwatch`. Tests: `api/adapter_cloudwatch_test.go`.
  Not built: CloudFormation template, `LimitIngest`, UI/docs polish —
  see Estimated effort for what's left.
- **2026-09-23 (later still): gzip question resolved, CloudFormation
  template landed.** Research turned up production code confirming
  Firehose HTTP-endpoint records arrive gzip-compressed by default
  (OpenTelemetry Collector's `awsfirehosereceiver`, plus corroborating
  vendor docs) — `decodeRecord`'s base64→try-gunzip→fallback already
  matched this, no code change needed. See "Gzip-or-not on the wire"
  above. `deploy/weft-cloudwatch-firehose/template.yaml` landed:
  Firehose delivery stream, both IAM roles (Firehose's own delivery
  role and CloudWatch Logs' subscription-filter role — the latter's
  trust policy corrected against AWS's own documented example after an
  earlier draft got the principal and condition operator wrong),
  required S3 failed-delivery backup with a 30-day expiry, and a small
  deploy-time-only custom-resource Lambda creating one subscription
  filter per `LogGroupNames` entry (native `Fn::ForEach` was ruled out
  — its logical-ID substitution needs alphanumeric-only values, which
  real log group names violate). Passes `aws cloudformation
  validate-template`; not yet deployed against a real account. See
  Components to build item 1.
- **2026-09-23 (later still): initial target is internal dogfooding,
  not an external customer.** The first real consumers of this push
  path are weft's own operational Lambdas — `weft-cold-shard-query`
  and `weft-compactor` (`infrastructure-catalog/stacks/asg-weft-
  application/main.tf`) — landing in the dogfood org's `ops` namespace
  (see `coordinator-scaling-live-testing.md`). Since these run in an
  AWS account this repo's infra already manages end to end, ingestion
  is native Terraform, not the customer-facing CFN template above:
  `infrastructure-catalog/stacks/asg-weft-application/cloudwatch-
  firehose.tf` (Firehose stream, both IAM roles, required S3 backup,
  two `aws_cloudwatch_log_subscription_filter` resources — no
  Fn::ForEach-style workaround needed, Terraform just declares two
  named resources directly for two known targets). `terraform plan`
  against the real prod root (`infrastructure-live/prod/weft`)
  confirms 8 of 10 new resources plan cleanly against live AWS
  (correct account/region interpolation verified in the rendered IAM
  policy); the remaining two (the Firehose stream itself and its
  dependent IAM policy) block on one documented manual prerequisite —
  minting an `ops`-scoped weft agent token and `kevcons secrets set
  CLOUDWATCH_FIREHOSE_TOKEN <token>` — not yet done, not something
  Terraform or this session does on its own (a live mutation against
  weft's own prod DB). Not applied — awaiting that prerequisite plus
  explicit go-ahead per this repo's Terraform-apply safety rule. A
  second manual step after apply: configure the two `log_groups` rows
  mapping the Lambda log groups to `ops`-namespace services (see the
  new file's header comment for both steps). The customer-facing CFN
  template and its "publish to weft-public" follow-up are unaffected
  and still the eventual external-customer path.
- **2026-09-23 (later still): kinesis-mode source creation built —
  poll-mode is being retired, in two passes.** Decided: rip out the
  poll-based CloudWatch path entirely, including every doc reference to
  it ever having existed — but not before something else can create a
  CloudWatch source, since `AddSource` currently hard-codes
  `SourceTypeCloudWatch` and is the *only* way any CloudWatch
  `log_groups` row gets created (proto, gRPC, HTTP admin API, `weft
  source add` CLI, and a React frontend component all exist and are
  live for poll mode; none of it exists yet for kinesis). This pass
  built that missing piece:
  - `proto/weftpb/weft.proto`: `AddSourceRequest` gained `source_type`
    (field 11) — `""`/`"cloudwatch"` keeps today's poll-mode behavior
    unchanged; `"kinesis"` is new. Regenerated with `protoc`.
  - `store.CreatePendingKinesisSource` (`store/source.go`): registers
    the AWS-log-group→service mapping `CloudWatchAdapter.
    FindLogGroupByAWSName` needs, `StatusPending`, no schema, no
    cross-account role. Idempotent on (namespace, service). Deliberately
    a new method, not a reuse of `AddLogGroup` (which hardcodes
    `StatusConfigured`, wrong for this) or `UpsertPendingLogGroup`
    (whose create-branch hardcodes weftd-specific fields — never hit
    for kinesis in practice, since the row always pre-exists before any
    ingest, but still the wrong method to extend).
  - `coordinator/service/server.go`: `AddSource` branches on
    `req.SourceType` — kinesis calls `CreatePendingKinesisSource`
    directly (no `AWSAccount` row, no role assumption); every other
    value keeps the existing poll-mode path byte-for-byte.
  - `api/adapter.go`: new `accumulatePendingSamples`, called
    from `IngestHandler` right after `enrichRows`. This is the
    "Schema bootstrap" piece the plan called for above — since there's
    no agent to decide when a sample is "done" (unlike weftd's
    `SubmitSample`), it accumulates raw lines from real Firehose-pushed
    traffic into the pending row's `Sample`/`Detection` on every batch,
    up to `SampleTarget`, reusing `UpsertPendingLogGroup` for the write
    (its update branches are already source-type-agnostic — only the
    create branch is weftd-specific, and is never reached here). Scoped
    tightly to `SourceTypeKinesis` + `StatusPending`/`StatusSampling` so
    it can't touch weftd's own pending rows (which arrive via
    `IngestStream`, never through `IngestHandler`/`enrichRows`) or
    resample a configured source. A real bug caught by its own test on
    the first run: the gate only checked `StatusPending`, so accumulation
    silently stopped after the first batch once `UpsertPendingLogGroup`
    flipped the row to `StatusSampling` — fixed to accept both.
  - `api/sources.go` + `api/handlers.go`: new `POST /api/sources/kinesis`
    → `handleAddKinesisSource` (namespace/service/log_group/region only
    — no role, no schema fields, unlike `handleAddCloudWatchSource`).
  - `shell/http_remote.go`: new `AddKinesisSourceRequest`/
    `AddKinesisSource`, mirroring `AddCloudWatchSource`'s shape.
  - `cmd/weft/main.go`: `weft source add --type kinesis` — skips
    `--role-arn`/`--interval` entirely, calls the new HTTP endpoint,
    tells the user to run `weft source configure` once a sample has
    accumulated. `--type` defaults to `cloudwatch` (unchanged behavior)
    during this transitional pass.
  - Finishing a kinesis source reuses `weft source configure` /
    `ConfigureLogGroup` completely unmodified — confirmed by test, not
    assumed: it was already source-type-agnostic before this pass.
  - Tests: `store/source_test.go` (pending→configure contract),
    `coordinator/service/add_source_kinesis_test.go` (AddSource's new
    branch, including its own validation), `api/
    adapter_cloudwatch_sample_test.go` (accumulation across two
    batches, and that a configured source is never touched). Full repo
    `go test ./...` green.
  - **Explicitly not done in this pass, by design** (per the user's
    chosen scoping — build kinesis-mode onboarding first, retire poll
    only once it can't leave a gap): the React frontend
    (`AddCloudWatchSource.tsx` has no kinesis equivalent yet — untested
    without a live browser session, deliberately deferred rather than
    shipped unverified), the actual poll-mode ripout (`coordinator/
    poller/`, `SampleCloudWatchSource`, `AWSAccount`, the poll-mode
    halves of the proto/API/CLI/frontend surface), and the full
    documentation rewrite ("if a user never saw it, it never happened"
    — README, `docs/manual/cloudwatch.md` (customer-facing, generates
    into the frontend via `cmd/docgen/`), `docs/ingestion-cloudwatch.md`,
    `docs/architecture-overview.md`, `docs/architecture.md`, and several
    `docs/plans/*.md` cross-references). All three are the next pass.
  - Found and deliberately left alone: an existing worktree
    (`worktree-cloudwatch-pending-configure`) with unmerged work
    unifying CloudWatch onboarding onto weftd's pending→configure spine
    plus a poller reconcile loop — confirmed with the user to be
    abandoned/superseded; not pulled into this work.
- **2026-09-23 (later still): React frontend for kinesis-source creation
  landed.** The first of the three items the previous pass deferred.
  `AddKinesisSource.tsx` (new) is `AddCloudWatchSource.tsx`'s
  counterpart, deliberately smaller: no role/account/external-ID/poll-
  interval fields (nothing of ours to assume a role into — Firehose
  pushes to us) and no "pull sample" step (there's nothing to poll on
  demand; `accumulatePendingSamples` fills the sample
  asynchronously from the customer's real Firehose deliveries once their
  CFN stack is live). It only collects service/log_group/region and
  calls the already-existing `POST /api/sources/kinesis`
  (`handleAddKinesisSource`) — no new backend work needed, that handler
  and route landed in the prior pass. The resulting `StatusPending` row
  surfaces through the existing generic `getPendingSources` /
  `<PendingSourceConfigure>` flow unmodified, confirmed by reading
  that component: it already branches only on `PendingSource.status`
  ("sampling" vs "pending"), nothing CloudWatch-poll-specific. Wired into
  `Namespaces.tsx` as a second "+ Add Kinesis source" button next to the
  existing CloudWatch one. `frontend/src/lib/api.ts`:
  `SourceInfo.source_type` widened to include `'kinesis'`;
  new `AddKinesisSourceRequest`/`addKinesisSource`.
  `ConfiguredSourcesList.tsx` gained a kinesis-specific meta line
  (log group · region · "pushed via Firehose", no poll interval — it has
  none) and, in passing, a real bug fix: the source-type badge's
  className was a plain string literal (`` `csl-badge-{s.source_type}` ``
  never interpolated, so every source — cloudwatch and weftd alike —
  rendered the same unstyled badge class); now a template literal.
  `npm run build` (tsc -b + vite build) and `npx vitest run` both green;
  no component-level test harness exists for this UI area yet (same gap
  `AddCloudWatchSource.tsx` already had — deliberately not introduced
  here as a side quest). Full repo `make test` still green throughout
  (Go-side untouched this pass). Remaining from the prior pass's
  three-item list: the poll-mode ripout and the full documentation
  rewrite.
- **2026-09-23 (final pass): poll-mode fully ripped out; documentation
  rewritten.** Confirmed with the user first — no enabled
  `source_type=cloudwatch` rows exist in prod, so this was a straight
  deletion rather than a staged migration. Backend: `coordinator/poller`
  deleted outright (its `ShardProvider` interface, lease-per-log-group
  polling loop, and `FetchCloudWatch` call — the backfill caller in
  `coordinator/service/server.go` is untouched, per Non-goals);
  `cmd/coordinator/main.go`'s wiring and stale comments removed.
  `store.AWSAccount` deleted along with `Organization.AWSAccounts`,
  `LogGroup.AWSAccountID`/`AWSAccount`, and the now-single-flow
  `AddLogGroup`'s `awsAccountID` parameter (was always `nil` outside the
  poll-mode branch — updated ~13 call sites, all test/seed code). Also
  dropped `LogGroup.LastPollAt`/`LastToken` (the poller's own
  `FilterLogEvents` checkpoint, unread by anything else) and
  `PollInterval` (the poller's ticker cadence, meaningless once nothing
  polls) — migration 077 drops all three columns plus the `aws_accounts`
  table, with a full down-migration; `log_group_source_type`'s
  `'cloudwatch'` enum value is left in place (Postgres can't drop an enum
  value without recreating the type) as an inert legacy value nothing
  writes anymore. `LogGroupWithAWSAccount` renamed `LogGroupJoined` with
  the `aws_accounts` LEFT JOIN and its three columns dropped. Proto:
  `SampleCloudWatchSource` RPC and both its messages deleted;
  `AddSourceRequest` reserved its six now-unused fields
  (`schema_yaml`/`poll_interval`/`aws_account_id`/`role_arn`/
  `external_id`/`source_type`) and kept only what the kinesis flow needs
  — `AddSource` (`coordinator/service/server.go`) now unconditionally
  registers a pending kinesis source, no more branch; `SourceInfo` lost
  `last_poll_at` the same way. Regenerated with `protoc`. API: deleted
  `handleSampleCloudWatchSource`/`handleAddCloudWatchSource` and their
  routes (`POST /api/sources/cloudwatch{,/sample}`); `handleListSources`
  dropped the now-gone fields from its response. CLI: `weft source add`
  is kinesis-only now — dropped `--type`/`--role-arn`/`--account`/
  `--external-id`/`--interval` entirely, no more inline sampling wizard
  at add time (there's nothing to sample from yet). Frontend:
  `AddCloudWatchSource.tsx` deleted; `Namespaces.tsx` now offers only
  "+ Add Kinesis source"; `SourceInfo.source_type`/`ConfiguredSourcesList`
  narrowed to `weftd`/`kinesis`/`direct`. `integration/buckets_test.go`
  and `coordinator/service/add_source_kinesis_test.go` updated to the new
  `AddSourceRequest` shape. `go build ./...`, `go vet -tags test ./...`,
  and `make test` (full suite, exercising migration 077 against a real
  test DB) all green; frontend `npm run build` and `npx vitest run` green.
  Docs: `docs/ingestion-cloudwatch.md` and `docs/manual/cloudwatch.md`
  (customer-facing, generated into the frontend via `cmd/docgen`) rewritten
  end to end to describe the Firehose push flow as the only path, no
  "intended, not built" framing left. `README.md`'s Quick Start rewritten
  off the role/external-ID flow onto CFN-deploy + `weft source configure`;
  also dropped its stray customer-facing mention of `warp` by name, per
  standing house rule that warp is internal-only. `docs/architecture.md`,
  `docs/architecture-overview.md` (write-path diagram and schema-source
  paragraph), and `docs/cli.md` (`weft source add`'s docs — previously a
  one-line stub — filled in properly, folded in per the user's request to
  land CLI docs together with this pass rather than as separate work)
  updated to match. Plan cross-references fixed forward-facing
  (`http-ingest.md`, `compliance/ingest-governance.md`,
  `request-tracing.md`) or annotated moot/historical in place rather than
  rewritten (`coordinator-chaos-tests.md`, `coordinator-horizontal-scaling.md`,
  `coordinator-scaling-live-testing.md`, `coverage-improvement.md`,
  `onboarding/tasks.md`) — those documents' value is mostly as dated
  investigation records, not living specs, so their historical narrative
  was left intact with a note rather than rewritten to read as if the
  poller had never existed. `docs/plans/README.md`'s index rows for this
  plan and `http-ingest` updated to reflect both being nearly done. This
  plan is now feature-complete end to end; remaining work is the live
  pilot against a real deployed stack and `LimitIngest` rate-limit
  plumbing — see Estimated effort above (itself now understating how much
  is done, since it predates this pass and the frontend/CLI pass before
  it).
