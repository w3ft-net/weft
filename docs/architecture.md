# Weft — Architecture Overview

## What is weft?

A centralized log analysis service. Customers connect their AWS accounts, and query logs using gpipe pipelines (awk/sed/jq/grep). Logs are ingested from CloudWatch into time-sharded DuckDB+Parquet nodes. Queries are distributed across shards with predicate pushdown, then processed by a stateless gpipe compute pool.

## System components

```
weft CLI / Browser UI
        │
        ▼
   Coordinator
   ├── Parses pipeline, extracts predicates
   ├── Routes to shard nodes by time range
   ├── Merges shard results
   └── Fans out remaining stages to compute pool
        │                              │
        ▼                              ▼
   Shard Nodes                    Compute Pool
   (time-partitioned)             (stateless gpipe workers)
   DuckDB + Parquet               Pipeline execution
   Predicate pushdown             Post-filter stages
   Meta-var injection
```

### Coordinator

Single process (1-2 instances behind ALB for HA). Responsibilities:

- Serve the CLI API and web UI
- Parse incoming pipelines and extract pushdown predicates
- Route queries to the correct shard nodes based on time range
- Merge results from multiple shards
- Fan out non-pushable pipeline stages to the compute pool
- Stream results back to the client (WebSocket for browser, streaming response for CLI)

### Shard nodes

Each shard owns a configurable time window of log data for all customers. Data is stored in DuckDB with Parquet backing on local disk. See [shard-lifecycle.md](shard-lifecycle.md).

### Compute pool

Stateless gpipe workers that receive pre-filtered rows from shard nodes and execute the remaining pipeline stages. Autoscaled on query queue depth.

## Local development

Docker Compose with containerized DuckDB shard nodes. This lets us test multi-node behavior (shard handoff, coordinator routing, retention expiry) locally without real ASGs. Initial config: 2 shard containers, 1-hour retention each — kept short so shard lifecycle behavior is exercisable in a dev session.

## Production deployment

- Shard nodes in an ASG, time-driven scaling (launch/terminate by schedule)
- Coordinator behind ALB (1-2 instances)
- Compute pool deferred to Phase 3 — coordinator runs gpipe inline initially

## Multi-tenancy

Weft is multi-tenant. The data hierarchy is:

```
Customer (tenant — Kevlar, Acme Corp, ...)
  └── AWS Account (one IAM role per account)
       └── App (vimgym, callbox, ...)
            └── Log Group / weftd source
```

`$CUSTOMER` is a system-level field applied automatically by the coordinator based on the authenticated user's token. It is never exposed as a user-facing meta-variable — it is a row-level security boundary. Every query the coordinator sends to shards includes a `WHERE customer = '...'` predicate, injected unconditionally.

User-facing meta-variables (`$APP`, `$LEVEL`, `$TS`, `$INSTANCE`) are documented in [cli.md](cli.md).

## Interfaces

1. **CLI** (first) — `weft` is the shell. Pseudofiles per app, Unix pipeline idioms, `tail -f` with live push. See [cli.md](cli.md). Enables end-to-end testing without a UI.
2. **Web UI** (React, later) — pipeline editor, streaming results, app/time selectors.

## Key dependencies

- [github.com/kevlarcon/warp](https://github.com/kevlarcon/warp) — awk, sed, jq, bash interpreters + gpipe
- DuckDB (via CGo or shell exec)
- AWS SDK — CloudWatch Logs, EC2/ASG for shard lifecycle

## Design documents

- [shard-lifecycle.md](shard-lifecycle.md) — time-sharded storage, ASG controller, retention
- [ingestion-cloudwatch.md](ingestion-cloudwatch.md) — CloudWatch polling/subscription, schema detection
- [ingestion-weftd.md](ingestion-weftd.md) — direct-ingest agent for sub-second log tailing
- [query-execution.md](query-execution.md) — predicate extraction, pushdown, routing, fan-out
- [materialized-views.md](materialized-views.md) — saved pipelines as indexes/subsets
- [cli.md](cli.md) — CLI design and commands
