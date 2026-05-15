# Shard archive lifecycle test plan

Validate the INGESTING -> READ_ONLY -> ARCHIVED transition end-to-end
in production, then benchmark Lambda cold shard query latency against
live EC2 shards.

## Prerequisites

- Lambda deployed: `kevcons ext release-lambda` (builds cmd/lambda-shard,
  uploads to `weft-cold-shard-query`)
- Coordinator has `PARQUET_DIR` and `LAMBDA_FUNCTION_URL` set (both are
  wired in Terraform; verify via SSM env on the coordinator instance)
- At least one INGESTING shard with real log data in it

## Step 1: Shorten bucket timings

`window-duration` now drives both rotation and archive — the old
separate `--retention-period` flag was removed. A READ_ONLY shard
archives one full `window-duration` after its `window_end` (Op 5,
`coordinator/reconciler.go:159`: `now − window_end > window_duration`).
So a 5m window means rotation at `window_end`, then ARCHIVED ~5m later
— the whole cycle finishes in ~10 min plus reconciler tick slack.

```bash
kevcons ext buckets set --label default --window-duration 5m
```

## Step 2: Pull in the current shard's window_end

The existing INGESTING shard has a `window_end` set from the old wide
(168h) boundary. Force it a couple minutes out so rotation fires on
the next reconciler tick:

```bash
kevcons db tunnel -- psql -c "
  UPDATE shard_records
  SET window_end = to_char(
        now() AT TIME ZONE 'UTC' + interval '2 minutes',
        'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"')
  WHERE state = 'INGESTING';
"
```

## Step 3: Watch the lifecycle

Tail coordinator logs and confirm each transition fires:

1. **INGESTING -> READ_ONLY** (Op 3 — reconciler tick after window_end passes)
2. **READ_ONLY -> ARCHIVED** (Op 5 — reconciler tick after `now - window_end > window_duration`, i.e. ~5m past rotation with the Step 1 setting)
   - Coordinator exports DuckDB to Parquet on S3 (`s3://<cold_storage>/<apps>/<shard_id>.parquet`)
   - Swaps registry entry's Shard impl to LambdaColdShard
   - Retires EBS volume (snapshot -> detach -> delete)
3. **Instance terminated** (Op 6 — next tick after archive)

Confirm via:
```bash
kevcons db tunnel -- psql -c "SELECT id, state, window_end, parquet_path FROM shard_records ORDER BY id;"
```

## Step 4: Verify cold shard is queryable

Run a query through the weft shell that hits the archived time range.
The coordinator should route it to the LambdaColdShard (Lambda function
URL), not to a live EC2 gRPC shard.

```bash
weft sh -c 'cat vimgym.log | head -20'
```

Check coordinator logs for "cold query" or Lambda invocation evidence.

## Step 5: Benchmark — Lambda vs EC2

Goal: compare row-return latency for identical queries against the same
dataset served from (a) a live EC2 DuckDB shard and (b) the Lambda cold
shard reading Parquet from S3.

### Setup

Before archiving, record baseline timings against the EC2 shard. After
archiving, run the same queries against the Lambda shard.

### Queries to bench (run each 5x, take p50)

| Query | Why |
|-------|-----|
| `SELECT count(*) FROM logs WHERE app = 'vimgym'` | Pushdown-only, measures pure scan overhead |
| `SELECT * FROM logs WHERE app = 'vimgym' ORDER BY ts DESC LIMIT 100` | Small result set, tests cold-start + first-byte latency |
| `SELECT * FROM logs WHERE app = 'vimgym' AND message LIKE '%error%'` | Filter scan, moderate selectivity |
| `SELECT * FROM logs WHERE app = 'vimgym'` | Full scan, measures throughput at scale |

### Measurement

Use coordinator-side timing (log the wall-clock from query dispatch to
last row received). The interesting numbers:

- **Cold start penalty**: first Lambda invocation vs subsequent (Lambda
  may need to download the Parquet file from S3 on cold start)
- **Steady-state latency**: p50/p95 of subsequent invocations
- **Throughput**: rows/sec for full-table scans

### Recording results

Drop a markdown table into this file (or a sibling) with:
- Query, EC2 p50 ms, Lambda cold ms, Lambda warm p50 ms, row count

## Step 6: Restore normal timings

After testing, restore production-appropriate values:

```bash
kevcons ext buckets set --label default --window-duration 168h
```

## Failure modes to watch for

- `ParquetDir` empty on coordinator -> ArchiveShardOp silently no-ops
- Lambda not deployed -> function URL 404s -> cold shard Query returns error
- S3 export fails (permissions, bucket policy) -> archive aborts, shard stays READ_ONLY
- Coordinator restarts mid-test -> reconstructs registry from shard_records; ARCHIVED rows with parquet_path get LambdaColdShard re-created
