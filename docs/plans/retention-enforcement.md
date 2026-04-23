# Retention enforcement (future work)

The pricing table on the landing page advertises per-plan log retention
(`swatch` 30d, `yard` 90d, `bolt` 1y). The metrics rollup migration
(`026_cron_results_rollups.sql`) introduces an equivalent per-plan cap
for log-derived metrics via `store/user.go` `LogRetention` /
`MetricRetention`. Both are honored at **query time only**: the API
filters rows older than the caller's plan cap before returning them.
On disk, data sits longer than advertised — up to the global pyramid
boundary in Postgres, and indefinitely in S3 parquet until a bucket's
shard retention ages it out.

This is an **illusion** of enforcement. Rows and objects exist past the
advertised retention; they're just not returned. This doc captures
what real enforcement needs so the illusion can be retired.

## Why this matters

- **Compliance / data deletion requests.** If a customer asks us to
  prove their data is gone after 30 days, we can't. It's filtered out
  of responses but not deleted.
- **Cost.** We pay for Postgres disk and S3 storage on data the plan
  says we don't keep. At current volumes negligible; at 100× volume,
  not.
- **Tier differentiation.** Bolt (1y) customers see a real product
  difference vs. swatch (30d) only because the query layer enforces
  it. If a bug lets rows leak through the filter, every tier sees the
  same history.

## Current state

### Postgres (`cron_results` and rollup tables)

- `cron_results` is partitioned daily by `ran_at`, pg_partman drops
  partitions older than 7d.
- `cron_results_hourly` / `_4h` / `_daily` are partitioned by
  `bucket_start` with retentions matching the pyramid (14d / 35d /
  400d).
- Retention is **global**, not per-customer. A swatch customer's
  daily-tier rows persist up to 400d in a shared partition with a
  bolt customer's rows. The query router (`CronAggregate`,
  `LatestCronResult`, `QueryCronResults`, the `weft.*` builtins)
  ANDs `bucket_start >= now() - LogRetention(tier)` into every
  filter to hide them.

### S3 parquet (log shards)

- Each app bucket has a `RetentionPeriod` (default 168h) controlling
  when a shard gets archived to S3 — but archived doesn't mean
  deleted. Parquet objects live in S3 indefinitely.
- `bolt`'s "Cold storage (S3)" feature is actually the current
  default for every tier; there's no differentiation.
- No S3 lifecycle policy is set. We pay S3 Standard storage on all
  parquet objects forever.

## What real enforcement needs

### Postgres: per-customer sweep

A background worker (or pg_cron job) that, per row, deletes anything
older than that row's owning customer's `LogRetention`. Two ways:

1. **Sweep procedure.** Every N minutes, for each plan tier, DELETE
   FROM the rollup tables where `cron_watch_id` belongs to a customer
   on that tier AND `bucket_start < now() - retention(tier)`. Needs a
   join to `cron_watches → customers → subscriptions`. Cheap if the
   join is well-indexed; expensive if it isn't.
2. **`retain_until` column.** Add to every row at write time based on
   the owning customer's current tier; drop by it. Works well for
   writes, breaks if a customer downgrades (their old rows have a
   `retain_until` reflecting the old, more generous cap). Would need
   a retroactive update on tier change.

Option 1 is simpler and correct across tier changes; option 2 is
cheaper at query time but has the downgrade wart. I lean 1 unless
the sweep cost gets painful at scale.

Open question: the global pg_partman retention stays at the pyramid
max (400d for daily). That means bolt-tier data keeps the partition
alive for everyone; when the partition finally drops, any residual
rows from lower-tier customers go with it. The sweep just ensures
those residual rows are never more than N minutes old. Acceptable
grace, given the query layer already hides them.

### S3 parquet: lifecycle + delete

Three pieces:

1. **Per-tier bucket tagging.** When a shard archives to S3, tag the
   object with the owning customer's tier (or embed it in the key
   prefix, e.g. `weft-archive/tier=bolt/customer=.../...`). Needs a
   schema decision: are objects keyed by customer today? Look at
   `coordinator/archiver.go`.
2. **S3 Lifecycle rules.** One rule per tier — 30d expiration for
   `tier=swatch` prefix, 90d for `yard`, 365d for `bolt`. AWS handles
   the delete; we just tag correctly at write time.
3. **Query-side skip.** The shard planner must not hand a shard to
   the compute pool if its window is entirely outside the caller's
   retention. This is already implicit via the `bucket_start` filter
   for metrics; need the analog on the log-query path.

### Cold-storage differentiation

Bolt's feature list mentions "Cold storage (S3)" as if it's a Bolt
exclusive. Today it's universal because the archiver runs on every
shard regardless of tier. To make it a real feature: non-bolt tiers
get their parquet objects hard-deleted (not archived) when the shard
rotates out of retention. Bolt's objects move to S3 Glacier /
Deep-Archive via a second lifecycle rule, staying queryable with
higher latency. Needs a separate pass on the query planner to wake
objects from Glacier on demand.

## Prerequisites

- `customer_id` must be on every row/object we want to enforce
  against. Metrics rows already have it via `cron_watch_id → watch →
  customer`. Parquet objects need the customer dimension on their
  key/tag path, which may or may not already be there — audit
  `coordinator/archiver.go` first.
- A cheap way to query "tier for customer X" that doesn't round-trip
  Stripe on every sweep tick. `subscriptions` table has it; make
  sure the read path is cacheable.

## Order of operations

1. Audit parquet key structure for customer tagging (small, read-only).
2. Build Postgres per-customer sweep — higher value, scoped work.
3. S3 lifecycle rules per tier — requires step 1 done first.
4. Cold-storage differentiation for bolt — last, biggest planner
   change.

Nothing in this doc is a blocker for the current metrics rollup
work; the query-side filter lands us on a good footing. This plan
is for when the "illusion" above needs to become real.
