# Plan: Python SDK

A small Python library that lets in-process code ship records to
weft — either through the local weftd Unix socket (default) or
directly to `/api/ingest` (fallback). Three v1 features:

1. **Generic event emission** (`weft.send`) for application events.
2. **Heartbeats** (`weft.heartbeat`, `weft.heartbeat_loop`) for
   liveness signaling, fronting the wire protocol defined in
   [`app-heartbeats.md`](app-heartbeats.md).
3. **Runtime stats collector** (`weft.runtime.start`) that
   periodically emits Python-flavored process telemetry: GC
   generation stats, thread count, RSS, FD count, uptime.

Ships in lockstep with [`go-sdk.md`](go-sdk.md) so feature parity
holds from day one. Depends on
[`http-ingest.md`](http-ingest.md) for the fallback transport;
heartbeats and the local-socket path work without it.

## Why

Roughly half of the customer apps weft serves are Python — Django,
Flask, FastAPI, plain workers, data pipelines. Telling those
customers "POST NDJSON to this URL" works but doesn't meet them
where they are. A `pip install weft` + three lines in `main()` is
the experience this plan delivers.

The Go SDK ([`go-sdk.md`](go-sdk.md)) covers the same surface for
weft's own binaries plus customer Go services; this plan is the
parallel deliverable for the other half. Same wire protocol on the
other side; idiomatic API on each side.

Python's runtime story differs from Go's in ways the SDK can't
paper over (no goroutines; GC is reference-counted plus
generational, not concurrent), so the runtime collector emits a
Python-shaped record. The Go and Python runtime records share
fields where they correspond (RSS, uptime, FDs) and diverge where
they don't.

## Architecture

### Two transports

```
            ┌──────────────────────────────────────────┐
            │  customer / weft Python process          │
            │                                          │
            │   import weft                            │
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

Mirrors the Go SDK transport design exactly. Local-weftd-socket
first (records via the existing socket listener; heartbeats via
the dedicated heartbeat socket from `app-heartbeats.md`), HTTP
fallback for environments without local weftd (Lambda,
serverless, customer dev boxes).

Selection happens once at construction. Apps that need to override
pass `weft.with_http(...)` or `weft.with_socket(...)` explicitly.

### API surface

```python
# Top-level convenience — uses a module-global Client constructed
# from env on first use. Suitable for 90% of cases.

import weft

weft.send(app, event)             # event is a dict; serialized to one JSON line
weft.send_line(app, line)         # for already-serialized JSON
weft.heartbeat(service)           # one-shot heartbeat
weft.heartbeat_loop(service, interval=30)  # spawns a daemon thread; returns a Stop handle

weft.flush()                      # block until pending records drain
weft.close()                      # flush + shut down the global client


# Explicit Client — for tests, multiple destinations, or apps that
# want full lifecycle control.

from weft import Client

c = Client()                                  # env-driven
c = Client.with_http(endpoint, token)         # explicit HTTP
c = Client.with_socket(path)                  # explicit socket

c.send(app, event)
c.heartbeat(service)
stop = c.heartbeat_loop(service, interval=30)
stop()                                         # cancels the loop
c.close()
```

Casing follows PEP 8 (`send`, not `Send`). Type hints throughout —
`from __future__ import annotations`, generic types in stdlib
(no `typing.Dict[...]` ceremony).

### Heartbeat loop implementation

A daemon thread (`threading.Thread(daemon=True)`) ticks every
`interval` seconds, calling `heartbeat(service)`. Returns a
zero-arg callable that signals the thread to stop and joins
briefly.

Daemon thread (rather than asyncio task) chosen because the SDK
must work for sync callers (Django views, Flask routes, scripts,
data pipelines) and async callers (FastAPI, aiohttp) without
forcing one to learn the other. A daemon thread is invisible to
both. Async-native callers can opt in to an `aweft` shim later if
demand exists; v1 is sync-call-friendly.

### Runtime collector

```python
import weft.runtime

weft.runtime.start(service="my-service", interval=30)
```

One record per tick, shape:

```json
{
  "type": "py_runtime",
  "ts": "2026-04-28T20:55:36Z",
  "service": "my-service",
  "uptime_s": 21847,
  "py_version": "3.12.4",
  "threads": 8,
  "fds_open": 23,
  "rss_mb": 142.3,
  "gc_count_gen0": 142891,
  "gc_count_gen1": 12453,
  "gc_count_gen2": 89,
  "gc_objects_tracked": 184523,
  "gc_collected_total": 1429328,
  "gc_uncollectable_total": 0
}
```

Fields chosen for what's actually diagnosable:

- `threads` — thread leaks. From `threading.active_count()`.
  Stdlib only.
- `fds_open` — read from `/proc/self/fd`. Linux-only, like the
  rest of weft. FD leaks are common in Python apps that forget
  `with open(...)`.
- `rss_mb` — read from `/proc/self/statm`. Stdlib only — no
  psutil dependency.
- GC stats from `gc.get_stats()` (per-generation collection
  counts) and `gc.get_count()` (objects since last collection).
  Generation 0/1/2 break down where memory churn is happening.
- `gc_objects_tracked` — `len(gc.get_objects())`. Working-set
  proxy for the GC's view of the heap.

**No psutil dependency in v1.** Everything above is stdlib +
`/proc`. If a customer wants more (per-thread CPU, open
connections, etc.) they can layer their own `psutil` calls and
`weft.send` the result. Keeping the install ~50KB pure-Python
matters for Lambda layers and slim containers.

### Configuration

| Env | Default | Meaning |
|---|---|---|
| `WEFT_SOCKET` | `/var/run/weftd.sock` | Records socket path. Empty disables local transport. |
| `WEFT_HEARTBEAT_SOCKET` | `/var/run/weftd-heartbeat.sock` | Heartbeat socket path (per `app-heartbeats.md`). |
| `WEFT_ENDPOINT` | `https://ingest.w3ft.net` | HTTP fallback endpoint. |
| `WEFT_TOKEN` | (none) | Bearer for HTTP transport. Required if HTTP is selected. |
| `WEFT_BATCH_BYTES` | `262144` | Flush threshold. |
| `WEFT_BATCH_MS` | `100` | Time-based flush. |

Programmatic equivalents via `weft.configure(...)` or
`Client(socket=..., endpoint=..., token=..., ...)`.

## Parity with the Go SDK

Both SDKs ship in lockstep with the same v1 feature set. Casing
follows each language's conventions.

| Feature | Python | Go |
|---|---|---|
| Generic event emission | `send(app, event)` | `Send(app, event)` |
| Heartbeat one-shot | `heartbeat(service)` | `Heartbeat(service)` |
| Heartbeat loop helper | `heartbeat_loop(service, interval)` | `HeartbeatLoop(ctx, service, interval)` |
| Process basics (RSS, uptime) | runtime collector | runtime collector |
| FD count | `/proc/self/fd` | `/proc/self/fd` |
| Language heap stats | `gc.get_stats()` | `runtime.MemStats` |
| Language-specific extras | thread count, gc generation counts | goroutines, GC pause histogram |

Where a Python feature has no Go analogue (or vice versa), the
SDK ships nothing rather than fake it. Goroutines and Python
threads are not the same shape and shouldn't pretend to be.

Neither SDK lands without the other; same release branch, same tag
day. Drift between them is a bug.

## Components to build

1. **`clients/python/weft/`** — new package, published to PyPI as
   `w3ft` (the `weft` name is taken by an unrelated project; the
   import name remains `weft`). Pure Python, no compiled extensions,
   no required deps.
   Optional `weft[http]` extra pulls in `urllib3` for the HTTP
   fallback if the host's stdlib `http.client` proves too limited
   in practice (probably not — keep it stdlib-only as long as we
   can).
   - `weft/__init__.py` — module-level convenience functions
     (`send`, `heartbeat`, etc.) backed by a lazily-constructed
     module-global `Client`.
   - `weft/client.py` — `Client` class; transport selection.
   - `weft/_transport_socket.py` — Unix-socket NDJSON writer with
     reconnect-on-error.
   - `weft/_transport_http.py` — bearer-auth POST via
     `http.client` (or `urllib3` if we go that route).
   - `weft/_heartbeat.py` — daemon-thread loop helper.
   - `weft/runtime/__init__.py` — `start()` function and the
     periodic ticker (also a daemon thread).
   - `weft/runtime/_proc_linux.py` — `/proc/self/fd`,
     `/proc/self/statm` readers; non-Linux fallbacks return None.
2. **Tests** — `pytest`-driven; no network. Mock socket and HTTP
   transports.
3. **`docs/manual/python-sdk.md`** — install snippet, env-var
   table, the runtime record schema, the parity caveat (don't
   write code that assumes a goroutines field exists in Python's
   record), Django/Flask/FastAPI integration recipes.
4. **PyPI release** — Reuses the existing release pipeline
   (`.kevcons/ext/release.lua`). Versioned independently from the
   main weft module so the SDK can iterate without forcing
   coordinator/shard rebuilds. First publish: `w3ft 0.1.0`. See
   [Publishing to PyPI](#publishing-to-pypi) below for the
   operational recipe.
5. **Type stubs** — none needed; the package is fully typed
   inline. PEP 561 marker file (`py.typed`) so consumers' type
   checkers see the annotations.

## Estimated effort

- Package scaffolding + Unix socket transport + tests: **0.75 day**
- HTTP fallback transport (bearer auth, batching, retry): **0.5 day**
- `heartbeat` + `heartbeat_loop` (daemon thread): **0.25 day**
- Runtime collector (gc stats + threads + FDs + RSS): **0.5 day**
- Linux `/proc/self/{fd,statm}` reader + tests: **0.25 day**
- Django/Flask/FastAPI integration recipes in docs: **0.5 day**
- PyPI publish setup + first release: **0.25 day**
- Tests (mocked transports, runtime ticker, daemon-thread cleanup
  on `close()`): **0.5 day**

**Total: ~3.5 days.** Same magnitude as the Go SDK; same upstream
prereqs (`app-heartbeats.md` for the heartbeat endpoint;
`http-ingest.md` for the HTTP fallback transport).

## Publishing to PyPI

The PyPI distribution name is **`w3ft`**; the import name remains
`weft` (the `weft` name on PyPI is held by an unrelated package).
Set `name = "w3ft"` in `pyproject.toml` — consumers run
`pip install w3ft` and then `import weft`.

### One-time setup

1. **Create a PyPI API token** at
   <https://pypi.org/manage/account/token/>. Scope "Entire account"
   for the first publish; narrow to project-scoped after the
   project page exists. Repeat at
   <https://test.pypi.org/manage/account/token/> for TestPyPI.
2. **Stash both tokens** in `~/.pypirc` with mode 0600:
   ```ini
   [pypi]
   username = __token__
   password = pypi-AgEI...

   [testpypi]
   repository = https://test.pypi.org/legacy/
   username = __token__
   password = pypi-AgEN...
   ```
3. **Decide on ownership before the first upload.** A personal
   account is fine for a single maintainer; create a `w3ft-net`
   PyPI org if the SDK should be co-maintained. Re-homing a
   project from one account to another later is a manual PyPI
   support ticket — easier to start in the right place.
4. **Verify project URLs resolve.** `pyproject.toml`'s
   `[project.urls]` block points at `weft.net` / `weft.net/docs`;
   dead links on the PyPI project page are a bad first impression.

### Each release

```bash
cd ~/dev/weft-py
pipenv run pip install --upgrade build twine
pipenv run python -m build       # → dist/w3ft-X.Y.Z-py3-none-any.whl + .tar.gz
pipenv run twine check dist/*    # README rendering sanity check

# TestPyPI dry run — same code path, cheap insurance
pipenv run twine upload --repository testpypi dist/*
pip install --index-url https://test.pypi.org/simple/ --no-deps w3ft
python -c "import weft; print(weft.__version__)"

# Real upload
pipenv run twine upload dist/*
```

The version comes from `pyproject.toml`'s `version` field — bump
it before building. PyPI rejects re-uploads of a version that's
already been published, even if it was subsequently yanked, so a
botched release means version-bumping forward rather than
overwriting.

## Non-goals

- **Not a metrics library.** No counters, gauges, or histograms
  with state held in the SDK. Same posture as the Go SDK; same
  reasoning. Customers wanting StatsD-shape APIs use a StatsD
  client and ship aggregates via `weft.send`.
- **Not a tracing library.** Weft does logs and metrics; tracing
  is a separate product question.
- **Not its own spool.** If the local socket fails the SDK retries
  the next batch; if HTTP fails it drops with a logged warning.
  Spool-on-disk is weftd's job (see `weftd-disk-spool.md`).
- **Not psutil-dependent.** Pure stdlib + `/proc`. Customers who
  want richer process metrics import psutil themselves and
  `weft.send` the result.
- **Not Windows.** Per `feedback_no_windows.md` — weft is Linux-
  only by design. The `/proc/self/...` readers are Linux-only;
  non-Linux falls back to None for those fields.
- **Not asyncio-native in v1.** Daemon-thread heartbeat loop and
  ticker work for both sync and async callers without forcing the
  caller to learn one or the other. An `aweft` async-native shim
  is a future possibility if demand exists.
- **Not OpenTelemetry-compatible.** OTel's `metrics.Meter` API
  shape would force counter/gauge abstractions we explicitly
  don't want. Customers who need OTel emit through their existing
  OTel collector and let `http-ingest.md`'s OTel-attribute
  normalization land them in the right columns.
- **Not auto-instrumentation.** No Django middleware, no Flask
  before-request hook, no `logging.Handler` bridge in v1. The SDK
  ships records the caller composes; convenience wrappers can
  come later.
