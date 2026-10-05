# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What is weft?

A centralized, multi-tenant log analysis service. Customers connect AWS accounts; weft ingests logs from CloudWatch (or via the weftd agent) into time-sharded DuckDB+Parquet nodes. Users query logs through a custom shell (`weft`) that presents log sources as pseudofiles and uses Unix pipeline idioms (awk/sed/jq/grep) powered by the [warp](https://github.com/graybeardcorp/warp) interpreter. The query planner pushes predicates down into shard-level SQL and runs the rest as gpipe compute stages.

## Build & run

```bash
# Build the server
go build -o server ./cmd/server

# Run (default port 8080)
./server
# or with custom port
PORT=9090 ./server
```

The project depends on `github.com/graybeardcorp/warp` via a local replace directive (`../warp`), so that repo must be checked out as a sibling.

## Architecture

**Coordinator** — single Go process that serves the CLI API and web UI, parses pipelines, extracts pushdown predicates, routes queries to shards, merges results, and fans out non-pushable stages to the compute pool. Also runs the Kinesis ingest consumer (Phase 1).

**Shard nodes** — time-partitioned DuckDB instances. Each owns a window of log data and is its own finite state machine (`logdb.Shard`): LAUNCHING → INGESTING (→ DEGRADED, the ingest circuit breaker) → READ_ONLY → ARCHIVING → ARCHIVED, with SUSPENDED (recoverable instance-death branch) and GONE (cleanup sink). A bucket can hold several INGESTING shards at once (its `Target`); the bucket FSM rotates/scales the cohort. The coordinator hosts the FSMs but each shard's own `Evaluate` drives its convergence — there's no separate reconciler. See `logdb/states.go` and `docs/plans/asg-as-api-stateless.md` for the transition table.

**Compute pool** — stateless gpipe workers for post-filter pipeline stages. Run by `cmd/compute`; the coordinator dispatches every query to it over gRPC (`--compute`, required) and never runs pipeline stages itself.

**weftd** — optional Go agent deployed on customer instances for sub-second log tailing via gRPC. Ships structured lines to the coordinator.

**CLI shell** — `weft` drops into an interactive shell. Log sources appear as pseudofiles (`vimgym.log`, `warp.log`). The warp bash interpreter handles pipes/awk/sed/jq natively. The query planner analyzes the full pipeline as one unit and folds filters into SQL WHERE clauses.

Inter-node protocol is gRPC (shard queries, weftd ingest, tail subscriptions). Coordinator-to-shard lifecycle commands use HTTP (`/health`, `/transition`, `/shutdown`).

## Multi-tenancy

`$CUSTOMER` is a system-level field injected unconditionally by the coordinator as a `WHERE customer = '...'` predicate on every shard query. It is never exposed as a user-facing meta-variable — it is a row-level security boundary.

## Key design docs

All in `docs/`:
- `architecture.md` — system overview and component interactions
- `query-execution.md` — predicate extraction, pushdown rules, pipeline splitting, shard routing
- `shard-lifecycle.md` — shard states, window rotation, retention, DuckDB schema, Docker Compose dev setup
- `ingestion-cloudwatch.md` — subscription filters, Kinesis consumer, schema detection
- `ingestion-weftd.md` — agent architecture, gRPC protocol, deployment
- `ingestion-syslog.md` — TLS syslog listener (Render and future platforms), the Producer interface, how to add a new syslog-speaking platform
- `materialized-views.md` — saved pipelines as continuously-updated view tables
- `cli.md` — shell design, pseudofiles, meta-variables, commands
- `known-issues.md` — verified, unfixed gaps worth remembering (root cause, impact, why it's not fixed yet)

## Local development

Docker Compose with containerized DuckDB shard nodes. Dev config: 1-hour shard windows, 2-hour retention (2 shards at steady state). Coordinator manages containers via Docker API instead of EC2/ASG.

## Project status

Early scaffold stage. The server binary is a minimal HTTP skeleton with a `/health` endpoint. The frontend (`frontend/src/`) is empty. Design documents are complete and define the target architecture.
