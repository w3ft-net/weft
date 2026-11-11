# Runbook: archive retention janitor

Operator guide for enabling and running the retention janitor — the daily
job that deletes over-retained customer rows from archived parquet in S3.
For *why* it works this way (mixed-customer shards → row-level rewrite, not
object expiry), see [`../plans/retention-janitor.md`](../plans/retention-janitor.md).

> **This permanently deletes customer data.** It always runs: there is no
> on/off setting (the `janitor_enabled` flag was removed once it was live in
> prod). A new environment needs the compactor Lambda deployed and validated
> before its coordinator starts, or the janitor falls back to an in-process
> compactor that is only fit for dev.

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
4. **Start it**: deploy the coordinator. It starts the janitor at boot.

## Verify it's running

- Coordinator log at boot: `coordinator: retention janitor started (daily)`
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
| `COMPACTOR_FUNCTION_URL` | SSM blob (TF) | — | unset → in-process compactor (dev) |
| `RetentionGrace` | `tenancy/hosted/retention` const | 7d | buffer past advertised retention before deleting |
| `RetentionGCDelay` | `tenancy/hosted/retention` const | 1h | tombstone lifetime before GC (covers in-flight reads) |
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

## Sized archives

New large archives are written by the export in the sized layout: split by log
key, with each key's time buckets wide enough for its own files to come out near
16 MiB (`docs/plans/per-service-tables.md`, "Archive layout"):
`<shard>.attempt-N/sized/customer_id=.../namespace=.../service=.../kind=.../_w=<minutes>/_part=...`.
The janitor only rewrites them. A retention rewrite of a sized (or the first
keyed) archive writes a sized archive, so the pass does not undo the layout; any
other archive is rewritten as a single file, as before. The janitor does not
convert existing archives to the layout: that is a one-off, run by hand with
`scripts/convert_archives.py` (see the plan, "Converting existing archives"). Run it
from the repo root: `python3 scripts/convert_archives.py --stage plan`, then
`--stage convert`, then `--stage apply`. It needs the `SwapShardArchive` RPC, so the
coordinators must already be running a build that has it, and it reaches them through
SSM. Unit tests: `python3 -m unittest scripts/test_convert_archives.py`.

The **compactor Lambda** and the **cold-shard Lambda** must be running the
sized-layout code before a sized archive is rewritten or read: an old compactor
would collapse it into one file, and an old cold shard bounds time by a day,
which can drop rows from keys with wider buckets.

To roll one shard back after a manual conversion, before the old object's
tombstone `delete_after` passes, point `shard_records.parquet_path` back at the
old object, delete that tombstone, and restart the coordinators.

## Disable / roll back

There is no switch to turn the janitor off; stopping it means deploying a
coordinator without it. Already-deleted data is **not recoverable** — the
janitor only ever touches S3 parquet, and shard archival no longer keeps
an EBS snapshot backstop (removed 2026-09-15; parquet in S3 is the sole
durable copy).
