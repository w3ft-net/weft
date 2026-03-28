# Shard Lifecycle

## Overview

Shard nodes are time-partitioned DuckDB instances that each own a window of log data. The coordinator is the sole authority over shard lifecycle — it launches, monitors, transitions, and terminates shards. Shards are passive workers; they do what the coordinator tells them.

## Shard states

```
LAUNCHING → INGESTING → READ_ONLY → EXPIRING → TERMINATED
                            ↑
                     (coordinator signals
                      window close)
```

- **LAUNCHING** — container starting, DuckDB initializing, registering with coordinator
- **INGESTING** — actively accepting writes from the ingestion pipeline, serving queries, pushing to tail subscribers
- **READ_ONLY** — window closed, no longer accepting writes, still serving queries
- **EXPIRING** — past retention period, coordinator has stopped routing queries to it
- **TERMINATED** — container destroyed

Only one shard is in the INGESTING state at a time. When the coordinator transitions the active shard to READ_ONLY, it simultaneously launches a new shard to take over ingestion.

## Coordinator as controller

The coordinator owns all lifecycle decisions:

### Monitoring

The coordinator polls each shard's health endpoint on a regular interval. Shard health reports include:

- Current state (INGESTING, READ_ONLY, etc.)
- Time window (start, end)
- Row count
- Disk usage
- Last write timestamp (for ingesting shards)

### Window rotation

The coordinator runs a timer based on the configured shard duration (1 hour in dev, 8-24 hours in production). When the window closes:

1. Launch a new shard container
2. Wait for new shard to report healthy
3. Tell the old shard to transition to READ_ONLY
4. Redirect ingestion to the new shard
5. Resubscribe any active `tail -f` sessions to the new shard

### Retention enforcement

The coordinator tracks each shard's time window. When a shard's window falls entirely outside the retention period:

1. Stop routing queries to it
2. Transition to EXPIRING
3. Terminate the container

### Failure recovery

If a shard stops responding to health polls:

1. Mark it as unhealthy in the shard registry
2. Launch a replacement shard for the same time window
3. Replacement shard re-ingests from CloudWatch (or weftd replay) for its window
4. Once replacement is healthy, terminate the failed shard's container (if still running)
5. Resubscribe any affected tail sessions

CloudWatch is the source of truth — shard data is ephemeral and always re-derivable.

## Shard API

Each shard exposes a gRPC service for queries (see [query-execution.md](query-execution.md)) and an HTTP health endpoint for coordinator polling:

```
GET /health

{
  "state": "INGESTING",
  "window_start": "2026-03-28T10:00:00Z",
  "window_end": "2026-03-28T11:00:00Z",
  "row_count": 142387,
  "disk_bytes": 28491200,
  "last_write": "2026-03-28T10:47:32Z"
}
```

The coordinator also issues lifecycle commands:

```
POST /transition { "to": "READ_ONLY" }
POST /shutdown
```

## DuckDB schema

Each shard has one table:

```sql
CREATE TABLE logs (
    line_num    BIGINT,
    raw         TEXT,
    fields      TEXT[],          -- whitespace-split for awk $N
    ts          TIMESTAMP,
    level       TEXT,
    app         TEXT,
    instance    TEXT,
    payload     JSON,            -- DuckDB native JSON type
    -- expanded keys (from schema detection):
    -- added dynamically per app schema
);
```

Expanded keys (user_id, action, duration_ms, etc.) are added as columns based on the schema detected for each app. See [ingestion-cloudwatch.md](ingestion-cloudwatch.md) for schema detection.

## Local development (Docker Compose)

The coordinator manages shard containers via the Docker API instead of EC2/ASG:

- `docker create` / `docker start` to launch shards
- `docker stop` / `docker rm` to terminate
- Health polling via the same HTTP endpoint
- DuckDB data on Docker volumes (survives container restart for testing, disposable)

Configuration for dev:

```yaml
shard:
  duration: 1h        # short window for testing lifecycle
  retention: 2h       # 2 shards active at steady state
  health_interval: 5s
```

This means at steady state there are 2 shard containers: one ingesting, one read-only. Every hour, a new shard launches, the old ingesting shard goes read-only, and the oldest read-only shard gets terminated. This is exercisable in a single dev session.

## Production (ASG)

Same coordinator logic, but launching/terminating via EC2 API:

- Shard nodes are EC2 instances in an ASG (coordinator manages desired count directly, not autoscaling policies)
- EBS gp3 volumes for DuckDB storage
- Instance type: t3.micro (DuckDB is memory-efficient on local Parquet)

## Open questions

- **Graceful handoff semantics:** during window rotation, there's a moment where both the old and new shard exist. Should the coordinator buffer writes during the transition, or is it acceptable to have a brief overlap where the old shard gets a few extra rows past its nominal window end?
- **Shard discovery on coordinator restart:** if the coordinator itself restarts, it needs to rediscover existing shards. In Docker this means listing containers with a weft label. In prod, querying the ASG. Should the coordinator also persist its shard registry to disk for faster recovery?
- **Parallel re-ingestion:** when recovering a failed shard, re-ingesting its full time window from CloudWatch could take time. Should we allow partial query results from a recovering shard (serve what's been re-ingested so far)?
