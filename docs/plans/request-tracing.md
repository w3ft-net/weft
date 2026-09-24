# Request tracing

## Status

🟢 Data model, ingest extraction, the `trace <id>` query mechanism, its
display layer, and internal dogfood propagation have landed (see commits
af20640, 03377f2, c3cea58, and the trace-display follow-up). Not started:
the `weft-go`/`weft-py` SDK surface (context propagation, `Send`/`send`
auto-tagging, logging integration, `net/http` middleware, `TraceHeaders`/
`trace_headers` helpers) and customer-facing docs (component 7 below).

Emerged from a design discussion following
`docs/plans/tail-latency-regression.md` (2026-09-21) — that investigation
exposed a real observability gap (a slow request left no trace of where it
stalled; diagnosing it took ~1.5 hours of live SSM archaeology across the
ALB, the coordinator, and weft's own self-monitoring). This plan turns
that gap-closing work into a real customer-facing feature rather than
one-off internal bookkeeping. Weft becomes a dogfood consumer of its own
feature — the same mechanism that would have made today's incident
legible to a customer is what makes it legible to weft's own team too.

## Why

When a request crosses multiple services, each service's logs are an
isolated stream — nothing ties "this one log line in service A" to "this
one log line in service B" as belonging to the same user-facing request.
For interactive debugging (exactly the shape of the tail-latency
incident), that means reconstructing a request's path by hand across
however many systems it touched. This plan adds a `trace_id` correlation
mechanism so a customer (or weft, debugging itself) can pull every log
line belonging to one request, across every service it touched, with a
single query.

## Non-goals

- **No span model.** No `duration`, `span_kind`, or parent/child
  hierarchy. `trace_id` is a flat correlation field on an ordinary log
  row — there is no span tree to store, render, or query. (Not
  architecturally precluded later — see "Deferred, not precluded" below
  — just not part of this plan.)
- **No OTLP ingest.** Considered and explicitly rejected after evaluating
  whether to replace weftd with the OTel Collector entirely (rejected —
  see `docs/plans/weftd-client-side-parsing.md`'s benchmark: weftd
  deliberately targets ~half the OTel Collector's RSS/CPU footprint by
  carrying no transform language; that decision stands untouched).
  Dropping OTLP means weftd's ingest path needs no new adapter or wire
  protocol for the base mechanism.
- **No dedicated trace UI.** No waterfall, no service map. `trace <id>`
  is a shell verb, consumed with the same pipeline idioms as any other
  log query.
- **No built-in Python web-framework middleware packages** (Flask/
  Django/FastAPI). Python's middleware APIs are incompatible across
  frameworks and the actual mechanism is a few lines regardless of
  framework — ship a primitive + a documented per-framework snippet
  instead of three maintained packages.
- **No sampling design.** Flagged as a future risk below, not designed
  away here — likely a much smaller concern than it would be under a
  span-based design, since this adds a field to data already being
  ingested rather than a new high-cardinality telemetry stream.

## Architecture

### Two complementary correlation mechanisms

1. **Passive — schema-driven extraction on existing plain-text logs.**
   `ingest.Parser` already extracts four schema-declared fields
   (`timestamp_field`, `level_field`, `field_separator`,
   `json_start_field` — see `docs/plans/weftd-client-side-parsing.md`).
   This plan adds a fifth: `trace_id_field`. Because `ingest.Parser` is
   already shared across every ingest path that uses it — weftd
   (client-side), the offline `weft ingest` CLI, and every HTTP-pushed
   adapter (CloudWatch, Heroku, Render) via the shared `enrichRows`/
   `enrichRow` step (`api/adapter.go`) — all of them gain trace
   correlation simultaneously, with no path-specific work. Zero
   code change for any customer who already embeds *some* correlation ID
   in their logs by any existing means (an API gateway request ID, a
   framework-generated request UUID, anything).
2. **Active — SDK-based propagation.** `weft-go`/`weft-py`
   (`docs/completed/go-sdk.md`/`python-sdk.md`) already ship
   `weft.Send`/`weft.send` from inside customer application code. This
   plan adds ambient trace context on top: a context-carried trace ID
   (Go's `context.Context`, Python's `contextvars.ContextVar` — both are
   each language's standard mechanism for carrying a value through a
   call chain without threading it through every function signature)
   that's generated or adopted at request entry, auto-attached to every
   `weft.Send`/`weft.send` call, and available for propagation to
   downstream services.

Mechanism 1 requires nothing from the customer; mechanism 2 requires
adopting the SDK but gives active propagation help, not just passive
correlation. They're additive, not alternatives.

**This also resolves weft's own internal tracing.** Weft's own binaries
(coordinator, shard, compute, weftd) already use `weft-go` for their own
runtime telemetry. Instrumenting weft's own `api` → `coordinator` →
`compute`/`shard` request path uses the *same* SDK mechanism customers
get — one design, not two, and weft's own dogfooding directly validates
the customer-facing feature.

### Propagation header: W3C `traceparent` wire format only

Reuse the format `version-trace_id-parent_id-flags` from the [W3C Trace
Context spec](https://www.w3.org/TR/trace-context/) for the header
carrying trace context between services. Important scoping point reached
during design: **the spec only standardizes the propagation wire
format** — it does not require span-tree storage, duration tracking, or
semantic-convention attribute support from a receiver. A system that
reads just the `trace_id` segment and ignores the rest is fully
compliant. Adopting the header shape is therefore a separate decision
from the data model (already scoped above to flat correlation only) —
it costs nothing extra and buys free interop with anything in a
customer's infrastructure that already emits/forwards `traceparent` for
unrelated reasons (a CDN, an API gateway, a service mesh). `parent_id`
and `flags` are carried but not given meaningful semantics under this
design.

### Storage: `trace_id` as a reserved, indexed column

A plain-string column (not validated against any specific ID shape — it
needs to hold whatever a customer's existing logs already use, from a
UUID to a short hash). This is a deliberate, called-out exception to
`docs/plans/http-ingest.md`'s existing policy against promoting OTel-
style attributes to reserved columns — justified because the entire
value of a trace ID is exact-match point lookup, unlike general
attributes used for filtering.

### Query: `trace <id>` shell verb

A new source-tier builtin, alongside `cat`/`head`/`tail` — no `/log/...`
path argument, since a trace isn't scoped to one app. Fans out across
every app/service the customer has, rows ordered by `ts ASC`. Verified
cheap during design: `Provisioner.ShardDescriptorsFor` already returns
every shard in a time range with app-membership filtering skipped
whenever the namespace argument is empty (`logdb/registry.go`'s
`queryable()`, the `serving != nil` check) — the cross-app fan-out is a
new caller of existing shard resolution, not new plumbing. Should
respect the shell's normal default range rather than defaulting to
unbounded — a direct lesson from how `docs/plans/tail-latency-
regression.md`'s incident played out.

**Display (landed):** a bare `trace <id>` — no downstream awk/sed/jq/grep
stage — forces the planner's `includeMetaVars` projection (normally only
triggered by an explicit meta-var reference) so `NAMESPACE`/`SERVICE`/etc.
are always real, pipeline-filterable fields for trace specifically
(`planner/planner.go`). The coordinator buffers the full merged result
(`compute/local.go`'s `emitTraceTagged`), emits a one-line summary
("N line(s) across M service(s), \<first ts\> to \<last ts\>"), then each row
prefixed with a colored `[namespace/service]` badge (`theme.TraceTag`,
deterministic per-service color, disjoint from `theme.Palette()`'s
semantic colors). Once piped into a compute stage, automatic
tagging/summary stops — namespace/service metadata doesn't survive past a
bash pipeline stage, and the user already has explicit `$SERVICE`/
`$NAMESPACE` access at that point instead.

This tagging/summary is gated to interactive dispatch only
(`alertCtx == nil` in `coordinator.go`'s `executeQuery`) — it must never
reach a saved cron/watch/dashboard fire, which is the direct consequence
of the "Cron/alert-triggered queries carry no real trace ID... `trace
<id>` will only ever surface interactively-triggered queries" limitation
below: a fabricated summary row and ANSI codes in a watch's output would
corrupt `AlertCommand`'s stdin-line evaluation and pollute dashboard
tiles, not just look wrong.

### SDK surface, per language, per integration point

| Piece | Python | Go |
|---|---|---|
| Inbound (extract/generate trace_id at request entry) | primitive (`contextvars`-based) + documented snippet per framework | built-in middleware (`net/http`-compatible — works broadly since Go's middleware convention is unified across routers) |
| Logging (stamp trace_id onto regular log output, not just `weft.send()`) | built-in `logging.Filter` for stdlib `logging` | built-in `slog.Handler` wrapper for stdlib `log/slog` |
| Outbound (attach the header to a downstream call) | primitive function (`weft.trace_headers()` → dict) | primitive function (`weft.TraceHeaders(ctx)` → map) |

Logging integration is the one piece both languages get a full built-in
for — both stdlib loggers are close enough to ecosystem-standard to
justify direct support, and it's the piece that determines whether a
customer's *existing* logging calls (not just explicit `weft.send()`
calls) get correlated at all.

## Components to build

1. **Reserved `trace_id` column** — schema/DDL, planner recognition
   (same tier as `namespace`/`service`/`kind`), shard SQL WHERE-clause
   support.
2. **`ingest.Parser` extension** — `trace_id_field` as a fifth
   schema-declared extraction rule.
3. **`trace <id>` planner + coordinator support** — new source command
   recognized in the planner; new coordinator path calling
   `ShardDescriptorsFor` with no namespace/service filter, `trace_id = ?`
   in place of the usual `namespace/service/kind` predicates.
4. **weftd/proto changes** — `trace_id` field added to the `IngestLine`
   proto (weftd → coordinator) and to the SDK-to-weftd local Unix socket
   protocol, so an SDK-originated trace ID survives the pipe as a
   structured value rather than round-tripping through text extraction.
5. **`weft-go` SDK** — context-carried trace ID, `weft.Send` auto-
   tagging, `slog.Handler` wrapper, `net/http` middleware,
   `TraceHeaders(ctx)` helper.
6. **`weft-py` SDK** — `contextvars`-carried trace ID, `weft.send`
   auto-tagging, `logging.Filter`, documented per-framework middleware
   snippets (Flask/Django/FastAPI), `trace_headers()` helper.
7. **Docs** — the `traceparent` header convention (and the "this doesn't
   imply full OTel semantics" clarification, since it's a natural point
   of confusion), `trace_id_field` schema config, SDK usage per
   language.

## Deferred, not precluded

- **Span-like fields** (`duration`, a parent identifier) aren't
  architecturally ruled out by this design — nothing stops adding
  `duration_field`/`parent_id_field` siblings to the same schema-
  extraction pattern later, for customers whose own logs already carry
  that structure (paired start/end lines, an existing parent-request
  field). It's opt-in and conditional rather than a guaranteed byproduct
  of ingest the way real span data would be — not part of this plan.
  **Update**: a real `Span` type has since landed in `weft-go` (v0.4.0)
  for weft's own dogfooding, independent of this bullet's original
  customer-log-extraction framing — see `docs/plans/span-display.md`,
  which picks up exactly this hook to promote `span_name`/`span_id`/
  `parent_span_id`/`duration_ns` to reserved columns.
- **Sampling / volume control** — no design here. Real prior art exists
  (head-based vs. tail-based sampling) to draw on if/when volume becomes
  a real constraint; likely a smaller concern than under a span-based
  design since this is a new field on existing ingest, not a new
  telemetry stream.
- **A Ruby SDK.** Not scoped in this plan, but flagged explicitly:
  Ruby/Rails is a common enough language for weft's likely customer
  profile that a `weft-go`/`weft-py`-equivalent `weft-rb` will very
  plausibly be needed before this feature is complete in practice — a
  customer running a Rails service has no way to participate in active
  propagation (mechanism 2) without one, only passive correlation
  (mechanism 1, which still works via `trace_id_field` on their existing
  logs regardless). Revisit once Go/Python SDK support has shipped and
  there's a concrete customer asking for it, rather than building three
  languages simultaneously.
- **Weft's own self-monitoring isn't trace_id-aware yet.** The internal
  dogfooding pass (weft's own api→coordinator→compute→shard/cold-shard
  request path, instrumented with the same weft-go SDK mechanism
  customers get) makes weft's own logs carry `trace_id`, but nothing
  wires that into `trace <id>` queryability — that requires first
  understanding how weft's existing self-monitoring log ingestion is
  configured (referenced in `docs/plans/tail-latency-regression.md`),
  which wasn't scoped as part of landing the propagation mechanism
  itself. Until that lands, the trace ID is visible in raw logs
  (CloudWatch, SSM) but not queryable through weft itself.
- **Cron/alert-triggered queries carry no real trace ID.** `Coordinator.
  ExecuteForWatch` takes a `context.Context` for signature consistency
  with the rest of the internally-traced call chain, but its one call
  site (`coordinator/cron.go`) passes `context.Background()` — a watch
  firing has no inbound request to propagate from, and minting a
  synthetic trace ID per firing (to make alert-triggered activity
  traceable too) is a design decision on its own, not a byproduct of
  this plan. `trace <id>` will only ever surface interactively-triggered
  queries until that's revisited.
- **Live-shard query logging stays trace-gated; cold-shard logging
  doesn't.** A cold-shard (Lambda) query always logs its completion
  (shard id, duration, row count), `trace_id` appended only when
  present — archived-data queries are the rarer, costlier path most
  likely to explain hidden latency, so unconditional logging there
  doesn't reintroduce log spam. A live-shard query logs only when a
  trace ID is actually ambient, since nearly every query touches a live
  shard and unconditional per-query logging there would be real spam.
  This asymmetry is deliberate, not an oversight — revisit only if real
  usage data argues for it.

## Cross-links

- `docs/plans/tail-latency-regression.md` — the originating incident.
- `docs/plans/http-ingest.md` — the adapter-pattern precedent this
  follows, and the reserved-column policy `trace_id` is a deliberate
  exception to.
- `docs/plans/weftd-client-side-parsing.md` — the footprint benchmark
  that ruled out an OTel-Collector-shaped architecture, and the
  `ingest.Parser` extraction pattern `trace_id_field` extends.
- `docs/plans/span-display.md` — builds on the now-landed `weft-go`
  `Span` type to roll up and aggregate span data in `trace <id>`,
  picking up this doc's "Span-like fields" deferred hook above.
- `docs/completed/go-sdk.md`, `docs/completed/python-sdk.md` — the
  existing SDKs this plan extends.
