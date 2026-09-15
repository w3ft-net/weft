# Runbook: archive retention janitor

Operator guide for enabling and running the retention janitor — the daily
job that deletes over-retained customer rows from archived parquet in S3.
For *why* it works this way (mixed-customer shards → row-level rewrite, not
object expiry), see [`../plans/retention-janitor.md`](../plans/retention-janitor.md).

> **This permanently deletes customer data.** It is **off by default**
> (`janitor_enabled = false`). Turn it on per environment only after the
> compactor Lambda is deployed and validated.

## What it does, at a glance

Once a day the coordinator runs one pass:

```
PlanRetention            classify every ARCHIVED shard from shard_archive_stats
  └─ per shard ─ ExecutePurge
       Keep      nothing over-retained → skipped
       Unknown   no stats → compactor CaptureStats backfills them (no delete)
       Delete    all rows expired → tombstone the object, retire the shard (→ GONE)
       Rewrite   mixed → compactor rewrites survivors to a new key, swap
                 parquet_path, refresh stats, tombstone the old key
  └─ SweepTombstones      delete S3 objects whose grace (delete_after) elapsed
```

The heavy parquet work runs in the **`weft-compactor` Lambda**; the
coordinator only plans, finalizes (Postgres), and GC-sweeps. A customer
with **no active subscription is never deleted** (treated as "unmapped"),
and deletion lags the advertised retention by a 7-day grace buffer.

## Prerequisites

- `weft-compactor` Lambda exists (Terraform applied) and its code is
  published (`kevcons ext release-lambda compactor`, run by CI's "Deploy
  compactor Lambda" step).
- The coordinator's instance role can invoke it
  (`coordinator_invoke_compactor` policy — both `InvokeFunctionUrl` and
  `InvokeFunction`).
- `shard_archive_stats` is populating. New archives fill it automatically;
  shards archived before that ship are classified `Unknown` and
  backfilled on first pass.

## Enable

1. **Apply infra** (creates the Lambda, IAM, Function URL, CI policy; the
   janitor stays off):
   - `infrastructure-catalog/stacks/asg-weft-application` — the
     `weft-compactor` resources.
   - `infrastructure-live/prod/weft/github-deployment.tf` — the CI deploy
     policy.
   - ⚠️ The plan changes the coordinator's **SSM config blob** (adds
     `COMPACTOR_FUNCTION_URL`). Review that diff and confirm no out-of-band
     `kevcons config set` values are clobbered before applying.
2. **Ship the binary**: merge to the deploy branch so CI publishes the real
   `bootstrap` over the Terraform placeholder. (Manual:
   `kevcons ext release-lambda compactor`.)
3. **Smoke-test the Lambda** against a known archived shard before deleting
   anything — a read-only `capture` op:
   ```
   aws lambda invoke --function-name weft-compactor \
     --payload '{"op":"capture","src":"s3://<cold-bucket>/<service>/<shard>.parquet"}' \
     --cli-binary-format raw-in-base64-out /dev/stdout
   ```
   Expect a JSON body with a `stats` array.
4. **Flip it on**: set `janitor_enabled = true`, apply, and restart/redeploy
   the coordinator so `JANITOR_ENABLED=1` reaches it.

## Verify it's running

- Coordinator log at boot: `coordinator: retention janitor enabled (daily)`
  and `janitor: compactor Lambda at <url>`.
- After a pass: lines like `janitor: purge <shard> (rewrite)` are absent on
  a clean run; errors are logged per-shard without aborting the pass.
- Postgres:
  - `SELECT id, last_purged_at, parquet_path FROM shard_records WHERE last_purged_at IS NOT NULL;`
    — recently-processed shards.
  - `SELECT * FROM parquet_tombstones;` — objects awaiting GC (cleared once
    `delete_after` passes and the sweep deletes them).

## Knobs

| Setting | Where | Default | Notes |
|---|---|---|---|
| `janitor_enabled` | TF var → `JANITOR_ENABLED` env | `false` | master on/off |
| `COMPACTOR_FUNCTION_URL` | SSM blob (TF) | — | unset → in-process compactor (dev) |
| `RetentionGrace` | `logdb/janitor.go` const | 7d | buffer past advertised retention before deleting |
| `RetentionGCDelay` | `logdb/janitor.go` const | 1h | tombstone lifetime before GC (covers in-flight reads) |
| interval | `startRetentionJanitor` | 24h | the daily cadence |
| `compactor_lambda_memory` | TF var | 2048 MB | bump if rewrites OOM |

## What it touches

- **S3**: writes rewritten parquet to a new `*.r<ns>.parquet` key; deletes
  superseded/retired objects (deferred via tombstones).
- **`shard_records`**: `parquet_path` (swap), `last_purged_at`; archived
  shards may transition to `GONE` (row deleted by `onEnterGone`).
- **`shard_archive_stats`**: refreshed after a rewrite / backfilled on
  Unknown.
- **`parquet_tombstones`**: insert on purge, delete on GC.

## Troubleshooting

- **`capture stats … No files found`** — the shard's `parquet_path` points
  at a missing object (a zombie row). Logged and skipped; safe. Investigate
  the shard if it persists.
- **`compactor lambda returned 5xx`** — check the `weft-compactor` log
  group. Common: missing S3 write IAM, or OOM on a large rewrite (raise
  `compactor_lambda_memory`).
- **Queries 404 after a rewrite** — the registry didn't repoint;
  `RefreshArchivedPath` should run on every `OutcomeRewritten`. A
  coordinator restart re-reads `parquet_path` from Postgres and self-heals.
- **A paying customer's data disappeared** — should be impossible (unmapped
  customers are preserved, paid tiers honored). Check `subscriptions` for an
  unexpected `status`/`expires_at`, and the `parquet_tombstones` /
  coordinator logs for what was deleted and when.

## Disable / roll back

Set `janitor_enabled = false`, apply, restart the coordinator. The Lambda
and IAM can stay (idle). Already-deleted data is **not recoverable** — the
janitor only ever touches S3 parquet, and shard archival no longer keeps
an EBS snapshot backstop (removed 2026-09-15; parquet in S3 is the sole
durable copy).
