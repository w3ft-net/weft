# Remaining questions from comments.md

## 3. Lambda-shard / Glacier lifecycle policy — RESOLVED
Bucket `weft-cold-{ACCOUNT_ID}` in `infrastructure-catalog/stacks/asg-weft-application/main.tf` (lines 635-661) has two lifecycle rules:
- `glacier-ir-parquet`: transitions to GLACIER_IR after 90 days (instant retrieval — needed for lambda-shard ms-latency queries)
- `expire-old-parquet`: hard deletes after `var.cold_retention_days`

## 4. Instance type claim — RESOLVED
`.kevcons.yaml` `build.instance_type: t4g.xlarge` is the Packer **build** instance (DuckDB CGO compilation needs RAM), not the runtime type. Actual prod shard instances from `infrastructure-live/prod/weft/terraform.tfvars`:
- **small tier**: t4g.micro (2 running) — the active tier
- **medium tier**: t4g.small (0 desired, available for scale-up)
- **large tier**: t4g.medium (0 desired, available for scale-up)

Architecture overview updated accordingly.

## 5. Usage metering — RESOLVED
**Bytes, not lines.** The drain loop calls `Usage.Record(customer, int64(len(row.Raw)))` for every ingested row (`coordinator.go:137`). The unit is byte length of the raw log line.

**It is running.** `StartUsageFlush` runs every 5 minutes (`coordinator.go:194`), draining completed hourly buckets to `usage_hourly` via upsert. `QueryUsage` combines `usage_hourly` + `usage_daily` tables. The `GetUsage` gRPC endpoint and the shell's `/etc/status` view both query this data, including the current unflushed hour from memory.

**Not wired to Stripe.** Usage data is tracked internally and shown in the dashboard, but there is no Stripe metered billing integration — no usage records are pushed to Stripe subscriptions. The `UsageRecord` model exists in `store/user.go` but is separate from the hourly/daily tables and has no Stripe reporting code.

## 6. QueryToCompute empty response
Answered in architecture-overview.md: it's an unimplemented optimization for direct shard→compute streaming. The working path is coordinator-relayed (CreateJob → SendRows → GetResult). The shard server stub at `shard/service/server.go:86-90` just returns empty.
