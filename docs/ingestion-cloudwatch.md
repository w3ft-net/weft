# Ingestion: CloudWatch

## Overview

CloudWatch is the default, zero-install ingestion path. Customers connect their AWS accounts by granting IAM roles, and weft ingests logs via CloudWatch subscription filters. This is the higher-latency option (seconds to low minutes depending on CloudWatch batching); sub-second tailing requires [weftd](ingestion-weftd.md).

## Data model

```
Customer (tenant — Kevlar, Acme Corp, ...)
  └── AWS Account (one IAM role per account)
       └── App (vimgym, callbox, ...)
            └── Log Group (/ecs/vimgym-prod, ...)
```

`$CUSTOMER` is never exposed as a user-facing meta-variable. The coordinator applies it automatically to every query based on the authenticated user's token. It is a row-level security boundary.

## Customer onboarding

Per AWS account the customer wants to connect:

1. Customer creates an IAM role in their account with:
   - Trust policy allowing our ingestion account to assume it (with external ID)
   - Permission: `logs:CreateSubscriptionFilter`, `logs:DeleteSubscriptionFilter`, `logs:DescribeLogGroups`, `logs:DescribeSubscriptionFilters`
2. Customer provides us with:
   - Role ARN
   - Log group → app name mapping (e.g., `/ecs/vimgym-prod` → `vimgym`)
3. We provide a CloudFormation template / Terraform module to automate steps 1-2

Multiple AWS accounts per customer are expected (landing zone pattern). Each account has its own role. A single customer may have accounts in different regions.

## Subscription filter setup

Once a customer grants access, weft assumes their role and creates a CloudWatch subscription filter on each configured log group. The subscription filter ships logs to our Kinesis stream (one stream per customer, or a shared stream with customer ID tagging — TBD).

Flow:
```
Customer's CloudWatch Log Group
    → Subscription Filter
    → Kinesis Data Stream (in our account)
    → Weft ingest consumer
    → Active shard (DuckDB INSERT)
```

### Why subscription filters over polling

- Lower friction for the customer — set up once, logs flow automatically
- Lower latency than polling (CloudWatch batches and pushes vs. us pulling on an interval)
- No rate limit management or pagination logic on our side
- CloudWatch handles retry/backpressure to Kinesis natively

## Ingest consumer

A consumer process reads from the Kinesis stream and writes to the active shard:

1. Read batch of records from Kinesis
2. For each record: decompress (CloudWatch subscription data is gzipped), parse the CloudWatch Logs envelope
3. Apply schema detection rules to extract structured fields
4. Write batch to the active shard's gRPC ingest endpoint
5. Checkpoint the Kinesis shard iterator

The ingest consumer runs on the coordinator (Phase 1) or as a separate process (later).

## Schema detection

One-time setup per app. Run during onboarding or manually via `weft schema detect --app vimgym`.

Samples ~500 lines from the app's log group and auto-detects:

1. **JSON coverage** — what % of lines are valid JSON? Mixed prefix+JSON?
2. **Key analysis** — consistent keys across lines → proposed as columns
3. **Prefix pattern** — positional structure before JSON payload (timestamp, level, service)
4. **Field separator** — whitespace, comma, tab

User confirms the detected schema. Stored as a flat YAML file on the coordinator's EBS volume, transmitted to shard nodes on creation.

```yaml
customer: kevlar
app: vimgym
format: prefix_json
prefix_fields: [ts, level, service]
prefix_separator: " "
json_field: payload
expand_keys:
  - user_id: TEXT
  - action: TEXT
  - duration_ms: INTEGER
  - success: BOOLEAN
```

Shard nodes use this schema to parse incoming log lines into the DuckDB table structure (see [shard-lifecycle.md](shard-lifecycle.md) for the table schema).

## Re-ingestion on shard failure

When a shard fails and a replacement launches (see [shard-lifecycle.md](shard-lifecycle.md)), the replacement re-ingests its time window from CloudWatch. This uses `FilterLogEvents` with the shard's start/end timestamps — polling is fine here since it's a one-time backfill, not live ingest.

## Open questions

- **Kinesis topology:** one stream per customer, or shared stream with customer ID tagging? Per-customer is simpler isolation but more streams to manage. Shared is cheaper but needs careful partitioning.
- **Cross-region:** if a customer has log groups in us-east-1 and eu-west-1, do we run ingest consumers in each region, or ship cross-region to a central Kinesis stream? Latency vs. cost tradeoff.
- **Subscription filter limits:** CloudWatch allows max 2 subscription filters per log group. If the customer already uses one (e.g., for their own Datadog/Splunk), we're competing for the second slot. Should we document this limitation, or explore alternatives (CloudWatch Logs Insights API as a fallback)?
- **Schema versioning:** when an app's log format changes (new fields, different prefix), how do we handle schema migration? Re-detect and update, or version schemas so old data is still queryable with the old schema?
