# Plan: weftd client-side schema parsing

## Status

**Proposed.** Closes the gap that left `$LEVEL`, parsed `$TS`, and
`expand_keys` JSON-promotions broken for the weftd path even though the
schema YAML is already persisted on `log_groups`. CloudWatch (poller) and
the offline `weft ingest` CLI already consume the schema via
`ingest.Parser`; weftd does not.

## Why client-side, not server-side

Every modern log shipper does parsing in the agent — Fluent Bit,
Vector, Filebeat, Datadog Agent, Promtail/Alloy, OpenTelemetry
Collector all extract structure (regex / grok / JSON / level /
timestamp) before the wire. The exception is Splunk's Universal
Forwarder, which is positioned as the dumb-shipper sibling to a
separate Heavy Forwarder that does the work — a different competitive
posture than ours.

Doing the work in the customer's process keeps our coordinator small
(parse CPU is paid by the fleet that produced the data) and meets the
architectural shape ops teams already budget for. Server-side
enrichment ("option a" in the discussion that produced this plan)
would have moved the work onto the coordinator's hot path and is
explicitly **not** the fallback if (b) gets hard — pursuing both
simultaneously is the duplication the user wants to avoid.

### Footprint budget

From the VictoriaMetrics 2026 head-to-head log-collector benchmark at
~10k logs/sec / 2 MB/sec
([https://victoriametrics.com/blog/log-collectors-benchmark-2026/](https://victoriametrics.com/blog/log-collectors-benchmark-2026/)):

| Agent | RSS (MiB) | CPU (cores) |
|---|---|---|
| Promtail | 63 | 0.66 |
| Alloy | 66 | 0.58 |
| **Fluent Bit** | **78** | **0.26** |
| OTel Collector | 107 | 0.49 |
| Vector | 154 | 0.41 |

**Target: ≤ 80 MiB RSS / ≤ 0.30 vCPU at 5 MB/sec sustained.** This sits
weftd inside the Fluent Bit / Promtail / Alloy band and well below
Vector and OTel Collector. The numerics also rule out the architectural
choice that pushes them apart — see "no programmable transform"
below.

## What's in scope

The five fields the schema today declares for content-derived
extraction:

- `timestamp_field` + `timestamp_format` — pull `ts_unix_ns` from a
  positional whitespace field or a JSON dot-path; fall back to
  ingestion time when unset.
- `level_field` + `level_pattern` — pull the level into the `Level`
  column; honour the optional regex extractor (e.g. Celery's
  `INFO/MainProcess` → `INFO`).
- `field_separator` — non-whitespace separators (CSV, pipe, etc.).
- `json_start_field` (prefix_json mode) — parse JSON from a known
  positional offset.
- `expand_keys` — promote a fixed list of JSON keys into typed
  columns (`payload_json` already ships; this is about the columns
  the planner pushes against).

These are the same five `ingest.Parser` already implements for the
CloudWatch + offline-CLI paths. **Reuse, don't fork.**

## What's explicitly out of scope

- **No embedded transform language.** No Lua, no VRL, no JS, no jq
  preprocessing. Vector/Datadog ship those and pay 2-3× the CPU and
  ~2× the memory for the privilege; that's the architectural choice
  that splits the 80 MiB band from the 150 MiB band. weftd's parser
  stays the fixed schema-driven set — the same parsers Fluent Bit
  compiles into its binary
  ([https://docs.fluentbit.io/manual/about/fluentd-and-fluent-bit](https://docs.fluentbit.io/manual/about/fluentd-and-fluent-bit)).
  Customer extensibility lives at the *coordinator* via
  `weft source configure` editing `schema_yaml` — never as a hook the
  agent runs.
- **No coordinator-side parsing fallback.** If the agent didn't parse
  it, it doesn't get parsed. This is the duplication we're avoiding.
- **No retroactive parse of old rows.** Rows ingested before a schema
  change keep the empty `level` they had. Backfill is a separate plan
  if anyone ever asks.

## Architecture

### Schema delivery

A new gRPC: `WeftCoordinator.GetAppSchema(app)` returning the
schema_yaml string + a content hash. weftd:

1. Calls it once per `(customer, app)` at startup.
2. Refreshes on a TTL (default 5 min) — if the hash changed, swap the
   active parser atomically; if not, no-op.
3. On parse failure (malformed line vs schema), increments a counter
   and ships the row with `Level=""` / ingestion-time `ts` rather
   than dropping it. The agent never silently discards data because
   of a schema drift.

The TTL + hash give us cheap polling (one round-trip per app per 5
min) without a push channel. If a configuration UX wants
near-instant propagation later, we add a "schemas changed" gRPC
event — the cache shape doesn't change.

### Parser placement

`ingest.Parser` already does the work. weftd imports it directly:

```go
import "github.com/kevlarcon/weft/ingest"

parser := ingest.NewParser(schema)            // compile regexes once
row, _ := parser.ParseLine(line, time.Now())   // hot path
row.App = ...; row.Customer = ...; ...         // weftd-derived fields
stream.Send(row)
```

The compiled regex (`Schema.LevelPattern`) lives in the parser; we
don't recompile per row. Schema swap on TTL refresh creates a fresh
parser and atomically replaces the pointer — readers see either the
old or new parser, never a partial.

### Wire shape

LogRow protobuf doesn't change. weftd just fills more fields
(`Level`, `TsUnixNs` from the line vs ingestion time, `PayloadJSON`
for prefix_json mode). Coordinator's `IngestStream` continues to
trust the agent's structured columns — it does not re-parse, and it
does not validate that `level` matches the schema. (We accept that
a compromised agent could ship malformed rows; the threat model is
covered by token scoping, not by re-parsing.)

## Footprint controls

To stay inside ≤ 80 MiB RSS / ≤ 0.3 vCPU at 5 MB/sec, the
implementation rules:

1. **One parser per app, not per line.** Cache `(customer, app) →
   *Parser` in weftd; build at first row, refresh on TTL.
2. **Compiled regex only.** No `regexp.MatchString` per row; pre-
   compile in `NewParser`.
3. **Bounded JSON depth.** `payload_json` is shipped as bytes;
   `expand_keys` runs through `gjson.GetBytes` with a flat key list
   (no path traversal beyond two segments). gjson is already a
   dependency for the alert path.
4. **No allocations inside the hot loop where avoidable.** Reuse the
   `[]string` buffer for `strings.Fields`; reuse a single
   `LogRow` builder via a sync.Pool keyed by app.
5. **Bench-gated.** A `go test -bench BenchmarkWeftdParse` runs at PR
   time over a 50k-line sample (api/worker/edge/orders/billing/
   notifier shapes) and asserts:
   - `B/op < 600` per row (one allocation budget for fields slice +
     LogRow).
   - `ns/op < 4500` per row → ≥ 220k rows/sec → ≥ 47 MB/sec at
     ~216-byte rows. That's 9× our target throughput; gives us
     headroom for the field of customers running unusual workloads.
   - Process RSS at end of bench < 80 MiB.

## Failure modes

- **Coordinator unreachable at startup.** weftd retries the schema
  fetch with backoff; while waiting, it tails files and ships rows
  with `Level=""` (the old behavior). No data loss, just degraded
  enrichment until the coordinator answers.
- **Schema changes mid-tail.** Next refresh tick picks up the new
  hash; the swap is per-app atomic. In-flight rows of the old
  parser ship with old fields. Acceptable.
- **Schema declares a field weftd can't satisfy** (e.g.
  `level_field: "5"` but the line only has 3 fields). The parser
  skips that field and continues — the row ships with `Level=""`.
  Counter increments. No crash, no drop.
- **Customer ships agents older than the coordinator** that lack
  the schema-fetch RPC. The new RPC is added, not retired; old
  agents keep working in the no-schema fallback path the agent has
  today. New extraction features only kick in for fleets that
  upgrade — same posture as every other agent vendor.

## Rollout

1. Land the gRPC + coordinator handler. Existing weftd unaffected.
2. Land the parser-cache in weftd behind a feature flag
   (`WEFTD_PARSE_CLIENT_SIDE=1`). CI runs the bench gate.
3. Flip the demo's twelve agents on, re-run the multi-customer
   verification, confirm `awk '$LEVEL == "ERROR"'` returns the
   plausible 5-15% of rows depending on `ERROR_RATIO`.
4. Flip on by default after one fleet-week of telemetry confirming
   the RSS ceiling holds under real workloads.
5. Remove the flag.

## Test plan

- **Unit (weftd):** parser-cache TTL behaviour; hash-based no-op
  refresh; concurrent reads during swap; malformed-line counter.
- **Unit (coordinator):** `GetAppSchema` returns the right YAML for
  the right `(customer, app)`; namespace-scope enforcement; missing-
  app returns NOT_FOUND.
- **Integration:** the multi-customer demo (already in
  `dev/demo/`) is the on-host happy path; `awk '$LEVEL ==
  "ERROR"'` and `jq 'select(._level == "ERROR")'` both return non-
  zero counts after 30s of generator output.
- **Benchmark gate:** as above, in CI.

## Open questions

- **Schema fetch transport: gRPC or HTTP/2?** The agent already has a
  bidi gRPC stream open to the coordinator for ingest; adding a
  unary RPC over the same connection costs nothing. Default to gRPC.
- **What happens when an app has zero rows but a schema exists?** No
  parser is built (nothing to parse). The fetch is lazy on first
  row. Trade-off: a one-row burst takes one extra round-trip; we
  could pre-fetch on startup for declared apps if the latency
  bothers anyone. Defer.
- **Do we want hot-reload of the level regex?** Today schema changes
  go through `ConfigureLogGroup`. If a customer fixes a regex while
  watching live data, do we want it to apply within seconds rather
  than within the 5-min TTL? Probably yes for the dashboard
  flow. Add a "schemas changed" gRPC event in v2 if the TTL feels
  slow during real onboarding.
