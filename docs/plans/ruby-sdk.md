# Ruby SDK

## Status

🟡 Designed, not started. Targets the parity bar set by `weft-go` and
`weft-py` after the SDK-fed logging work ([sdk-line-header](sdk-line-header.md))
and the span/trace work ([request-tracing](request-tracing.md)). Shape
follows the Python SDK ([completed/python-sdk](../completed/python-sdk.md)):
a small library, same wire protocol, idiomatic API.

## Problem

Ruby shops (Rails, Sidekiq, Sinatra, plain workers) have no SDK. They can
point weftd at a log file and walk the schema wizard, or POST NDJSON by hand.
Neither gives them trace IDs on every line, spans, heartbeats or runtime
stats, which is what the Go and Python SDKs deliver.

## Goals

- `gem install w3ft`, `require "weft"`, one namespace, no setup beyond
  an `init_logging` call and an optional `Weft::Client`.
- Same wire behaviour as Go and Python: records socket and HTTP fallback,
  heartbeats and runtime stats on the records transport, line header v1 in
  an SDK-owned log file, stdout/stderr capture.
- Ambient trace and span context that works across threads, fibers and the
  common servers without the caller threading anything through.
- Zero runtime dependencies. Stdlib only (`socket`, `json`, `net/http`,
  `securerandom`, `logger`, `monitor`).
- A conformance suite shared with Go and Python so parity is tested, not
  asserted.

## Non-goals

- Windows. weftd is Linux-only; capture and fd handling are Linux/macOS
  (development) only, and capture is skipped elsewhere.
- Ruby older than 3.2, JRuby and TruffleRuby in v1 (see Versions).
- An OpenTelemetry bridge. Spans are weft's own model, as in Go.
- Patching third-party gems globally. Outbound propagation is an explicit
  helper or opt-in Faraday middleware, never a monkey patch that is on by
  default.
- Replacing the app's logger. The gem supplies a logger device and a
  formatter; the app keeps `Logger`, Rails.logger or SemanticLogger.

## Naming and surface

Gem `w3ft` (matches the PyPI name; `weft` is taken on RubyGems), required as
`require "weft"`. Everything hangs off `Weft`; there is no second require.

```ruby
require "weft"

log = Weft.init_logging(fallback: $stderr)   # returns Weft::LogFile or nil
log&.capture_stdio

Weft.info("listening on #{port}", port: port)
Weft.span("db.query") { |s| ... }

Weft.send("orders", {id: 1})
Weft.send_line("orders", "raw line")
Weft.heartbeat_loop("orders")
Weft.runtime_stats("orders")

Weft::Client.new(socket: "/var/run/weftd.sock")     # explicit handle
Weft::Client.with_http(endpoint, token)
```

Module functions use a lazily built process-global client, as in Python;
`Weft::Client` is the explicit form for tests and multiple destinations.
Method names are snake_case; keyword arguments for options.

gRPC, Rack and Faraday helpers live in the same gem but load nothing until
used: `Weft::GRPC`, `Weft::Rack::TraceMiddleware`, `Weft::Faraday`. They are
`autoload`ed constants that `require` their dependency lazily, so
`require "weft"` never loads `grpc`, `rack` or `faraday`.

## Parity matrix

Reference columns are the Go SDK at v0.6+ and the Python SDK in the state
the parity work leaves it. "Ruby" is what this plan builds.

| Feature | Go | Python | Ruby |
|---|---|---|---|
| `send` / `send_line` | yes | yes | yes |
| Socket transport, reconnect-once | yes | yes | yes |
| HTTP fallback | yes | yes | yes |
| `with_socket` / `with_http` | yes | yes | yes |
| Heartbeat, loop, extra fields | yes | yes | yes |
| Runtime stats, extra fields | yes (`go_runtime`) | yes (`py_runtime`) | yes (`rb_runtime`) |
| Trace ID ambient context | yes | yes | yes |
| Span ambient context, `start_span` | yes | yes | yes |
| `traceparent` parse/format with span ID | yes | yes | yes |
| HTTP server middleware | `TraceMiddleware` | WSGI/ASGI | Rack |
| Outbound header injection | `TraceHeaders` | `trace_headers` | `Weft.trace_headers`, Faraday middleware |
| Line-headered log file (`init_logging`) | yes | yes | yes |
| Second stream (`kind`) in the same file | `KindHandler` | `handler(kind=)` | `log.logger(kind:)` |
| `info`/`warn`/`error` helpers with ctx | `Infof` | `weft.logging` | `Weft.info` |
| stdout/stderr capture | yes (pipes + `dup3`) | yes (writers + `dup2`) | yes (`$stdout` reassign + `dup2`) |
| gRPC trace propagation | `weft.GRPC` | `weft.grpc` | `Weft::GRPC` |

## Ambient context

Go carries trace and span on `ctx`; Python uses `contextvars`; Ruby needs a
storage that follows a request across threads and fibers.

- Storage is **fiber-local**: `Fiber[]` / `Fiber.current.storage` on Ruby
  3.2+, which is inherited by child fibers and, since 3.2, by
  `Thread.new` at creation. This is the same inheritance model as
  `contextvars` and matches what Async (fiber scheduler) expects.
- `Thread.current[:key]` is fiber-local in Ruby, not thread-local, and does
  not inherit. It is not used, because an `Enumerator` or any code that
  hops fibers would lose the trace ID.
- Rails: a request that runs under `ActiveSupport::IsolatedExecutionState`
  (thread or fiber, per `config.active_support.isolation_level`) is
  unaffected, because the gem owns its own `Fiber[]` keys and the Rack
  middleware sets and restores them around the request. No dependency on
  ActiveSupport.
- API shape: `Weft.with_trace(id) { }` and `Weft.with_span(span) { }` set and
  restore around a block (the Ruby analog of `set`/`reset` pairing in
  Python); `Weft.trace_id` and `Weft.current_span_id` read. There is no
  unpaired setter, because Puma and Sidekiq reuse threads and an unpaired
  set leaks across requests.
- Cross-thread handoff to a pool (Sidekiq jobs, `Concurrent::Future`) is the
  caller's job: capture `Weft.context` (a frozen `{trace_id, span_id}`)
  and re-enter with `Weft.with_context(ctx)`. Sidekiq middleware that does
  this from the job payload is a follow-up, not v1.

## Spans

Same model as Go: `Weft.span("name") { |s| ... }` parents off the ambient
span, becomes ambient inside the block, logs a start line and a done line
through the logger, and the log handler moves `span_name`, `span_id`,
`parent_span_id` and `duration_ns` into the header. `s.record_error(e)`
marks the done line; an exception escaping the block records the error and
re-raises. A span with no trace ID in scope generates none and is a
no-op wrapper that still yields (matches Go: no fabricated trace IDs).

## Logging

### Line-headered file

`Weft.init_logging(path = nil, fallback: $stderr)` mirrors Go and Python:

- path or `$WEFT_LOG_FILE` opens an `O_APPEND` file; else JSON lines go to
  the fallback with `trace_id` as a field; an unopenable file is reported on
  the fallback and degrades the same way.
- Returns a `Weft::LogFile` or `nil` (no file, no capture); the README shows `log&.capture_stdio`.
- Header v1 is written by one `Weft::Header.encode` shared by all paths;
  values containing `\x1e`, `\x1f` or `\n` are dropped, as elsewhere.
- Each line is one `write(2)` of the whole `\x1e...\x1e body \n` string via
  `IO#syswrite` on a file opened `File::WRONLY | File::APPEND | File::CREAT`.
  Lines under `PIPE_BUF` are atomic across processes; longer lines are
  atomic per-`write` on regular files with `O_APPEND` on Linux, which is the
  property the Go and Python writers already rely on.

### Logger integration

- `log.logger(kind: nil, level: ::Logger::INFO)` returns a stdlib
  `::Logger` whose logdev is the `LogFile` and whose formatter emits the
  header plus a JSON body (`{"msg": ..., attrs}`), reading trace and span
  from ambient context. `Rails.logger = log.logger` is the Rails
  integration; no Railtie, no dependency.
- `Weft.info(msg, **attrs)` / `warn` / `error` go through that logger.
  Unlike Go there is no ctx argument, because context is ambient; the
  signature is the Ruby-natural one and the parity matrix row is about the
  capability.
- Tagged and structured logging: `ActiveSupport::TaggedLogging` wrapping
  the logger works unchanged (tags land in the message). SemanticLogger gets
  an appender documented in the README (`SemanticLogger::Appender::IO`
  on a `Weft::LogFile`-backed IO with a formatter that calls
  `Weft::Header.encode`); no code in the gem depends on SemanticLogger.
- Exceptions: the formatter puts `class`, `message` and `backtrace` in the
  JSON body so a multi-line backtrace is one line (newlines escaped by
  JSON), satisfying the header's "a message containing a raw newline" rule.

### stdout/stderr capture

`log.capture_stdio` mirrors Python's design:

1. Replace `$stdout` and `$stderr` (not the constants `STDOUT`/`STDERR`,
   which C extensions and `Kernel#warn` callers may hold) with line
   writers that write `fmt=text` lines, `kind` `stdout`/`stderr`, trace
   ID from ambient context.
2. `dup2` fds 1 and 2 onto the log file (`IO#reopen` on `STDOUT`/`STDERR`
   with the log file's `File`) so native-extension and crash output lands
   in the same file without a header.
3. Hook `Thread.report_on_exception` output and `at_exit` uncaught
   exceptions through the same writers (Ruby prints those to `$stderr`, so
   step 1 covers them).

Guard rails identical to the other SDKs: skipped when stdout is a tty or
the `LogFile` is nil; explicit call only; documented systemd rule (stop
`StandardOutput=append:` once the SDK owns the fds).

### Fork safety (Puma, Unicorn, Passenger, Sidekiq)

Preforking servers load the app, then fork workers. State that must not
cross a fork:

- the records-socket connection (two processes writing one stream
  interleave frames);
- background threads (heartbeat, runtime stats, do not survive `fork`);
- the log file `File` is fine to share: `O_APPEND` writes from two
  processes are independent lines.

Rules:

- Every stateful object records `Process.pid` at creation and checks it on
  use; a pid mismatch drops the inherited connection and reconnects.
  `Process._fork` is hooked (Ruby 3.1+) so the gem resets itself without the
  app configuring `on_worker_boot`.
- `Weft.heartbeat_loop` and `Weft.runtime_stats` threads are restarted in
  the child by the same hook, and are not started at all before the fork
  if the caller invoked them in `config.ru`/`application.rb` (the hook
  restarts them in each worker). Documented because preload is the common
  setup.
- `capture_stdio` before `fork` is safe: the dup'ed fds and `$stdout`
  replacement are inherited and keep pointing at the same file.

## Transports

- **Socket**: `UNIXSocket`, frame `app:line\n`, or with an ambient trace ID
  `app:\x1eTRACE\x1eline\n` (unchanged envelope; this plan inherits the
  trace-only limitation noted in sdk-line-header). Reconnect once on a
  failed write. A `Mutex` guards the connection; a lines-with-`\n` check
  rejects the record. Connect timeout 2s (`Socket.tcp`-style non-blocking
  connect, since `UNIXSocket.new` has no timeout argument).
- **HTTP**: `Net::HTTP` with persistent connection, bearer token, one-event
  NDJSON body `{"app","message","trace_id"}` to `/api/ingest`, as in Go.
- **Selection**: identical to Go and Python: `with_http` wins; else socket
  if its path resolves; else HTTP if a token is set; else a no-op transport.
  Env: `WEFT_SOCKET`, `WEFT_ENDPOINT`, `WEFT_TOKEN`, `WEFT_LOG_FILE`.
- Delivery is synchronous in v1, as in Go. If hot-path latency matters, a
  bounded queue plus writer thread is a later change; it would need the
  fork hook above and a drop policy, so it is deliberately out.

## Heartbeats and runtime stats

Heartbeat records go to `<service>/heartbeats`, runtime stats to
`<service>/runtime`, both on the records transport, with reserved keys
(`type`, `service`, `ts`) winning over caller-supplied fields.
`Weft.heartbeat_loop(service, interval: 30, fields: {})` and
`Weft.runtime_stats(service, interval: 30, fields: {})` return a stop
handle (`stop.call`), run in a daemon `Thread`, emit immediately and then
every interval, and swallow transport errors.

`rb_runtime` fields (the reasonable divergence from `go_runtime` and
`py_runtime`): `type`, `ts`, `service`, `uptime_s`, `rb_version`, `engine`,
`threads` (`Thread.list.size`), `fds_open` (`/proc/self/fd`), `rss_mb`
(`/proc/self/statm`), plus `GC.stat`: `gc_count`, `gc_major_count`,
`gc_minor_count`, `gc_time_ms` (3.1+), `heap_live_slots`,
`heap_free_slots`, `total_allocated_objects`, `total_freed_objects`,
`malloc_increase_bytes`. `ObjectSpace.count_objects_size` and
`ObjectSpace.each_object` are not sampled by default (both are O(heap) and
stop the world); a `deep: true` option adds `ObjectSpace.count_objects`
(cheap, per-type counts).

## Propagation

- **Rack**: `Weft::Rack::TraceMiddleware` adopts a well-formed inbound
  `traceparent`'s trace ID and span ID, otherwise generates a trace ID,
  runs the app inside `with_trace`/`with_span`, and echoes `Traceparent`
  on the response, same semantics as Go's `TraceMiddleware`. Rails:
  `config.middleware.insert_before 0, Weft::Rack::TraceMiddleware` (no
  Railtie). Sinatra/Hanami/Roda use `use`.
- **Outbound HTTP**: `Weft.trace_headers` returns a hash for `Net::HTTP`,
  HTTParty, Typhoeus (`req.merge!(Weft.trace_headers)`); `Weft::Faraday`
  is a Faraday middleware that adds it. No global patch of `Net::HTTP`.
- **gRPC**: `Weft::GRPC::ClientInterceptor` and `ServerInterceptor`
  (subclasses of `GRPC::ClientInterceptor`/`ServerInterceptor`) carrying
  `weft-trace-id` and `weft-span-id` in metadata, replacing stale values,
  ignoring a span ID with no trace ID, covering unary and streaming
  shapes (`request_response`, `client_streamer`, `server_streamer`,
  `bidi_streamer`), exactly the Go contract. Loaded only when the app
  references `Weft::GRPC`.

## Versions and dependencies

- Ruby **3.2+**: `Fiber[]` storage, `Process._fork` (3.1), `Data` for
  value objects, current CRuby support window. CI matrix 3.2, 3.3, 3.4.
- No runtime dependencies. Development: `minitest`, `rake`, `rack`,
  `grpc`, `faraday` (the optional integrations are tested against the real
  gems, installed as development dependencies only).
- Linux is the supported platform for capture and runtime fds; macOS runs
  the rest for development (no `/proc`; fields degrade to absent, as in the
  other SDKs).

## Testing

**minitest**, not RSpec: it is in the stdlib (zero dev-dependency
baseline), the other SDKs' tests are plain assertions over small
fixtures, and the suite is mostly unit tests over byte strings. Layout
mirrors Python's: one test file per module, a fake socket server fixture
(`UNIXServer` in a tmpdir) for transport tests, a real subprocess for
capture and fork tests (a child that calls `capture_stdio`, writes via
`puts`, `warn`, `system("echo")`, and a native crash path, then the test
reads the file and checks headers).

Cases that must exist because they are Ruby-specific:

- `Fiber[]` inheritance into `Thread.new` and `Fiber.new`, and no leak
  across sequential requests on one reused thread.
- Pid-change reset of the socket and background threads after `fork`
  (`Process.fork` in the test, then send from the child).
- Header drop on values with `\x1e`, `\x1f`, `\n`.
- Rack middleware inside a real `Rack::MockRequest`; gRPC interceptors over
  an in-process `GRPC::RpcServer` with the health service.

## Conformance suite

The three SDKs implement the same byte-level contracts (line header
encoding, `traceparent`, the socket envelope, reserved heartbeat/runtime
keys, validation rules) separately. Today parity is maintained by review.
This plan adds a shared set of vectors that every SDK runs.

- **Location**: the public repo `w3ft-net/sdk-conformance-specs`, holding
  only language-neutral JSON files and a README. It must contain nothing
  from weft internals, for the same reason the SDKs must not. Each SDK
  vendors a copy under `testdata/conformance/` via `make conformance`,
  which syncs from a sibling checkout and records the source commit in
  `testdata/conformance/VERSION`; tests read the files, so no submodule
  checkout is needed to run them. Go and Python are wired in.
- **Vectors** (all six files exist; Ruby implements the same runners):
  - `header.json`: `{input: {ts_ns, level, trace_id, span_name, span_id,
    parent_id, dur_ns, fmt, kind, body}, expect: "<exact line>"}`, including
    values dropped for control bytes, empty values, and zero or negative
    durations.
  - `traceparent.json`: `parse` and `format` cases: valid, malformed,
    all-zero span, uppercase hex, wrong lengths.
  - `socket_frame.json`: `{app, line, trace_id}` to the exact frame, and
    the rejected cases (newline in line, control byte in trace id).
  - `reserved_keys.json`: heartbeat and runtime extras that must not
    clobber `type`, `service`, `ts`.
  - `service_names.json`: valid and invalid heartbeat service names.
  - `grpc_metadata.json`: ctx to outgoing keys and incoming to ctx, incl.
    stale values and orphan span.
- **Runner contract**: each SDK has a test that loads every vector file and
  fails on an unknown file, so adding a vector forces all SDKs to
  implement it before they can bump the pinned copy.
- **Parity ledger**: the repo's README holds the feature matrix above, with
  each cell linking to the SDK test that proves it. A cell with no test is
  a parity gap by definition.
- Go and Python adopt it first (they already have the behaviours), which
  validates the vectors before Ruby depends on them.

## Packaging and release

- Repo `w3ft-net/weft-rb`, gem name `w3ft`, `lib/weft.rb` plus
  `lib/weft/*.rb`; `weft.gemspec` with `required_ruby_version >= 3.2`,
  `metadata["rubygems_mfa_required"] = "true"`.
- Versioning: SemVer, `0.x` until the three SDKs share a conformance
  release; the version lives in `lib/weft/version.rb` and `Weft::VERSION`.
  Public SDK history stays clean: no WIP commits on `main`, release by
  squashed PR.
- Release flow: `rake release` equivalent run by hand: tag `vX.Y.Z`,
  `gem build`, `gem push` (OTP). RubyGems versions are immutable like Go
  proxy tags, so a bad release is yanked and superseded, never retagged.
- Compatibility: Ruby support dropped only in a minor bump pre-1.0 and a
  major after.

## Open questions

1. Gem name: `w3ft` (mirrors PyPI) versus claiming `weft-sdk`. `weft` is
   taken, so `w3ft` is the default.
2. Floor of Ruby 3.2: acceptable, or are there customers on 3.0/3.1
   (where `Fiber[]` does not exist and a `Thread.current`-based fallback
   with its fiber-hop weakness would be needed)?
3. Rails-first or plain-Ruby-first: is a Railtie (auto-insert the
   middleware, wire `Rails.logger`) wanted, accepting an optional
   `railties` dependency, or does the documented two-line setup stay?
4. Sidekiq job propagation (client and server middleware carrying the
   trace through the job payload): in v1 or a follow-up?
5. JRuby: needed? It changes fd capture (`dup2` unavailable) and
   `Process._fork`.
6. ~~Is the conformance repo public?~~ Decided: public, `w3ft-net/sdk-conformance-specs`.
7. Async delivery: keep synchronous like Go, or ship a bounded queue given
   Ruby's GVL makes a blocked socket write more costly to a Puma thread
   than in Go?

## Milestones

1. **Conformance repo with Go and Python wired in. Done.**
   `sdk-conformance-specs` has vectors for header, `traceparent`, frame,
   reserved keys, service names and gRPC metadata; Go and Python run them.
2. **Core gem.** Context storage, trace IDs, `traceparent`, header encoder,
   `Client` with socket and HTTP transports, `send`/`send_line`,
   heartbeats and runtime stats, fork reset. Conformance green.
3. **Logging.** `init_logging`, `LogFile`, `logger(kind:)`, `info`/`warn`/
   `error`, `capture_stdio`, spans.
4. **Propagation.** Rack middleware, Faraday middleware, `trace_headers`,
   gRPC interceptors.
5. **Docs and release.** README mirroring the Go one, `docs/manual`
   section and `connect-source.md` entry for Ruby, `0.1.0` on RubyGems,
   plan moved to `docs/completed/`.
