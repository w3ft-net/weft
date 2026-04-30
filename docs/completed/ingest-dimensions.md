# Plan: Configurable ingest dimensions

Add a fixed set of reserved observability dimensions as real columns on every shard. Six new columns — `env`, `version`, `region`, `cluster`, `k8s_namespace`, `pod`, `container` — sourced from token, agent config, cloud metadata, or the k8s log-path layout. The existing `host_group` column gets renamed to `deployment` (matching OTel's `k8s.deployment.name`) and becomes a first-class meta-var; ASG-based deployments reuse the same column, one ASG per deployment. Two existing columns (`app`, `instance`) gain alias meta-vars (`$SERVICE`, `$HOST`) so the vocabulary aligns with industry conventions without a data migration.

Custom (user-defined) dimensions are explicitly out of v1 scope. Customers needing a non-reserved dimension put it in the JSON payload and `jq` it like any other field; pushdown still works via the existing `json_extract` path, just slower than a real column.

## Why

Today the only fast-path dimensions are `$LEVEL`, `$APP`, `$INSTANCE`, `$TS` — four real columns the planner rewrites to direct `WHERE` clauses. Anything else lives in payload JSON; pushdown works via `json_extract_string`, but order-of-magnitude slower because there are no row-group statistics on JSON keys.

For dimensions customers filter on constantly — env, pod, version, host — the slow path is the wrong default. A fixed reserved set keeps the schema small enough to manage while covering the names that show up across every observability standard (OpenTelemetry semconv, Datadog unified service tagging, Elastic Common Schema, k8s recommended labels). Custom names remain functional via the existing JSON-extract route; they just don't get the column treatment.

## The reserved set

| Meta-var (awk) | Meta-var (jq) | Column | Source |
|---|---|---|---|
| `$APP` (alias `$SERVICE`) | `._app` (alias `._service`) | existing `app` | Log filename / source declaration |
| `$HOST` (alias `$INSTANCE`) | `._host` (alias `._instance`) | existing `instance` | Agent `os.Hostname()` |
| `$LEVEL` | `._level` | existing `level` | Schema-extracted |
| `$TS` | `._ts` | existing `ts` | Schema-extracted |
| `$DEPLOYMENT` | `._deployment` | existing `host_group` (renamed to `deployment`) | Agent config (or k8s/ASG via OTel mapping) |
| `$ENV` | `._env` | new `env` | Token-derived (coordinator-stamped) |
| `$VERSION` | `._version` | new `version` | Agent (`version_file`, fsnotify-watched) |
| `$REGION` | `._region` | new `region` | Agent (cloud metadata at startup) |
| `$CLUSTER` | `._cluster` | new `cluster` | Agent (k8s downward API) |
| `$K8S_NAMESPACE` | `._k8s_namespace` | new `k8s_namespace` | Agent (k8s log-path parse) |
| `$POD` | `._pod` | new `pod` | Agent (k8s log-path parse) |
| `$CONTAINER` | `._container` | new `container` | Agent (k8s log-path parse) |

Six new columns plus one rename (`host_group` → `deployment`). No registry. Entries appended to `metaVars` in `planner/awk.go:19` and `jqMetaVars` in `planner/jq.go:11`. Plus alias entries — `SERVICE` → `app` and `HOST` → `instance` — pointing two meta-var keys at one column each.

## Naming convention

Awk meta-vars stay all-caps short flat names — `$ENV`, `$POD` — matching today's `$LEVEL` / `$APP` / `$INSTANCE` shape.

Jq meta-vars use leading-underscore lowercase — `._env`, `._pod` — matching today's `._app` / `._level`. The underscore disambiguates from real JSON field names (`.pod` is a payload key; `._pod` is the column). jq is case-sensitive, but the underscore convention is more reliable than relying on case alone — real JSONL sometimes contains `Pod` or `PoD` style keys.

The one verbose name is `k8s_namespace`. The shorter `namespace` is unavailable because weft already uses "namespace" for the customer's logical token-grouping that's part of the path (`/log/weft/...`). Both meta-var spellings: `$K8S_NAMESPACE` / `._k8s_namespace`.

OpenTelemetry-canonical dotted forms (`service.name`, `deployment.environment.name`, `k8s.pod.name`) are not usable as identifiers in awk or SQL; a normalization layer at HTTP ingest maps the standard OTel attributes onto weft column names so customers shipping from an OTel collector land in the right columns automatically.

## Per-dimension provenance

### deployment — agent config (renamed from host_group)

Per-agent global value, set in weftd config:

```yaml
deployment: api          # was: host_group: api
```

Stamped on every row from this agent. The column is renamed `host_group` → `deployment` in DuckDB (`ALTER TABLE logs RENAME COLUMN`); the proto field is renamed correspondingly. weftd accepts the new `deployment:` key and rejects `host_group:` with a config-validation error pointing at the new key — pre-launch, no compatibility shim per the project's no-legacy-support stance.

Conceptually maps to k8s Deployment (the OTel canonical) and reuses the same column for AWS ASG, GCP Managed Instance Groups, Azure VM Scale Sets, and any other "fleet of identical hosts" infrastructure. K8s mode auto-fills it from the most useful k8s ancestor (Deployment → DaemonSet → StatefulSet, in that order); ASG mode reads from the EC2 instance tag at startup if `deployment:` isn't explicitly set in config.

### env — token-derived

Token mint accepts an env value. The coordinator looks it up by token on each ingest and stamps the row before drain. weftd doesn't declare env locally; agent-side override has no effect — the token is the source of truth.

```
weft token create --namespace myco --env prod
```

A token without env stamps NULL. Queries on `$ENV` simply don't filter such rows in or out. Used in path layout (next section).

### version — agent file + fsnotify (with env-var fallback)

Per-source agent config nests both file and env under a single `version:` key:

```yaml
sources:
  - app: api
    path: /var/log/api.log
    version:
      file: /opt/api/VERSION   # live: fsnotify-watched, refreshed on change
      # env: APP_VERSION       # snapshot: read once at startup, no live refresh
```

`version.file` is the primary path: weftd reads it at startup, watches for changes via fsnotify, stamps `version` on every row from that source. The deploy system writes the file as part of a release; subsequent rows carry the new version without an agent restart.

`version.env` is a fallback for environments that prefer env vars — read once at startup via `os.Getenv`, no live refresh (a running process can't see env-var changes anyway). When both are set, `version.file` wins. `version.command:` is deliberately omitted — too easy to misuse.

### region — cloud-metadata probe

On startup, weftd probes the standard cloud metadata endpoints in order:

1. AWS IMDSv2 (`http://169.254.169.254/latest/meta-data/placement/region`)
2. GCP metadata (`http://metadata.google.internal/computeMetadata/v1/instance/zone`, region derived from zone)
3. Azure IMDS (`http://169.254.169.254/metadata/instance/compute/location?api-version=2021-02-01`)

First successful response sets `region` for the agent's lifetime; failures fall through to the next probe. None reachable → `region` stays empty and queries on `$REGION` don't filter. Customers running in environments without metadata (bare metal, lab) can override with `region:` in weftd config.

### cluster, k8s_namespace, pod, container — k8s mode

Populated only when weftd runs in its k8s DaemonSet mode (see `docs/plans/weftd-k8s.md`):

- `cluster` from kubelet downward API or a static value baked into the Helm chart.
- `k8s_namespace`, `pod`, `container` parsed from the log file path. The kubelet writes logs at `/var/log/pods/<namespace>_<pod>_<uid>/<container>/N.log` — that layout is stable across container runtimes.

Outside k8s mode, all four columns stamp NULL.

## Path layout

Existing layout: `/log/<namespace>/<app>.log` — e.g., `/log/weft/coordinator.log`.

When env is set on the row's token: `/log/<namespace>/<env>/<app>.log` — e.g., `/log/weft/prod/coordinator.log`.

A namespace can hold a mix. Tokens without env continue producing rows that resolve to the 2-level layout; tokens with env produce rows that resolve to the 3-level layout. The customer-visible filesystem reflects both:

```
weft> ls /log/weft
prod/                  ← from env-bearing tokens
staging/               ← also env-bearing
coordinator.log        ← from a legacy token without env
```

Tab completion and globbing handle the mix. `cat /log/weft/coordinator.log` still works; `cat /log/weft/prod/coordinator.log` also works; `cat /log/weft/*/coordinator.log` matches across every env that has one. No backfill, no synthetic placeholder env, no forced migration.

If a customer wants the cleaner 3-level layout for everything in a namespace, they rotate their old tokens out for new env-bearing ones — the legacy paths drain naturally as agents reauth.

### Path resolution with metrics munging

The env layer is orthogonal to the per-deployment metrics-app naming — both compose cleanly:

| env | deployment | source | path |
|---|---|---|---|
| unset | unset | metrics | `/log/weft/metrics.log` |
| unset | `coordinator` | metrics | `/log/weft/coordinator_metrics.log` |
| `prod` | unset | metrics | `/log/weft/prod/metrics.log` |
| `prod` | `coordinator` | metrics | `/log/weft/prod/coordinator_metrics.log` |
| `prod` | any | normal source `api` | `/log/weft/prod/api.log` |

The env path-segment slots in between namespace and the app leaf regardless of whether the leaf has been munged (metrics) or comes from a filename / unit name (everything else).

## host_group → deployment rename

`host_group` did two things in the current code: stamped a per-row column (today write-only — no meta-var, no pushdown), and synthesized the metrics-row app name (`<namespace>/<host_group>_metrics`). The redesign keeps the column (renamed) and drops the metrics-app munging:

- Rename `LogRow.HostGroup` → `LogRow.Deployment` in the proto. Field number 10 is reused so wire-format-compatible deserializers ignore the rename.
- Rename DuckDB column: `ALTER TABLE logs RENAME COLUMN host_group TO deployment;`
- Rename weftd config key: `host_group:` → `deployment:`. Pre-launch, no alias — bad keys fail config validation with a clear pointer at the new name.
- Rename `LogGroup.HostGroup` provenance field in `store/source.go:74`.
- The `<namespace>/<host_group>_metrics` app-name munging in `cmd/weftd/metrics.go:65-68` is **kept** — only the variable name changes (`globalHostGroup` → `globalDeployment`). Per-deployment metrics pseudofiles (`/log/weft/coordinator_metrics.log`, `/log/weft/shard_metrics.log`) continue to exist as today; they're the discoverable, tab-completable surface for "show me this deployment's metrics." The new `$DEPLOYMENT` meta-var is additive — it lets cross-cutting queries against non-metrics sources (or against a specific metrics pseudofile when you want a sub-filter) reach the same value, without removing the per-deployment file split.

The metrics case is special because metrics is the only built-in source where the app name is constant across hosts (literally `metrics`). File and journal tailers get per-source uniqueness from the filename / unit name; CloudWatch from the log group; HTTP ingest from the client-supplied app field. Metrics-and-only-metrics needs deployment baked into the app name to avoid collisions, and that's why the munging stays.

## Components to build

1. **Schema migration** — `ALTER TABLE logs ADD COLUMN env VARCHAR;` × 6 new columns plus `ALTER TABLE logs RENAME COLUMN host_group TO deployment;`. One Configure RPC pushed to every shard. Idempotent; existing rows carry NULL in the new columns; the rename preserves all existing data.
2. **Proto changes** — `LogRow` gains `Env`, `Version`, `Region`, `Cluster`, `K8sNamespace`, `Pod`, `Container` string fields. `HostGroup` renamed to `Deployment` (same field number).
3. **Coordinator stamping** — token lookup at ingest receipt sets `env`. The other dimensions arrive pre-stamped from the agent.
4. **weftd version reader** — `version_file` + fsnotify in the per-source goroutine; cached, refreshed on file change.
5. **weftd region probe** — startup-only cloud-metadata client (3 vendors), sets a global; config override if needed.
6. **Planner table extensions** — append entries to `metaVars` and `jqMetaVars`. Two new alias entries (`service` → `app`, `host` → `instance`).
7. **shellfs path layout** — `/log/...` resolver handles 2-or-3-level under each namespace based on whether the underlying row has `env` stamped. `ls` lists either subdirectories or `.log` files at each level.
8. **Token-mint UX** — `weft token create --env prod` (CLI) + Account UI field. Optional; tokens without env keep working.
9. **metavars** — rewrite `docs/manual/weftd.md` to remove `host_group`, add `version_file` and `region:` overrides. New `docs/manual/metavars.md` with the meta-var table (awk + jq spellings), the path layout, and the per-host avg recipe.

## Pushdown semantics

For each reserved dimension the planner rewrite is a direct column predicate, identical in shape to today's `$LEVEL`:

```
awk '$ENV == "prod"'                  → WHERE env = 'prod'
jq 'select(._env == "prod")'          → WHERE env = 'prod'
awk '$POD ~ "api-7f9b"'               → WHERE pod LIKE 'api-7f9b%'
```

Row-group min/max statistics make these queries fast even on multi-GB shards.

For non-reserved fields, the existing JSON-extract pushdown is unchanged:

```
jq 'select(.feature_flag == "abc")'   → WHERE json_extract_string(payload, '$.feature_flag') = 'abc'
```

Mixing both in one pipeline becomes a single `WHERE` with a column predicate (fast) AND a JSON-extract predicate (slower) — DuckDB plans it as one stage.

## Aggregate-by-dimension recipe

The original motivation: "avg memory pct by host over 1h". With `$HOST` as a real column, the watch becomes a one-liner:

```
# /cron.d/memory-per-host
# schedule: every 1m
tail -n 200 /log/<ns>/metrics.log \
  | jq -r 'select(._level == "INFO" and .type == "memory") | "\(._host) \(.pct)"'
```

That emits one row per host per minute. The watch's schedule is the bucket size; multi-row alerts (`alert above 80 -k 2 -t 1`) page per host independently. Window aggregates over the full per-host history fall under the `weft.avg_by` follow-up flagged in `docs/plans/composite-alerts.md`'s open-design space.

## Estimated effort

- Schema migration + Configure RPC update: **0.5 day**
- Proto + LogRow code generation + downstream: **0.5 day**
- Token mint CLI/UI + coordinator env stamping: **1 day**
- weftd `version_file` + fsnotify: **0.5 day**
- weftd region probe (three cloud variants + tests): **0.75 day**
- Planner meta-var table extensions + alias support: **0.5 day**
- shellfs 2-or-3-level path layout: **1 day**
- `host_group` → `deployment` rename across the codebase (proto, store, weftd config, shard schema, planner table): **0.75 day**
- Metavars docs (weftd + new metavars page + path layout): **1 day**
- Tests (per dimension, alias resolution, mixed-namespace path resolution): **1 day**

**Total: ~7.5 days** standalone. The four k8s-derived columns (`cluster`, `k8s_namespace`, `pod`, `container`) are defined here but populated by `docs/plans/weftd-k8s.md`; outside k8s mode they stamp NULL.

## Non-goals

- **No custom dimensions in v1.** Reserved set only. Customers needing a custom name put the value in the JSON payload and use the existing `jq` route. Add a generalized custom-dimension mechanism only when a customer asks for it.
- **No env auto-detection.** weftd never tries to guess env from the hostname or env vars. Env comes from the token, full stop.
- **No region auto-correction.** If cloud metadata returns one region and the customer overrides with another in config, the override wins — we don't second-guess.
- **No version inference from log content.** Version comes from the file/env config, never from parsing the log line itself.
- **No region in the path.** Region is a query-time filter only. No `/log/<ns>/<region>/...` layouts.
- **No backfill of env onto historical rows.** Old rows stamp NULL in `env` permanently; mixed-namespace path layouts are the deliberate consequence.
