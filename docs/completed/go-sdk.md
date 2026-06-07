# Plan: Go SDK

A small Go library that lets in-process code ship records to weft —
either through the local weftd Unix socket (default) or directly to
`/api/ingest` (fallback for environments without local weftd). Three
v1 features:

1. **Generic event emission** (`weft.Send`) for application events.
2. **Heartbeats** (`weft.Heartbeat`, `weft.HeartbeatLoop`) for
   liveness signaling, fronting the wire protocol defined in
   [`app-heartbeats.md`](app-heartbeats.md).
3. **Runtime stats collector** (`weft.runtime.Start`) that
   periodically emits `runtime.MemStats`, goroutine count, FD count,
   and GC pause data — the highest-leverage thing the SDK enables
   and the hardest to retrofit later.

Ships in lockstep with [`python-sdk.md`](python-sdk.md) so feature
parity holds from day one. Depends on
[`http-ingest.md`](http-ingest.md) for the fallback transport;
heartbeats and the local-socket path work without it.

## Why

Three drivers for a Go SDK on top of the generic HTTP intake:

1. **Customers running Go services** want to emit structured events
   without writing to a logfile and waiting for weftd to tail it.
   The same applies to Python customers and is solved in
   [`python-sdk.md`](python-sdk.md); the languages each get their
   own package because casing/idioms differ and a shared interface
   crosses no useful boundary.
2. **Weft's own Go binaries** — coordinator, shard, lambda-shard,
   compute, weftd itself — currently emit zero process-level
   telemetry. The recent prod audit found we have OS-level metrics
   (cpu/mem/disk via `cmd/weftd/metrics.go`) but no goroutine count,
   no heap, no GC pause, no FD count. An OOM shows up as the
   instance falling out of metrics; a goroutine leak shows up as
   nothing at all. The SDK is the natural home for a runtime-stats
   collector that closes that gap with one import statement per
   binary.
3. **Heartbeats** ([`app-heartbeats.md`](app-heartbeats.md)) ship
   over a wire protocol that anything with HTTP can use, but the
   ergonomic experience is `weft.HeartbeatLoop(ctx, "my-service",
   30*time.Second)` — one line in `main()`, no goroutine to manage,
   no URL string to remember.

## Architecture

### Two transports

```
            ┌──────────────────────────────────────────┐
            │  customer / weft Go process              │
            │                                          │
            │   import "github.com/graybeardcorp/weft-go"  │
            │                                          │
            │     │                                    │
            │     ▼                                    │
            │   transport selection                    │
            │   (env: WEFT_SOCKET / WEFT_ENDPOINT)     │
            └──────┬─────────────────────────┬─────────┘
                   │                         │
           default ▼                fallback ▼
            ┌──────────────┐         ┌───────────────────┐
            │ /var/run/    │         │ HTTPS POST to     │
            │ weftd.sock   │         │ ingest.w3ft.net   │
            │ (NDJSON)     │         │ /api/ingest       │
            └──────────────┘         └───────────────────┘
```

**Local-weftd socket (default).** Reuses the existing
`weftd.SocketListener` (`weftd/socket.go`) — apps already write
NDJSON to `/var/run/weftd.sock` via `nc -U` for shell scripts; the
SDK is the typed Go API on the same channel. weftd handles auth,
batching, gzip, and on-failure spool (see `weftd-disk-spool.md`),
so the SDK stays a few hundred lines.

**HTTP fallback.** When `WEFT_SOCKET` resolves to a missing or
unreadable path (Lambda, serverless containers without weftd,
customer dev boxes), the SDK falls through to direct
`http-ingest.md` POSTs. Auth via `WEFT_TOKEN` bearer; endpoint via
`WEFT_ENDPOINT` (default `https://ingest.w3ft.net`). Same NDJSON
body shape `http-ingest.md` already accepts.

Selection happens once at construction, not per-record. Apps that
need to override (e.g., a process inside a container that has
weftd but wants to bypass it) pass `weft.WithHTTP(...)` or
`weft.WithSocket(...)` explicitly.

### API surface

```go
package weft

// Client is the typed handle. Safe for concurrent use.
type Client struct { ... }

// New constructs a client using env-driven defaults. The returned
// client batches in-process and flushes every 100ms or 100 records,
// matching weftd's own internal cadence.
func New(opts ...Option) (*Client, error)

// Send queues a single record. App is the source identifier — it
// flows to the existing `app` column. Event is anything that
// json-marshals to a single object.
func (c *Client) Send(app string, event any) error

// SendLine is the convenience for already-marshaled JSON; useful
// when interop with text-shaped log infrastructure.
func (c *Client) SendLine(app string, line string) error

// Heartbeat fires one heartbeat for the named service. Maps to
// POST /h/<service> on the wire (see app-heartbeats.md). Returns
// quickly; does not block on remote success.
func (c *Client) Heartbeat(service string) error

// HeartbeatLoop heartbeats every interval until ctx is cancelled.
// One line in main(); the SDK manages the goroutine. Default
// interval is 30s when interval is zero.
func (c *Client) HeartbeatLoop(ctx context.Context, service string, interval time.Duration)

// Close flushes pending records and waits for them to drain.
// HeartbeatLoop goroutines stop when their ctx is cancelled, not
// when Close is called — they're caller-scoped.
func (c *Client) Close() error
```

That's the whole generic surface. No counter / gauge / histogram
abstractions — those are out of scope (see Non-goals).

### Runtime collector

```go
package weft/runtime

// Start launches a goroutine that emits a runtime stats record
// every interval. Records are stamped with app="<service>/runtime"
// so they live alongside but don't pollute application logs.
//
// Safe to call from main(); cancellation flows through ctx.
func Start(ctx context.Context, c *weft.Client, service string, interval time.Duration)
```

One record per tick (default 30s), shape:

```json
{
  "type": "go_runtime",
  "ts": "2026-04-28T20:55:36Z",
  "service": "coordinator",
  "uptime_s": 21847,
  "goroutines": 47,
  "fds_open": 23,
  "heap_alloc_mb": 142.3,
  "heap_sys_mb": 256.0,
  "heap_inuse_mb": 158.4,
  "stack_inuse_mb": 4.5,
  "gc_pauses_p50_us": 81,
  "gc_pauses_p95_us": 234,
  "gc_pauses_p99_us": 612,
  "gc_count": 1429,
  "next_gc_mb": 280
}
```

Fields chosen for what's actually diagnosable:

- `goroutines` — leak detector. Trend matters more than absolute.
- `fds_open` — read from `/proc/self/fd`. Linux-only, like the rest
  of weft. File-descriptor leaks are the second-most-common Go
  resource leak after goroutines and the stdlib doesn't expose it.
- Heap fields straight from `runtime.MemStats`. `heap_inuse_mb` is
  the working-set proxy.
- GC pause percentiles — `runtime.MemStats.PauseNs` is a ring of
  the last 256 pauses; we summarize as p50/p95/p99 to keep the
  emitted record bounded.
- `gc_count` and `next_gc_mb` for tuning headroom.

The shape lands as a regular JSON row; queries like
`select(.type == "go_runtime" and .service == "coordinator" and .goroutines > 1000)`
just work — no special planner integration.

### Configuration

| Env | Default | Meaning |
|---|---|---|
| `WEFT_SOCKET` | `/var/run/weftd.sock` | Unix socket path. Empty disables local transport. |
| `WEFT_ENDPOINT` | `https://ingest.w3ft.net` | HTTP fallback endpoint. |
| `WEFT_TOKEN` | (none) | Bearer for HTTP transport. Required if HTTP is selected. |
| `WEFT_BATCH_BYTES` | `262144` | Flush threshold. |
| `WEFT_BATCH_MS` | `100` | Time-based flush. |

Programmatic equivalents via `weft.WithSocket`, `weft.WithHTTP`,
`weft.WithToken`, `weft.WithBatch`. Env-only is the common case;
options are for tests and embedded scenarios.

## The "does weftd self-monitor through this SDK" question

Weft's binaries split into two camps:

- **coordinator, shard, lambda-shard, compute** — long-running Go
  processes that should emit runtime stats. They run alongside a
  local weftd in prod, so they import the SDK and use the local-
  socket transport. Done.
- **weftd itself** — the data-plane agent. Importing its own SDK
  for internal stats would create a circular dependency
  (weftd-the-agent → SDK → weftd-the-socket), and its "I'm dead"
  failure mode is precisely the case the SDK can't observe.

The recommendation: weftd has its own internal stats emission
(separate from the SDK's public package), pulling from the same
`runtime.MemStats` via a small unexported helper. The output is
identical in shape, so dashboards don't care. weftd's runtime
stats then ride the same logical pipeline weftd already uses to
ship its OS metrics today (`cmd/weftd/metrics.go`'s gRPC
`IngestStream`) — under `app=<namespace>/<deployment>_metrics`
alongside the cpu/mem/disk records.

This is the same shape as Datadog Agent's self-monitoring: external
apps use the public StatsD client; the agent's own runtime stats
come from an internal collector that talks directly to its in-
process emitter.

## Parity with the Python SDK

Both SDKs ship in lockstep with the same v1 feature set. Casing
follows each language's conventions (`Send` / `Heartbeat` in Go,
`send` / `heartbeat` in Python).

| Feature | Go | Python |
|---|---|---|
| Generic event emission | `Send(app, event)` | `send(app, event)` |
| Heartbeat one-shot | `Heartbeat(service)` | `heartbeat(service)` |
| Heartbeat loop helper | `HeartbeatLoop(ctx, service, interval)` | `heartbeat_loop(service, interval)` |
| Process basics (RSS, uptime) | runtime collector | runtime collector |
| FD count | `/proc/self/fd` | `/proc/self/fd` (or psutil) |
| Language heap stats | `runtime.MemStats` | `gc.get_stats()` |
| Language-specific extras | goroutines, GC pause histogram | thread count, gc generation counts |

Where a Go feature has no Python analogue (or vice versa), the
SDK ships nothing rather than fake it. Goroutines and Python
threads are not the same shape and shouldn't pretend to be.

Neither SDK lands without the other; same release branch, same tag
day. Drift between them is a bug.

## Components to build

1. **`clients/go/weft/`** — new top-level module
   `github.com/w3ft-net/weft-go`. Single-package public API as
   sketched above. Internal types stay unexported.
   - `transport_socket.go` — Unix-socket NDJSON writer with
     reconnect-on-error and a small in-memory ring (the SDK's local
     buffer; weftd does the heavy buffering).
   - `transport_http.go` — bearer-auth `http.Client` with gzip,
     batching, retry-with-backoff. Fewer features than weftd because
     spool is out of scope (see Non-goals).
   - `heartbeat.go` — `Heartbeat(ctx, service, interval)` runs the
     ticker loop until ctx is cancelled. Talks to the dedicated
     heartbeat endpoint defined in `app-heartbeats.md`; not the
     same channel as record emission.
   - `client.go` — public API; transport selection at construction.
2. **Runtime stats — same package, not a sub-package.**
   `c.RuntimeStats(ctx, service, interval)` is a method on
   `*Client` that periodically emits Go-runtime telemetry. No
   separate import.
3. **`clients/go/weft/fds_linux.go`** — `/proc/self/fd` count.
   Linux-only build tag; non-Linux is a stub returning 0.
4. **`clients/go/weft/CHANGELOG.md`** — versioned independently
   from the main module so the SDK can iterate without forcing
   coordinator/shard rebuilds.
5. **Wiring inside weft binaries** — `cmd/coordinator/main.go`,
   `cmd/shard/main.go`, `cmd/lambda-shard/main.go`,
   `cmd/compute/...` each gain four lines:
   ```go
   wc, _ := weft.New()
   defer wc.Close()
   go wc.Heartbeat(ctx, "coordinator", weft.DefaultHeartbeatInterval)
   go wc.RuntimeStats(ctx, "coordinator", weft.DefaultRuntimeStatsInterval)
   ```
6. **Internal weftd self-monitoring** — `cmd/weftd/runtime.go`,
   pulls from the same `runtime.MemStats` source but emits via the
   in-process gRPC path rather than importing the public SDK. Output
   shape identical to the SDK's runtime record.
7. **`clients/go/weft/example_test.go`** — runnable doc example
   showing the three-line setup and a `Send` call.
8. **`docs/manual/go-sdk.md`** — install snippet, env-var table,
   the runtime record schema, the "weft binaries also use this"
   note. Cross-link from `docs/manual/http-ingest.md`.
9. **Release pipeline** — module gets its own tag on each release
   (`weft-go/v0.1.0` style — Go tooling handles sub-module tags
   correctly when the directory layout matches). Reuses the
   `.kevcons/ext/release.lua` pipeline that already publishes the
   Python SDK.

## Estimated effort

- Public API + Unix socket transport + tests: **0.75 day**
- HTTP fallback transport (auth, batching, retry): **0.5 day**
- `Heartbeat` + `HeartbeatLoop` (talks to the heartbeat endpoint
  defined in `app-heartbeats.md`): **0.25 day**
- Runtime collector (MemStats + GC pauses + goroutine count): **0.5 day**
- Linux `/proc/self/fd` reader + tests: **0.25 day**
- Coordinator / shard / compute / lambda-shard wiring: **0.25 day**
- weftd internal self-monitoring (separate path): **0.5 day**
- Docs (`docs/manual/go-sdk.md`) + example: **0.5 day**

**Total: ~3.5 days.** Depends on `app-heartbeats.md` for the
heartbeat endpoint; depends on `http-ingest.md` for the HTTP
fallback transport. The local-socket record path works against
the existing `weftd.SocketListener` without further upstream work.

## Non-goals

- **Not a metrics library.** No counters, gauges, or histograms
  with state held in the SDK. Customers wanting "increment this
  counter on every request" wire that up themselves and `Send`
  the periodic snapshot. The platform's "build metrics from log
  lines via watches" story (see `coordinator/weftfuncs.go`) is
  what turns events into aggregates.
- **Not a tracing library.** Weft does logs and metrics; tracing
  is a separate product question.
- **Not its own spool.** If the local weftd transport fails, the
  SDK retries the next batch; if HTTP transport fails, the SDK
  drops with a logged warning. Spool-on-disk is weftd's job (see
  `weftd-disk-spool.md`); the SDK delegates rather than
  duplicating.
- **Not Windows.** Per `feedback_no_windows.md` — weft is
  Linux-only by design. The `/proc/self/fd` reader is build-
  tagged Linux; non-Linux builds compile but emit `fds_open: 0`
  rather than fail.
- **Not OpenTelemetry-compatible.** OTel's `metrics.Meter` API
  shape would force counter/gauge abstractions we explicitly
  don't want. Customers who need OTel emit through their existing
  OTel collector and let `http-ingest.md`'s OTel-attribute
  normalization land them in the right columns.
- **Not auto-instrumentation.** No HTTP middleware, no DB
  instrumentation, no `expvar` bridge in v1. The SDK ships records
  the caller composes; convenience wrappers can come later if
  there's demand.
