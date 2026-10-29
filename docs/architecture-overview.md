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
weftd (file/journal tail) ─────────────gRPC IngestStream──▶ Coordinator RingBuffer (1M rows)
                                                                  │
CloudWatch ──subscription filter──▶ customer's Firehose          │
  ──HTTP──▶ api's CloudWatchAdapter ──gRPC IngestStream──────────┤
Heroku/syslog ──HTTP/TCP──▶ api's adapters ──gRPC IngestStream───┘
                                                                  │
                                                             StartDrain() goroutine
                                                             groups by app, routes to
                                                             INGESTING shard
                                                                  │
                                                                  ▼
                                                            Shard (DuckDB)
                                                            INSERT INTO logs
```

CloudWatch, Heroku, and syslog sources all push over HTTP to the `api`
process, which shares one `enrichRows`/`IngestHandler` pipeline before
forwarding to the coordinator over the same `IngestStream` gRPC weftd uses
— see [ingestion-cloudwatch.md](ingestion-cloudwatch.md).

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
    ├─ Resolve matching shards (by time window) → ShardDescriptors
    │
    ├─ ExecuteQueryStream RPC to the compute worker (every query,
    │  including zero-stage ones):
    │    Compute fans ShardSQL out to the shards (QueryToSink)
    │    Compute k-way merges rows, applies the global limit
    │    Compute runs ComputeStages through warp, streams line batches back
    │
    └─ Stream results back to client
```

The coordinator always talks to a separate compute worker through `compute.Client` (gRPC); it refuses to start without `--compute`. The worker (`cmd/compute`) serves a `compute.Engine`, which does the shard fan-out, merge, and warp stages. Tests run a `compute.Engine` in-process in place of the client.

See `docs/optimizations.md` for known optimization opportunities (direct shard-to-compute streaming, narrow SELECT for compute path, schema-aware projection).

### Live tail (`tail -f`)

```
Historical rows streamed first, then TailBroker.Subscribe()
→ new ingest rows matched by app + grep patterns
→ pushed to client via gRPC stream
```

## Shard Lifecycle

Shards are **durable, volume-bound** entities. A shard's data lives on
a dedicated gp3 EBS volume tagged with its `shard_id`; the EC2
instance is just the current compute host. Instance death ≠ shard
death — the volume reattaches to a replacement instance and the shard
continues under the same identity.

See [shard-lifecycle.md](shard-lifecycle.md) for full detail and
[plans/ebs-shard-volumes.md](plans/ebs-shard-volumes.md) for the
EBS rollout design.

### States

```
              ┌─────────────┐
        ┌────▶│  INGESTING  │◀──┐──┐
christen│     └─────────────┘   │  │
        │                       │  │ wall-clock
        │     ┌─────────────┐   │  │ WindowEnd
        └─────│   ADOPTED   │   │  ▼
              └─────────────┘   │ ┌─────────────┐
                                │ │  READ_ONLY  │◀─┐
                 fresh EBS      │ └─────────────┘  │
                 no shard.json  │        │      rebind
                                │        │ retention
                 rebind ────────┘        ▼ boundary
                 (known id)              ┌─────────────┐
                                         │  ARCHIVING  │
                                         └─────────────┘
                                                │ detach +
                                                ▼ delete
                                         ┌─────────────┐
                                         │  ARCHIVED   │
                                         └─────────────┘
```

### Launch flow (christen)

1. ASG launches a new instance behind an `EC2_INSTANCE_LAUNCHING`
   hook (Pending:Wait).
2. Hook publishes to SNS → SQS; coordinator's lifecycle poller
   receives the event.
3. Coordinator decides CHRISTEN (no orphan volume for this
   tier/AZ): `CreateVolume` tagged `shard_id=pending-<instance>`,
   `AttachVolume` at `/dev/sdf`.
4. `CompleteLifecycleAction` with CONTINUE — instance finishes
   cloud-init. `startup.sh` mounts the new volume at
   `/opt/app/data`.
5. Shard service boots, finds no `shard.json` (fresh volume),
   heartbeats with empty `shard_id` + its `instance_id`.
6. Coordinator's `Heartbeat` RPC handler routes the empty-id
   heartbeat through `Provisioner.ProcessHeartbeat`, which christens
   inline: mints `shard-YYYYMMDDThhmm-N`, sends `Configure` RPC,
   rewrites the volume tag from `pending-<instance>` to the real
   shard id, records `ebs_volume_id` on the registry entry.

### Recovery flow

1. Instance hosting shard S dies. Coordinator detects stale
   heartbeat but **preserves** the registry entry because
   `EBSVolumeID` is populated.
2. ASG health-check launches a replacement; hook fires.
3. Coordinator sees an `available` volume tagged
   `shard_id=<S>` in the new instance's AZ → decides RECOVER
   instead of CHRISTEN → reattaches the existing volume.
4. Instance boots, mounts, reads `shard.json`, heartbeats as S.
   Rebind arm of `ProcessHeartbeat` rewires addr/instance in the
   registry. Same shard id, same data.

### Wall-clock rotation

- **INGESTING → READ_ONLY**: provisioner ticker checks each
  INGESTING shard; when wall-clock passes `WindowEnd`, state flips
  to READ_ONLY and is persisted. Writes stop routing there;
  queries still work.
- **READ_ONLY → ARCHIVING**: when `now - WindowEnd > RetentionPeriod`,
  coordinator exports DuckDB to Parquet, detaches and deletes the EBS
  volume (no snapshot — the parquet export already covers durability),
  terminates the instance. Queries transparently route to the ColdShard
  / LambdaColdShard reader from then on.

### Config knobs (`ProvisionerConfig`)

| Knob | Default (prod) | Role |
|---|---|---|
| `WindowDuration` | `7d` | One hot window |
| `RetentionPeriod` | `0` (archive disabled) | How long READ_ONLY lingers past WindowEnd before archive |
| `PollInterval` | `5s` | Reconciliation tick (ARCHIVED and GONE shards are skipped); inline christen via `Heartbeat` RPC runs on every heartbeat. A shard is stale after 60s of silence, or after 30s if EC2 reports its instance gone |
| `ConfigureRetries` / `ConfigureBackoff` | `12` × `5s` | gRPC retry on shard startup |

Dev/docker uses shorter values via `DevProvisionerConfig`. The
legacy `MaxRows`/`MaxDuration`/`MaxDiskBytes` triggers and the
`rotate()` path they fed have been removed — INGESTING → READ_ONLY
is driven exclusively by the wall-clock `WindowEnd` check in
`provisioner.check()`.

### Failure recovery and reap

- A shard with a bound EBS volume is **never** reaped on stale
  heartbeat — ASG + lifecycle hook drive recovery.
- A shard without a volume (pre-EBS legacy) follows the old reap-
  and-terminate path.
- Brand-new registry entries get a 2-minute grace period so the
  reap pass doesn't race a freshly-christened shard before it can
  heartbeat with its minted id.

## Multi-Tenancy and Auth

- **`customer`** column injected into every shard query as a WHERE clause — row-level security boundary, never exposed to users.
- **Namespace** groups apps within a customer (e.g., `weft/coordinator`, `weft/api`).
- **Agent tokens** (`wft_xxxx`) authenticate weftd agents to a specific customer + namespace.
- **Web users** authenticate via magic link (Sendgrid) or OAuth device flow → JWT (HS256, 30-day expiry).
- **RBAC**: per-namespace permissions (can_read, can_create_tokens, can_manage_watches, can_manage_functions, can_manage_integrations). Owners bypass all checks.

## Source Schemas

Every log source has a schema (`ingest.Schema`) that tells the parser how to interpret raw lines, stored as YAML in the `log_groups` table (`SchemaYAML` column). `weftd` applies it client-side before sending rows; every HTTP/syslog-pushed source (CloudWatch, Heroku, Render) is parsed server-side by a shared `enrichRows`/`enrichRow` step (`api/adapter.go`) that looks the schema up cache-aside.

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

The planner (`planner/`) has no access to source schemas. Today this is mostly fine — single-key jq projections fold directly into the SELECT (see the example trace below) — but a few cases still leave money on the table:

- **Object/array construction in jq** (`jq '{a: .x, b: .y}'`): falls back to `CAST(payload AS TEXT) as raw` so jq can run on the full JSON. With a known JSON source we could either skip the cast (raw is already JSON text) or narrow to a `json_object(...)` over only the referenced keys.
- **Tracking which keys jq actually reads** through more complex chains, so we can prune them at the shard.

See `docs/optimizations.md` for the planned next step.

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

**Phase 3 — Projection** (`planner/project.go`): The first compute stage is examined for field/key references. If it only reads specific fields (`cut -f1,3`, `awk '{print $2}'`) or pushable jq shapes (a single key `jq '.pct'`, a nested path `jq '.foo.bar'`, an array index `jq '.items[0]'`, object construction `jq '{a: .x}'`, array construction `jq '[.x, .y]'`), the access is folded directly into the SELECT and the stage drops out of compute. When a jq pipe has a pure-path prefix but a non-pushable suffix (`jq '.foo | length'`, `jq 'select(.x==1) | .users | sort_by(.name)'`), the prefix folds in and only the suffix ships to compute. jq projections are always wrapped `CAST(... AS VARCHAR)` so JSON objects/arrays come back as strings (otherwise the duckdb-go driver can't scan them, and it strips quotes from string scalars — breaking downstream jq stages).

**Partial pushdown**: A single jq expression like `select(.x == 1) | .y` is split — the select becomes a SQL predicate, `.y` becomes a compute stage. The awk equivalent: `LEVEL == "ERROR" { print $3 }` splits into a WHERE clause and a compute `awk '{ print $3 }'`.

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
    .pct → not a select → leftover compute "jq '.pct'"
    result: pushed=true, predicate=above, compute="jq '.pct'"

Projection (project.go:extractJqRawExpr):
  leftover is a single FieldExpr (.pct) → fold into SELECT
  rawExpr = CAST(json_extract(payload, '$.pct') AS VARCHAR), residual=nil
  → drops the jq stage entirely

  ShardSQL: SELECT line_num, CAST(json_extract(payload, '$.pct') AS VARCHAR) as raw FROM logs
            WHERE app = 'weft/metrics'
            AND json_extract_string(payload, '$.type') = 'memory'
            ORDER BY ts DESC LIMIT 10

  ComputeStages: []

Execution:
  coordinator injects customer WHERE, fans out to matching shards
  shard returns rows where raw = the .pct value (e.g. "57.49")
  no compute work — output goes straight to the client
```

## PostgreSQL Store

Single Postgres DB (gorm auto-migrate) holds all configuration and metadata:

**Core models:** Customer, Namespace, Account, User, MagicLink, DeviceCode, Subscription, TeamMember, NamespacePermission

**Operational:** AgentToken, ShardRecord (includes `ebs_volume_id` cache of the shard's data volume — authoritative binding is the volume's `tag:shard_id`, this column is a coordinator-side convenience), LogGroup, ShellFunction, CronWatch, CronResult, NotifyConfig, AlertIncident, SavedQuery

**Metered:** usage_hourly / usage_daily (pg_partman partitioned), ingestion_errors, shard_heartbeats (latest beat per source, pruned hourly; not partitioned)

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
- **Shards**: tiered ASGs — small (t4g.micro, 2 running), medium (t4g.small, 0), large (t4g.medium, 0). Each running shard has a dedicated gp3 EBS volume attached at `/dev/sdf`; volumes are tagged `shard_id` + `shard_tier` and outlive the instance (see Shard Lifecycle above).
- **Shard lifecycle hook**: `EC2_INSTANCE_LAUNCHING` per tier → SNS (`weft-shard-lifecycle`) → SQS. Coordinator long-polls the queue, creates/attaches volumes, signals `CompleteLifecycleAction`. Failed attaches ABANDON the pending instance; ASG retries.
- **Archive storage**: Parquet on `weft-cold-*` S3 bucket is the sole durable copy; hot volumes are detached and deleted on archive with no EBS snapshot (removed 2026-09-15 — the parquet export already covers durability, and the ongoing per-archival snapshot was an unbounded, un-cleaned-up cost leak).
- **Build**: kevcons + Packer AMIs
- **Dev**: Docker Compose (postgres, 2 shards with named volumes, coordinator, api, weftd, minio for S3, localstack for EC2/SNS/SQS integration tests)

## gRPC Services

### ShardQuery (port 9000, runs on each shard)

- `Query(QueryRequest) → stream LogRow` — pushdown SQL
- `Ingest(IngestRequest) → IngestResponse` — batch row insertion
- `IngestStream(stream LogRow) → IngestStreamResponse` — long-lived agent stream
- `Configure(ConfigureRequest) → ConfigureResponse` — blank shard initialization

### WeftCompute (port 9010, compute workers)

- `ExecuteQueryStream(ExecuteQueryRequest) → stream ExecuteQueryStreamResponse` — fan out to shards, merge, run stages, stream line batches back

### WeftCoordinator (port 9020, client-facing)

Served on two listeners. The public port (9020) serves only `Authenticate`, `IngestStream`, `SubmitSample`, `GetSourceStatus` and `Heartbeat` (`service.PublicMethods`) and refuses every other method as Unimplemented. The full service, JWT-gated, is on the coordinator's `--local-addr` (default `127.0.0.1:9025`), which the co-located api connects to. A third listener, `--internal-addr` (default `:9026`), serves only `Heartbeat` (`service.InternalMethods`) and is reachable only inside the VPC through the internal NLB; shards report there. A heartbeat from a shard with no identity yet must present the fleet `SHARD_SECRET` as its bearer.

- **Query**: `Query(CoordinatorQueryRequest) → stream CoordinatorQueryResponse`
- **Sources**: AddSource, ListSources, RemoveSource
- **Tokens**: CreateToken, ListTokens, RevokeToken
- **Shell**: SaveFunction, GetFunction, ListFunctions, DeleteFunction
- **Watches**: SaveWatch, GetWatch, ListWatches, DeleteWatch, GetWatchResults
- **Heartbeat**: `Heartbeat(ShardHeartbeatRequest) → HeartbeatResponse` — shards heartbeat `shard_id` (empty = unclaimed), `addr`, `instance_id`, `state`, `row_count`. The handler persists the heartbeat AND dispatches it to `Provisioner.ProcessHeartbeat` in a goroutine, so christen / rebind / adopt happens within seconds of the first heartbeat from a fresh shard, not on the next 5-min reconciliation tick.
- **Metering**: `GetUsage(GetUsageRequest) → GetUsageResponse`
