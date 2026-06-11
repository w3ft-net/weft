# Plan: App heartbeats — wire protocol

> **Superseded (2026-06).** The dedicated heartbeat channel described
> below — a weftd HTTP listener on `/var/run/weftd-heartbeat.sock`,
> an in-memory last-seen table, and a 5s `age_s` snapshot ticker — has
> been **removed**. Heartbeats are now ordinary records: the Go SDK
> (`weft.Client.Heartbeat`) emits `{"type":"heartbeat","service",…,"ts"}`
> to app `<service>/heartbeats` over the **same records transport** as
> every other event, landing at the same `/log/<ns>/<service>/heartbeats.log`
> pseudofile. There is no second socket and no server-side `age_s`
> densification; liveness is judged by **recency/absence** of heartbeat
> rows (e.g. the `weft::is_absent` watch predicate, or a windowed
> `count`). This collapses the capture hop onto the one data channel —
> the mainstream design (Datadog DogStatsD service checks, OTLP,
> Fluentd all multiplex liveness over their data channel; Prometheus
> infers `up` from scrape recency). The historical plan is kept below
> for the record. See `docs/manual/weftd.md` and the weft-go README for
> current usage.

A small HTTP listener inside weftd that accepts empty POSTs at
`http://127.0.0.1:<port>/h/<service>` (and the same handler bound
to a Unix socket for shared-netns environments). weftd remembers
when each service last checked in and emits the table as a periodic
snapshot to a new pseudofile (`<namespace>/heartbeats.log`), so
absence-of-heartbeat becomes a regular weft query rather than a
special primitive.

This plan defines the **wire protocol** and the weftd-side state.
The user-facing entry point is the Go SDK
([`go-sdk.md`](go-sdk.md)) and Python SDK
([`python-sdk.md`](python-sdk.md)) — both call this protocol under
the hood. Raw `curl` / `nc` / cron lines remain valid for languages
we don't ship an SDK for, but they're the fallback path, not the
recommended one.

Ships before the SDKs so the SDKs have a target to hit.

## Why

Two separate failures the audit surfaced that this fixes:

1. **Coordinator dead but host alive.** Today the coordinator emits
   no periodic "alive" beacon. The first line in `coordinator.log`
   on 2026-04-28 is mid-stream provisioner work; the only restart
   evidence is the cluster of `ingest-stream: first row from …`
   lines at 14:46:33-39. A coordinator that wedges (deadlock,
   stuck under heavy load, hung on a socket) is indistinguishable
   from one idling.
2. **Shard process zombie.** Shard `e7deaf23` was reaped by the
   coordinator at 14:47:24; its binary is still emitting
   `Unauthenticated` heartbeat errors at 20:55+, six hours later.
   We have no first-class signal that says "this shard's binary
   is or isn't alive on host X" — it can only be inferred from
   the absence of error spam, which is not actionable.

Both failures share a shape: the absence of a positive "I'm alive"
signal forces operators to infer liveness from secondary symptoms.
A heartbeat channel is the canonical fix.

Why this is its own plan rather than a section of the SDK plans:
the wire protocol is the source of truth, and at least three things
talk to it. The Go and Python SDKs are the two we'll ship first;
beyond those, customer apps in languages we'll never SDK
(Ruby, Rust, shell scripts in cron, lambdas, k8s liveness probes)
need a concrete URL and method to point at. Defining the protocol
in one place — and shipping it ahead of either SDK — keeps every
caller honest and lets the SDKs land in parallel.

## Architecture

### The endpoint

weftd grows an HTTP handler exposed over **two listeners**:

1. **Unix domain socket** at `/var/run/weftd-heartbeat.sock`
   (default; configurable). Always on. The canonical path —
   filesystem permissions are the auth, containers can't reach
   it unless the socket is mounted into them, which is the same
   pattern Datadog Agent and Prometheus node-exporter use.
2. **Localhost TCP** at `127.0.0.1:14001` (default; configurable).
   **Opt-in** — `heartbeat_port:` defaults to 0 (off). Operators
   in trusted single-tenant environments enable it for friction-
   free dev and shell-cron use. Operators in shared-netns or
   multi-tenant container environments leave it off.

Same handler behind both. One endpoint shape:

```
POST /h/<service>
  → 204 No Content
```

Where `<service>` is a URL-path-safe identifier (`[a-zA-Z0-9._-]+`,
1-128 chars; longer or weirder names → 400). Body is ignored. Query
string ignored. Empty body is the expected case.

Concrete callers (in approximate order of preference):

```bash
# SDKs — recommended path. See go-sdk.md / python-sdk.md.
weft.HeartbeatLoop(ctx, "my-service", 30*time.Second)   # Go
weft.heartbeat_loop("my-service", interval=30)          # Python

# Direct over the Unix socket — works without an SDK.
curl --unix-socket /var/run/weftd-heartbeat.sock \
     -fsS -X POST http://localhost/h/my-cron-job >/dev/null

# Direct over TCP, when the optional listener is enabled.
curl -fsS -X POST http://127.0.0.1:14001/h/my-cron-job >/dev/null

# Cron line, TCP listener enabled.
* * * * * curl -fsS -X POST http://localhost:14001/h/nightly-rollup >/dev/null 2>&1
```

Anything that can speak HTTP works. systemd timers, k8s liveness
probes (`exec` style with `curl --unix-socket` against a mounted
weftd socket), Ruby `Net::HTTP`, Rust `reqwest`, shell `nc`
invocations — all valid.

### The state

In weftd memory: `map[string]time.Time` with a `sync.Mutex`. Last-
write-wins. A service heartbeating 1000 times a second is fine —
each write is one mutex acquisition, no I/O, snapshot cadence
bounds the upstream cost.

A bounded LRU keeps the table from growing without limit if a
malicious or buggy caller invents service names per request:
`max_services: 1024` (configurable). When the cap is hit, the
oldest entry is evicted on the next write, with a single warning
log per minute that names the evicted service.

### The snapshot

Every 5 seconds (configurable), weftd walks the map and emits one
JSON record per service to a new pseudo-source. Records:

```json
{"type": "heartbeat", "service": "my-cron-job", "last_seen": "2026-04-28T20:55:14.831Z", "age_s": 0.42}
{"type": "heartbeat", "service": "coordinator",  "last_seen": "2026-04-28T20:55:11.002Z", "age_s": 4.23}
```

The records flow through the same gRPC `IngestStream` weftd already
uses for OS metrics. Stamped with `app=<namespace>/heartbeats`
(matching the existing `<namespace>/<deployment>_metrics` shape),
so the queryable surface is `/log/<namespace>/heartbeats.log`.

`age_s` is included pre-computed so absence queries don't have to
do their own time arithmetic — `select(.age_s > 60)` is the natural
syntax.

Snapshot cadence is the operator's tradeoff between freshness and
volume. Defaults: 5s emit, 30s "considered dead" threshold. A
service that goes silent shows `age_s` climbing, then disappears
from snapshots after 60s of silence (so absence-of-record is also
a valid signal).

### Why a snapshot, not direct alerts

weftd-as-process is in the data plane, not the alerting plane.
Putting alert decisions inside weftd would force every operator's
liveness threshold into agent config and reload-on-change machinery.
Emitting a snapshot stream and letting operators write watches
against it is the same pattern as everything else weft does —
data flows in, watches alert.

The cost is one record per service per 5s. At 100 services per
host that's 1200 records/minute — a rounding error against the
existing metrics stream.

## Path layout

`/log/<namespace>/heartbeats.log` (or `/log/<namespace>/<env>/heartbeats.log`
when env-bearing tokens are used, per the existing
`ingest-dimensions.md` 3-level path layout).

The leaf name `heartbeats` follows the metrics pseudofile
convention: discoverable by tab completion under the namespace,
`cat`able like any other source.

## Failure modes

| Failure | Behavior | Operator visibility |
|---|---|---|
| App stops heartbeating | `age_s` climbs in snapshot until the service drops out of the table at the configured staleness limit. | Watch fires when `age_s > threshold`. |
| weftd dies | No more snapshots emitted; the heartbeats.log stream goes silent for ALL services on that host. | Distinguishable from individual app death because everything goes silent at once. A separate watch on weftd liveness handles this layer. |
| Heartbeat HTTP listener wedges but weftd is otherwise alive | Apps' POSTs hang or 5xx; their heartbeats stop landing. Snapshot still emits with stale `age_s`. | Watch fires per-service. weftd self-monitoring (separate concern, see open question) catches the listener-specific case. |
| Wrong port / endpoint typo in the cron line | Operator sees the service never appear in heartbeats.log. | Discoverable: `grep my-service /log/<ns>/heartbeats.log` returns empty. |
| Service name typo (one cron uses `nightly-roll-up`, another `nightly-rollup`) | Both names appear in the table; operator sees the duplicate. | Discoverable: `cat heartbeats.log \| jq .service \| sort -u`. |

The "weftd dies" case deserves its own answer — see Open questions.

## Components to build

1. **`weftd/heartbeat.go`** — new file:
   - HTTP listener on the configured port, single `POST /h/<service>`
     handler.
   - Service-name validation (`[a-zA-Z0-9._-]+`, 1-128 chars).
   - Bounded `map[string]time.Time` with LRU eviction.
   - Snapshot ticker that walks the map and emits records.
2. **`cmd/weftd/main.go`** — wire up: read `heartbeat_port` from
   config, start the listener and snapshot ticker if non-zero.
3. **`weftd/config.go`** — new fields:
   ```yaml
   heartbeat:
     socket: /var/run/weftd-heartbeat.sock   # default; "" disables
     port: 0                                 # default; 0 disables, 14001 conventional
     snapshot_interval: 5s                   # how often to emit
     max_services: 1024                      # LRU cap
     stale_after: 60s                        # drop services from table after this much silence
   ```
   The Unix socket is the canonical channel; TCP is opt-in. Both
   share the same handler internally.
4. **`docs/manual/heartbeats.md`** — curl recipe, cron recipe, the
   record schema, the natural watch shape (`select(.age_s > 60)`),
   the "weftd dies" caveat with a pointer to the cross-host watch.
5. **First user: weft prod.** Add a heartbeat call to:
   - Coordinator: every 5s on a goroutine, `service=coordinator`.
   - Each shard: every 5s, `service=shard-<shard_id>` so per-shard
     liveness becomes queryable. Pairs naturally with the
     forthcoming `$NODE` column when `dimensions-from-files.md`
     ships.
   - lambda-shard, compute: same.
   These are stdlib `net/http` POSTs in the interim; once
   `go-sdk.md` lands they switch to `wc.HeartbeatLoop(ctx,
   "coordinator", 30*time.Second)`. The wire-protocol-only
   version is intentionally simple because we don't want to
   block on the SDK landing — same shape, fewer features.
6. **Bucket-3 watches** that depend on this:
   - Per-service liveness alert: `select(.age_s > 60)` against
     `heartbeats.log`.
   - The C4 zombie alert (currently aimed at heartbeat-rejection
     log spam) gets a cleaner companion: a positive "shard X is
     alive on host Y" signal that disappears when the binary dies.

## Open questions

- **weftd self-liveness.** weftd dying takes the heartbeat snapshot
  with it; we need a separate signal. Two options:
  1. Coordinator pings each known weftd over the existing gRPC
     channel and emits `weftd_alive` rows itself. Centralizes the
     check, requires no new transport.
  2. weftd writes its own heartbeat to a known log file that gets
     tailed by another process (a tiny supervisor). Requires a new
     supervisor.
  Recommend option 1 as a follow-up; doesn't block this plan.
- **Body-as-extension.** Today the body is ignored. A future
  version could accept JSON like `{"version": "v1.2.3"}` and
  stamp those fields onto the snapshot. Out of v1 scope; the URL-
  identifies-the-service shape leaves room for it.

## Estimated effort

- HTTP listener + handler + service-name validation: **0.5 day**
- In-memory state + LRU eviction + tests: **0.5 day**
- Snapshot ticker + record emission via existing gRPC path: **0.5 day**
- Config plumbing + defaults: **0.25 day**
- weft prod wiring (coordinator/shard/lambda-shard/compute, ~5 lines each): **0.25 day**
- Docs: **0.5 day**
- Tests (handler validation, LRU behavior, snapshot cadence,
  service drops out after stale_after): **0.5 day**

**Total: ~3 days.** No upstream prereqs; can ship before
`http-ingest.md` and `go-sdk.md`.

## Non-goals

- **Not a replacement for application logging.** Heartbeats say
  "the process is running and reachable on the local socket";
  they say nothing about whether it's serving traffic correctly.
  App health remains an app concern.
- **Not a replacement for metrics.** No counter/gauge values in
  the heartbeat record. If a service wants to ship "current queue
  depth," that's a regular metric emission, not a heartbeat
  extension.
- **No bearer-token auth on the endpoint.** Filesystem permissions
  on the Unix socket are the auth boundary; the optional TCP
  listener trades that for ergonomics in trusted environments.
  No header-based secret distribution in v1.
- **Not durable across weftd restarts.** State is in-memory; a
  weftd restart resets the map. Snapshot stream goes silent for
  whatever interval the restart takes; services start re-appearing
  as their next heartbeat lands.
- **Not a leadership/election primitive.** A heartbeat says "I'm
  alive"; it does not say "I'm the leader." Don't build leader
  election on top.
- **Not SDK-coupled.** SDKs are the recommended surface, but the
  wire protocol is the canonical interface and remains supported
  indefinitely. Languages we don't ship an SDK for hit the
  endpoint directly.
