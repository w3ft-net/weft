# Network and Storage Topology

Weft has five process roles: the **coordinator**, the **API
server**, one or more **shard nodes**, a **compute pool**, and
the **weftd agent**. In production these run on separate EC2
instances. In local dev they run as Docker Compose services.

## The coordinator

The coordinator is the single entry point for all user traffic. It
serves a gRPC API on port 9020 and a health endpoint on port 9021.

Entry point: `cmd/coordinator/main.go`.

Responsibilities:

- Authenticates users (JWT) and agents (bearer tokens).
- Parses shell pipelines, runs the query planner, and dispatches
  pushed-down SQL to shard nodes.
- Manages shard lifecycle: launches new shards, rotates time
  windows, archives expired shards to Parquet on S3, and
  terminates instances.
- Runs the cron executor (scheduled crons).
- Hosts the CloudWatch log poller.
- Receives weftd agent streams and routes rows to the active
  ingesting shard.

The coordinator stores metadata (users, teams, tokens, crons,
functions, dashboards, shard records) in PostgreSQL via the
`store` package. The store schema lives at `store/schema.sql`.

## The API server

A separate HTTP server that exposes REST endpoints for the
frontend and external integrations. It does not run shell
pipelines or manage shards — it proxies everything to the
coordinator over a gRPC client connection.

Entry point: `cmd/api/main.go`. Listens on port 8080.

The API server maintains a `WeftCoordinatorClient` (created in
`api/handlers.go`'s `Connect()` method) pointed at the
coordinator's gRPC port. Every HTTP handler translates the
request into a coordinator RPC call. For example, `/api/shell/exec`
calls `coordClient.Execute()`, and the ingest adapter at
`/api/ingest/heroku/{token}` opens a `coordClient.IngestStream()`.

In production, a public **ALB** (HTTPS:443 → HTTP:8080) fronts
the API server. The frontend static assets are served from S3,
not through the ALB — the ALB handles only `/api/*` traffic.

In Docker Compose, the API runs as a separate service
(`ROLE: api`) on port 8080.

Key files:
- Entry point: `cmd/api/main.go`
- Route registration and gRPC proxy: `api/handlers.go`
- HTTP ingest adapter: `api/adapter.go`

## Shard nodes

Each shard node owns a time window of log data in a local DuckDB
database. Shards transition through a lifecycle:

```
LAUNCHING → INGESTING → READ_ONLY → ARCHIVED → TERMINATED
```

Only one shard per bucket is INGESTING at a time. The coordinator
is the sole lifecycle controller.

Entry point: `cmd/shard/main.go`. gRPC on port 9000, HTTP health
on port 9001.

Shards expose three core RPCs:

- `Query` — execute pushed-down SQL, stream result rows back.
- `Ingest` — accept a batch of log rows from the coordinator.
- `Archive` — export data to Parquet on S3.

Each shard heartbeats to the coordinator every 60 seconds,
reporting its state, row count, disk usage, and its own
`ip:port`. The coordinator stores each shard's address in a
per-shard registry entry (`coordinator/registry.go`) and opens a
dedicated gRPC connection to it (`shard.NewGRPCShard` in
`coordinator/provisioner.go`). At query time, the coordinator
dials each matching shard **directly** — there is no load
balancer between coordinator and shards.

The DuckDB schema includes these columns per row:

```
line_num, raw, customer, app, level, ts_unix_ns, instance,
payload_json, fields, deployment, env, version, region,
cluster, k8s_namespace, pod, container, node
```

After a shard's time window expires and retention is up, the
coordinator archives it to Parquet on S3, then transitions it to
ARCHIVED. Queries against archived data are served by a Lambda
function that reads the Parquet files directly.

Key files:
- Shard interface: `shard/shard.go`
- Cold shard (S3/Lambda): `shard/cold_shard.go`,
  `shard/lambda_cold_shard.go`
- Shard record persistence: `store/shard.go`

## Compute pool

Stateless workers that run post-pushdown pipeline stages (the part
of a user's command that couldn't be folded into SQL). The
coordinator sends them a pipeline fragment and a stream of merged
rows; they run it through the warp bash interpreter and return
the output.

Entry point: `cmd/compute/main.go`. gRPC on port 9010.

Key files:
- Interface (`Executor`) + shard fan-out: `compute/compute.go`
- Engine (merge, limits, warp stages): `compute/engine.go`
- gRPC client the coordinator uses: `compute/client.go`

## The weftd agent

A Go binary deployed on customer machines. It tails local log
files, parses them, and streams structured rows to the coordinator
over a long-lived gRPC `IngestStream`.

Entry point: `cmd/weftd/main.go`. Configured via a YAML file
specifying the coordinator endpoint, agent token, and log source
paths.

## Protocol and auth

All inter-service communication is gRPC. The protocol is defined
in `proto/weftpb/weft.proto`.

Three auth mechanisms:

| Path | Mechanism | Verified by |
|---|---|---|
| User → coordinator | JWT (Google OAuth or password) | `auth/grpc.go` interceptors |
| Shard ↔ coordinator | Shared bearer token (`SHARD_SECRET`) | `auth/grpc.go` shared-token interceptors |
| weftd → coordinator | Agent token (`wft_xxx`) | `Authenticate` RPC, resolves to customer + namespace |

Session construction: `auth/session.go` resolves a JWT into a
`Session` carrying the user's ID, active customer, and role.
Every RPC handler calls `callerCustomer(ctx)` to extract the
verified customer — the request proto's customer field is ignored.

## Multi-tenancy

Customer isolation is enforced at five layers:

1. **Session** — JWT resolves to a customer via team membership.
   `auth/session.go`.
2. **RPC boundary** — every handler extracts customer from the
   verified session, never from the request. `coordinator/service/server.go`.
3. **Planner** — every query plan injects
   `WHERE customer = '...'` unconditionally. `planner/planner.go`.
4. **Shard SQL** — the customer predicate is baked into the SQL
   before it reaches DuckDB. No shard ever receives a query
   without it.
5. **Storage schema** — every PostgreSQL table that holds
   customer-specific data (tokens, crons, functions, dashboards)
   has a `customer_id` foreign key.

`$CUSTOMER` is never exposed as a user-facing meta-variable. It
is invisible infrastructure.

## Production infrastructure

Terraform stack: `infrastructure-catalog/stacks/asg-weft-application/main.tf`.

```
              Internet / weftd agents
                    │            │
       ┌────────────▼──┐  ┌─────▼──────────┐
       │  Public NLB   │  │  Public ALB    │
       │  TCP:9020     │  │  HTTPS:443     │
       └──────┬────────┘  └──────┬─────────┘
              │                  │
       ┌──────▼──────────────────▼──────┐
       │  Coordinator ASG (1-2)        │
       │  gRPC:9020  HTTP:9021         │   ┌───────────────┐
       │                               │   │  API ASG      │
       │  (also serves as NAT gateway  │   │  HTTP:8080    │
       │   for private-subnet nodes)   │   │  proxies to   │
       └──┬──────────┬─────────────────┘   │  coord:9020   │
          │          │                     └───────────────┘
   ┌──────▼──┐  ┌────▼──────────────────┐
   │ RDS     │  │ Internal NLBs (×2)    │
   │ Postgres│  │ coordinator:9020      │
   └─────────┘  │ compute:9010          │
                └──┬─────────────┬──────┘
                   │             │
      ┌────────────┘        ┌───▼────────┐
      │  (heartbeats IN)    │ Compute    │
      │                     │ workers    │
  ┌───▼──────┐  ┌───────┐  │ ASG        │
  │ Shard    │  │ Shard │  │ gpipe      │
  │ small    │  │ med/lg│  └────────────┘
  │ ASG      │  │ ASG   │
  │ DuckDB   │  │DuckDB │
  │ +EBS     │  │+EBS   │
  └────┬─────┘  └──┬────┘
       │           │
       └─────┬─────┘
             ▼
       S3 Parquet
      (cold archives)
```

Note what the internal NLBs front:

- **Coordinator internal NLB** (TCP:9020) — gives shards and
  compute workers a stable address for heartbeats and callbacks.
  Same coordinator target as the public NLB, but reachable from
  private subnets without traversing the internet.
- **Compute internal NLB** (TCP:9010) — the coordinator and
  shards reach compute workers through this.

Shards are **not** behind any load balancer. The coordinator
dials each shard directly at the `ip:port` it received from
that shard's heartbeat (see `shard.NewGRPCShard` in
`coordinator/provisioner.go`).

Shards live in private subnets. NAT is provided by the coordinator
via iptables masquerade (not a managed NAT gateway). Each shard
gets a dedicated EBS volume that follows it through its lifecycle
and is snapshotted before termination.

## Local development

`docker-compose.yml` stands up the full stack locally:

- `postgres` — RDS replacement
- `coordinator` — spawns shard containers via Docker API
- `api` — HTTP server on port 8080, proxies to coordinator
- `compute` — gpipe worker
- `minio` — S3 replacement for Parquet archives
- `weftd` — sample agent instances
- `lambda-shard` — dev replacement for the cold-shard Lambda

Dev defaults: 1-hour shard windows, 2-hour retention (2 shards at
steady state). The coordinator uses `DockerLauncher` instead of
`ASGLauncher` to manage shard containers.

---

## Challenge: trace a query from CLI to shard and back

**Goal:** add a request-tracing header that lets you correlate a
user's shell command with the shard queries it generates.

### Setup

Read these files first:

- `coordinator/coordinator.go` — find the `runQuery` and
  `executeQuery` methods. Understand how the coordinator selects
  shards, builds SQL, and fans out gRPC calls.
- `planner/planner.go` — read the `Plan` function. Understand
  what `ShardSQL` contains and how `ComputeStages` are separated.
- `shard/service/server.go` — read the `Query` handler. See how
  it receives SQL and streams rows back.
- `proto/weftpb/weft.proto` — look at the `QueryRequest` and
  `ResultRow` messages.

### Task

1. Add a `trace_id` field to the `QueryRequest` proto message.
   Generate a random hex string in the coordinator when it builds
   the request (use `crypto/rand`).

2. In the shard's `Query` handler, log the `trace_id` along with
   the SQL being executed and the number of rows returned.

3. In the coordinator's `executeQuery`, log the same `trace_id`
   with the list of shard addresses the query was fanned out to,
   and the total row count after merging.

4. Regenerate the proto (`protoc`) and verify the system compiles.

### What you'll learn

- How a pipeline becomes SQL and how that SQL reaches DuckDB.
- The fan-out/merge pattern the coordinator uses for multi-shard
  queries.
- How gRPC metadata flows between coordinator and shard.
- The proto → generated code → service handler loop.
