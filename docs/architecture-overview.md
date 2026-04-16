# Weft Architecture Overview

## What It Is

A multi-tenant log analysis platform. Customers connect AWS accounts or install agents; weft ingests their logs into time-sharded DuckDB nodes. Users query logs through a Unix-flavored shell where log sources appear as pseudofiles and pipelines (`grep`, `awk`, `jq`, `sort`) are first-class — the query planner pushes filter predicates down into shard-level SQL and runs the rest as bash compute stages via the `warp` interpreter.

## The Six Binaries

| Binary | Port(s) | Role |
|---|---|---|
| **coordinator** | 9020 gRPC, 9021 HTTP | Central brain. Shard lifecycle, query routing, ingest buffering, cron watches, tail broker. |
| **api** | 8080 HTTP | REST server for the web UI. JWT auth, magic links, Stripe billing, Slack OAuth. Proxies queries to coordinator via gRPC. |
| **shard** | 9000 gRPC, 9001 HTTP | One DuckDB instance per process. Accepts SQL queries and ingest batches. Reports heartbeat to coordinator. |
| **compute** | 9010 gRPC | Stateless bash executor for non-pushable pipeline stages (sort, uniq, awk transforms). |
| **weft** | — | CLI client. Interactive shell with bubbletea TUI; also compiles to WASM for the web terminal. |
| **weftd** | — | Agent daemon on customer machines. Tails files/systemd journals, streams `LogRow`s to coordinator via gRPC. |

Plus **lambda-shard** for cold-storage Parquet queries via Lambda.

## Data Flow

### Write path

```
weftd (file/journal tail) ──gRPC IngestStream──▶ Coordinator RingBuffer (1M rows)
                                                       │
CloudWatch ──Kinesis──▶ Coordinator Poller ────────────┘
                                                       │
                                                  StartDrain() goroutine
                                                  groups by app, routes to
                                                  INGESTING shard
                                                       │
                                                       ▼
                                                 Shard (DuckDB)
                                                 INSERT INTO logs
```

### Read path

```
CLI / Web UI
    │
    ▼
Coordinator.Query(pipeline, timeRange, customer)
    │
    ├─ Planner.Plan(pipeline) → QueryPlan
    │    ├─ ShardSQL (pushdown WHERE clauses)
    │    └─ ComputeStages (remaining bash pipeline)
    │
    ├─ Fan out ShardSQL to all matching shards (by time window)
    │
    ├─ If compute stages:
    │    Coordinator creates job on compute worker (CreateJob RPC)
    │    Shards stream rows to coordinator (QueryToSink)
    │    Coordinator forwards to compute worker (SendRows RPC)
    │    Coordinator fetches result (GetResult RPC)
    │
    │  Else: merge LogRows, apply limit/sort
    │
    └─ Stream results back to client
```

In prod, the coordinator connects to a remote compute ASG via `GRPCComputePool`. In dev/Docker, it runs compute in-process via `LocalComputePool`. The compute binary itself always runs warp/bash locally — the "remote" part is that it runs on a separate instance from the coordinator.

See `docs/optimizations.md` for known optimization opportunities (direct shard-to-compute streaming, narrow SELECT for compute path, schema-aware projection).

### Live tail (`tail -f`)

```
Historical rows streamed first, then TailBroker.Subscribe()
→ new ingest rows matched by app + grep patterns
→ pushed to client via gRPC stream
```

## Shard Lifecycle

```
LAUNCHING → INGESTING → READ_ONLY → EXPIRING → ARCHIVED → TERMINATED
```

- **Provisioner** polls every 5s (dev) / 5min (prod), checks row count, disk bytes, and age.
- Rotation: launch new shard → Configure RPC → swap INGESTING designation → old shard becomes READ_ONLY.
- After retention period: export to Parquet on S3 → ARCHIVED (queryable via ColdShard or LambdaColdShard) → terminate instance.
- Dead shards reaped via heartbeat timeout.

### Rotation thresholds

| | Dev | Prod |
|---|---|---|
| MaxRows | 50k | 5M |
| MaxDiskBytes | 50MB | 500MB |
| MaxDuration | 1h | 8h |

## Multi-Tenancy and Auth

- **`customer`** column injected into every shard query as a WHERE clause — row-level security boundary, never exposed to users.
- **Namespace** groups apps within a customer (e.g., `weft/coordinator`, `weft/api`).
- **Agent tokens** (`wft_xxxx`) authenticate weftd agents to a specific customer + namespace.
- **Web users** authenticate via magic link (Sendgrid) or OAuth device flow → JWT (HS256, 30-day expiry).
- **RBAC**: per-namespace permissions (can_read, can_create_tokens, can_manage_watches, can_manage_functions, can_manage_integrations). Owners bypass all checks.

## Source Schemas

Every log source has a schema (`ingest.Schema`) that tells the parser how to interpret raw lines. Schemas are stored as YAML in the `log_groups` table (`SchemaYAML` column) for CloudWatch sources and applied inline by `weftd` for agent-ingested sources.

### Schema fields

| Field | Purpose |
|---|---|
| `format` | Body structure: `"text"` (whitespace fields), `"json"` (entire line is JSON), `"prefix_json"` (fields then JSON tail) |
| `envelope` | Transport wrapper: `"kevcons"` strips `[timestamp] [instance]` prefix; `"none"` = no envelope |
| `field_separator` | Custom delimiter for positional fields (default: whitespace) |
| `timestamp_field` | 1-based field index or JSON dot-path (`.ts`); `"none"` = use ingestion time |
| `timestamp_format` | Go `time.Parse` layout |
| `level_field` | Field index or JSON dot-path for log level |
| `level_pattern` | Optional regex to extract level from field value |
| `json_start_field` | For `prefix_json`: 1-based index where JSON begins |
| `expand_keys` | JSON keys to promote to DuckDB columns for faster pushdown |

### How schemas affect storage

For `format: "json"` sources (like `weft/metrics`), the agent sets both `Raw` and `PayloadJson` to the same JSON bytes. The DuckDB `logs` table stores `raw` as TEXT and `payload` as JSON — for JSON sources, `raw` is already valid JSON that jq can consume directly.

For `format: "text"` sources, `raw` is the original log line, `fields` is the whitespace-split array, and `payload` is empty (`{}`).

For `format: "prefix_json"` sources, `raw` is the full line, `fields` covers the prefix, and `payload` is the JSON tail.

### Gap: the planner is schema-unaware

The planner (`planner/`) has no access to source schemas. This causes unnecessary work for JSON sources:

When a jq pipeline like `tail log/weft/metrics.log | jq 'select(.type == "memory") | .pct'` hits the planner:

1. **Parse**: source = `tail`, app = `weft/metrics`, stage = `jq 'select(.type == "memory") | .pct'`
2. **jq pushdown** (`jq.go:tryJqStagePushdown`): Flattens the pipe chain. `select(.type == "memory")` is pushable → SQL predicate `json_extract_string(payload, '$.type') = 'memory'`. `.pct` is not a select → becomes compute stage `jq '.pct'`.
3. **Projection** (`project.go:extractJqProjection`): Sees `jq '.pct'` → extracts `JSONKeys: ["pct"]`.
4. **Apply projection** (`apply_project.go:applyJSONProjection`): Rewrites SELECT to `CAST(payload AS TEXT) as raw, json_object('pct', ...) as payload`.

The `CAST(payload AS TEXT) as raw` rewrite is redundant for JSON sources — `raw` already IS valid JSON. The narrowed `payload` diverges from `raw`. This works today because the compute layer only reads `raw`, but it's fragile and wasteful.

**Fix**: The planner should accept schema metadata (at minimum, the source format). For `format: "json"` sources, skip JSON projection entirely — `raw` is already the JSON string that jq will consume. The `select` pushdown to SQL is still valuable; only the projection rewrite is unnecessary.

## Query Planner

The planner analyzes a full Unix pipeline as one unit:

- **Pushable** → SQL WHERE: `grep "ERROR"`, `awk '$3 == "500"'`, `jq 'select(.status >= 400)'`
- **Non-pushable** → compute stages: `sort`, `uniq -c`, `sed s/foo/bar/`, formatting transforms
- **Projection**: `cut -d, -f1,3` or `awk '{print $2}'` → only fetch needed fields
- **Follow mode**: `tail -f` detected → historical query + live subscription with grep patterns

### Debugging the planner

To trace what the planner does with a pipeline, understand the three-phase split:

**Phase 1 — Parse** (`planner/parse.go`): The pipeline string is parsed by the warp bash parser. The first command is the source (`cat`/`head`/`tail` + pseudofile). Remaining commands become `stageInfo` structs (cmd + args).

**Phase 2 — Pushdown** (`planner/planner.go:Plan`): Stages are walked left-to-right. Each stage is offered to `tryPushdown`, which dispatches by command:
- `awk` → `tryAwkPushdown` (meta-var comparisons, field comparisons, regex, NR ranges)
- `jq` → `tryJqStagePushdown` (select expressions with field/literal comparisons)
- `grep` → `tryGrepPushdown` (literal patterns → LIKE/ILIKE)
- `sed` → `trySedPushdown` (address patterns only, e.g., `sed -n '/pattern/p'`)
- `head`/`tail` → ORDER BY + LIMIT

Once a stage fails to push down, all subsequent stages become compute stages (no reordering).

**Phase 3 — Projection** (`planner/project.go`): The first compute stage is examined for field/key references. If it only reads specific fields (`cut -f1,3`, `awk '{print $2}'`) or JSON keys (`jq '.pct'`), a `Project` is extracted. The coordinator uses this to narrow the SQL SELECT clause, reducing data transfer from shards.

**Partial pushdown**: A single jq expression like `select(.x == 1) | .y` is split — the select becomes a SQL predicate, `.y` becomes a compute stage. The awk equivalent: `$LEVEL == "ERROR" { print $3 }` splits into a WHERE clause and a compute `awk '{ print $3 }'`.

### Example trace: `tail log/weft/metrics.log | jq 'select(.type == "memory") | .pct'`

```
Pipeline: tail log/weft/metrics.log | jq 'select(.type == "memory") | .pct'

Parse:
  source: cmd=tail, app=weft/metrics, limit=10, follow=false
  stages: [jq 'select(.type == "memory") | .pct']

Pushdown:
  jq stage → tryJqStagePushdown
    flatten pipe: [select(.type == "memory"), .pct]
    select(.type == "memory") → pushable
      predicate: json_extract_string(payload, '$.type') = 'memory'
    .pct → not a select → stop
    result: pushed=true, predicate=above, compute="jq '.pct'"

  ShardSQL: SELECT * FROM logs
            WHERE app = 'weft/metrics'
            AND json_extract_string(payload, '$.type') = 'memory'
            ORDER BY ts DESC LIMIT 10

  ComputeStages: ["jq '.pct'"]

Projection:
  extractJqProjection on "jq '.pct'" → JSONKeys: ["pct"]
  applyJSONProjection rewrites SELECT to:
    CAST(payload AS TEXT) as raw, json_object('pct', ...) as payload

Execution:
  coordinator injects customer WHERE, applies projection
  fans out to matching shards
  shard returns rows where raw = full JSON text
  compute feeds raw to: jq '.pct'
  jq extracts .pct from each JSON line → output
```

## PostgreSQL Store

Single Postgres DB (gorm auto-migrate) holds all configuration and metadata:

**Core models:** Customer, Namespace, Account, User, MagicLink, DeviceCode, Subscription, TeamMember, NamespacePermission

**Operational:** AgentToken, ShardRecord, LogGroup, ShellFunction, CronWatch, CronResult, NotifyConfig, AlertIncident, SavedQuery

**Metered:** usage_hourly / usage_daily (pg_partman partitioned), ingestion_errors, shard_heartbeats

## Frontend

React 19 + Vite + TypeScript SPA at `/frontend/`:

- **Shell page**: xterm.js terminal backed by Go compiled to WASM (`cmd/wasm/`). Sidebars for `/bin` (shell functions) and `/cron.d` (watches).
- **Dashboard**: watch sparklines, function list, ingestion status.
- **Account**: Stripe subscription, usage stats, team management, Slack/PagerDuty integrations.
- **Namespaces**: RBAC management.
- **Auth**: magic link login + device flow.

API client in `frontend/src/lib/api.ts` covers all REST endpoints. Auth state in React context with JWT in localStorage.

## Infrastructure

- **Prod**: AWS account 220664822695, domain w3ft.net, us-east-2
- **Coordinator**: behind ALB, 1-2 instances
- **Shards**: tiered ASGs — small (t4g.micro, 2 running), medium (t4g.small, 0), large (t4g.medium, 0); EBS gp3
- **Build**: kevcons + Packer AMIs
- **Dev**: Docker Compose (postgres, 2 shards, coordinator, api, weftd, minio for S3)

## gRPC Services

### ShardQuery (port 9000, runs on each shard)

- `Query(QueryRequest) → stream LogRow` — pushdown SQL
- `QueryToCompute(QueryToComputeRequest) → QueryToComputeResponse` — **stub**: intended for direct shard-to-compute streaming, currently returns empty
- `Ingest(IngestRequest) → IngestResponse` — batch row insertion
- `IngestStream(stream LogRow) → IngestStreamResponse` — long-lived agent stream
- `Configure(ConfigureRequest) → ConfigureResponse` — blank shard initialization

### WeftCompute (port 9010, compute workers)

- `CreateJob(CreateJobRequest) → CreateJobResponse` — pipeline stage setup
- `SendRows(stream ComputeMessage) → SendRowsResponse` — row streaming
- `GetResult(GetResultRequest) → GetResultResponse` — wait for completion

### WeftCoordinator (port 9020, client-facing)

- **Query**: `Query(CoordinatorQueryRequest) → stream CoordinatorQueryResponse`
- **Sources**: AddSource, ListSources, RemoveSource
- **Tokens**: CreateToken, ListTokens, RevokeToken
- **Shell**: SaveFunction, GetFunction, ListFunctions, DeleteFunction
- **Watches**: SaveWatch, GetWatch, ListWatches, DeleteWatch, GetWatchResults
- **Heartbeat**: `Heartbeat(ShardHeartbeatRequest) → HeartbeatResponse`
- **Metering**: `GetUsage(GetUsageRequest) → GetUsageResponse`
