# Plan: File-sourced dimensions + the `$NODE` column

Extends [`ingest-dimensions.md`](ingest-dimensions.md) (✅ Shipped). Two
related additions:

1. **Generalized file source** — any reserved dimension column can be
   populated from `path[:.json.path]`, watched live via fsnotify.
   Generalizes the `version_file` mechanism in `weftd/version.go` so
   the same trick works for `deployment`, `region`, `cluster`, and the
   new `node` column.
2. **`$NODE` column** — a new reserved dimension for stable-across-
   rotation *workload* identity, distinct from `$HOST` (machine) and
   `$DEPLOYMENT` (group). Only ever populated from a file source —
   never auto-derived — so it stays under operator control.

## Why

`ingest-dimensions.md` shipped with seven population mechanisms (token
mint, agent config, cloud metadata, k8s downward API, k8s log-path
parse, fsnotify-watched `version_file`, schema extraction). Each is
hardcoded to one column. The audit that motivated this plan found a
class of bug those mechanisms can't fix:

- Three weft shard hosts (`172-31-128-17`, `-33`, `-35` per the
  prod provisioner log) were ingesting their `/opt/app/data` disk
  metrics under `app=weft/metrics` instead of the expected
  `app=weft/shard_metrics`. They were christened as shards mid-run
  (the provisioner moved live shards onto them at `14:51:09` and
  `14:57:17` on 2026-04-28); their weftd had read its
  role-bearing config at startup and never refreshed.
- Recovery requires either a weftd restart on every shard rotation
  (operator toil) or an out-of-band SIGHUP path.

The existing `version_file` + fsnotify pattern already proves the
fix shape — read a file on disk, watch it for changes, stamp the
current value on every emitted row. Generalizing it costs roughly
nothing in code and unblocks every "this dimension changes during
the agent's lifetime" case.

The audit also surfaced a separate gap: **none of the existing
columns carries a stable workload identity that survives instance
replacement**. `$INSTANCE` is `os.Hostname()` (IP-derived on EC2,
churns); `$DEPLOYMENT` is a group label (many hosts, one value). For
shards specifically, the durable identity is the shard ID
(`shard-20260427T0000-65c0edef`) — written to a JSON file on the EBS
volume that follows the shard across host moves. There is no column
to stamp it on. `$NODE` is that column.

## The reserved set, amended

Adds one row to the existing table in `ingest-dimensions.md`:

| Meta-var (awk) | Meta-var (jq) | Column | Source |
|---|---|---|---|
| `$NODE` | `._node` | new `node` | Agent (file-sourced; never auto-derived) |

The choice of `node` over `shard`, `workload_id`, `replica`, or
`instance_id`:

- `shard` is a weft-internal concept; the column needs to be
  generic enough for k8s StatefulSet pods, EBS-backed services,
  custom-orchestrated workloads.
- `workload_id` reads as jargony; `instance_id` collides with
  `$INSTANCE` (which is the machine identity).
- `replica` overloads with k8s `ReplicaSet` and similar.
- `node` is short, generic, and not yet claimed by another weft
  concept (k8s "node" — i.e., the worker VM — is usually called the
  `host` in observability vocabulary, and that's already `$HOST`).

## File-source spec

Config syntax in weftd, under a new top-level `dimensions:` map:

```yaml
dimensions:
  deployment: /etc/weft/role
  node: /etc/weft/identity.json:.shard_id
  version: /opt/api/VERSION
  region: /etc/weft/region:.region
```

Each value is `path` or `path:.json.path`. Rules:

- **Plain path** — read the file's contents, trim, use as the value.
  Same semantics as today's `version_file`.
- **`path:.json.path`** — read the file, parse as JSON, evaluate the
  dot-path against the parsed value, stringify the leaf. Path syntax
  is a small subset of jq: bare field access (`.foo`), nested fields
  (`.foo.bar.baz`), array indexing (`.items[0].name`). No filters,
  pipes, alternatives, or computed indices — if you need more,
  shell-process the file separately.
- **Missing file or unresolvable path** — column stamps NULL; rows
  flow through normally; queries on the column don't filter them.
  Logged once at the appropriate level, not per-row.
- **Watched live** — the value is read at startup and on any
  `Create|Write|Rename|Remove` fsnotify event on the file's directory
  (the same dir-watch trick `VersionReader` already uses to handle
  rename-into-place editors). Subsequent rows pick up the new value
  on the next emission, no agent restart.

### Allowed columns

Any of: `deployment`, `version`, `region`, `cluster`, `node`.

**Not** overridable from file source:

- `env` — coordinator-stamped from the token; the token is the source
  of truth for compliance reasons.
- `app`, `instance` — source-identity guarantees would break if these
  could mutate mid-stream. `instance` stays `os.Hostname()`; `app`
  stays per-source filename-derived.
- `level`, `ts` — schema-extracted from the row itself.
- `k8s_namespace`, `pod`, `container` — populated from the kubelet
  log-path layout, which is itself stable for the container's
  lifetime; overriding from a separate file invites drift.

A `dimensions:` entry naming any disallowed column is a
config-validation error at weftd startup (pre-launch, no compatibility
shim per the project's no-legacy-support stance).

### Resolution order

If both `dimensions:` and a column-specific mechanism are configured
for the same column, **`dimensions:` wins**. Concretely:

- `dimensions.region: ...` overrides the cloud-metadata probe.
- `dimensions.version: ...` overrides any `version_file:` set on a
  source (`version_file:` becomes a backwards-compatible alias for
  `dimensions.version:` after this lands).
- `dimensions.deployment: ...` overrides the agent's static
  `deployment:` key and any k8s/ASG auto-fill.

Order makes operator override the trump card and keeps the
auto-derived sources as fallbacks.

## Architecture

Reuse and generalize `weftd/version.go`'s `VersionReader`.

```
weftd/file_value.go (new)
  type FileValueReader struct { current atomic.Pointer[string]; ... }
  func NewFileValueReader(ctx, spec FileValueSpec) *FileValueReader
  func (*FileValueReader) Current() string

weftd/file_value_spec.go (new)
  type FileValueSpec struct {
    Path     string   // /etc/weft/identity.json
    JSONPath []string // [shard_id]   — empty = whole file (trimmed)
  }
  func ParseFileValueSpec(s string) (FileValueSpec, error)
  // accepts "/path", "/path:.foo", "/path:.foo.bar", "/path:.items[0].name"

weftd/version.go
  // becomes a thin wrapper that constructs a FileValueReader with
  // an empty JSONPath. Existing callers keep working unchanged.

cmd/weftd/main.go
  // for each entry in cfg.Dimensions, spin up a FileValueReader
  // and stamp the value onto each LogRow.<Column> at emit time
  // alongside the existing per-column code paths
```

The dot-path evaluator is ~30 lines: tokenize on `.` and `[…]`,
walk a `map[string]any` / `[]any` tree, return the stringified leaf
or `("", false)` if the path doesn't resolve.

## Components to build

1. **`weftd/file_value.go`** — extract from `version.go`. Generic
   reader with optional JSON-path extraction. Move the existing
   fsnotify dir-watch trick into the new file.
2. **`weftd/file_value_spec.go`** — parser for `path[:.json.path]`
   spec, dot-path evaluator. Tests covering bare fields, nested
   fields, array indices, missing fields, malformed JSON.
3. **`weftd/version.go`** — re-implement on top of `FileValueReader`.
   Public API (`NewVersionReader`, `Current`) unchanged.
4. **`weftd/config.go`** — new `Dimensions map[string]string`. Parsed
   alongside existing config. Validation: each key must be in the
   allowed-columns set, each value must parse as a `FileValueSpec`.
5. **Proto** — `LogRow` gains `Node string` (next available field
   number after `ingest-dimensions.md`'s additions).
6. **Schema migration** — `ALTER TABLE logs ADD COLUMN node VARCHAR;`,
   one Configure RPC pushed to every shard. Idempotent.
7. **Coordinator stamp paths** — gRPC ingest, HTTP ingest, CloudWatch
   poller. Each accepts the agent-supplied `node` field; coordinator
   doesn't auto-stamp.
8. **Planner meta-var table** — append `$NODE` / `._node` to
   `metaVars` (`planner/awk.go:19`) and `jqMetaVars`
   (`planner/jq.go:11`).
9. **`docs/manual/dimensions.md`** — add a section on `dimensions:`
   config and the `$NODE` column. Include a worked example for the
   shard-rotation case.
10. **Internal: weft prod** — drop `/etc/weft/identity.json` onto
    every shard host as part of the existing
    christen → start-shard sequence in the provisioner. weftd config
    on shard hosts gets `dimensions: { node: /etc/weft/identity.json:.shard_id, deployment: /etc/weft/identity.json:.role }`.
    First user of the feature; validates the shape end-to-end.

## Estimated effort

- `FileValueReader` extraction + JSON-path parser/evaluator: **0.75 day**
- Config plumbing + validation: **0.5 day**
- Proto + schema migration + Configure RPC: **0.5 day**
- Planner meta-var entries + tests: **0.25 day**
- Docs amendment: **0.5 day**
- Tests (file-source live update, JSON-path edge cases, conflict
  resolution, NULL behavior): **0.75 day**
- Internal weft prod rollout (christen step + weftd config update): **0.25 day**

**Total: ~3.5 days.**

## Non-goals

- **No full jq.** Path syntax is field/index access only. If a
  customer needs `select(...) | first`, they pre-process the file
  with their own tooling and point weftd at the result.
- **No coordinator enforcement of `$NODE` uniqueness.** Two agents
  configured with the same `node` value will both stamp it; we don't
  reject. Operators choose what to put there.
- **No `$NODE` history.** If a host's `node` value changes mid-run,
  old rows keep their old stamp; new rows get the new one. No
  retroactive update.
- **No `dimensions:` for `app`, `instance`, `env`, `level`, `ts`,
  `k8s_namespace`, `pod`, `container`.** Listed disallowed above.
- **No backward-compat shim for `version_file:`.** Per
  `ingest-dimensions.md`'s convention, both spellings work for one
  release, then `version_file:` is removed.
- **No reload semantics for the `dimensions:` map itself.** Adding or
  removing entries requires an agent restart; only the *values*
  pointed at by an entry are live-watched.
