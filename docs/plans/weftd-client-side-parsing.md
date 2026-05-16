# Plan: weftd client-side schema parsing

## Status

**Shipped; default-on.** The full design is implemented and is now the
default path: `WEFTD_PARSE_CLIENT_SIDE` is an opt-*out* — set `=0` to
fall back to raw + Fields only as a fleet rollback lever. The flag is
retained (not yet removed) solely as that lever; remove it and move
this plan to `docs/completed/` once the fleet has run default-on
without incident. In code: `weftd/parser_cache.go` (`ParserCache`/
`ParserSlot`, hash-based refresh, `ingest.NewParser` reuse, OK/
missing-level counters); `schema_hash` on `GetSourceStatusResponse`
(`proto/weftpb/weft.proto`), with the coordinator computing the
SHA-256 (`coordinator/service/server.go`); `runSchemaRefresher` on a
60s ticker in `cmd/weftd/main.go`; `weftd/parse_bench_test.go`'s
`BenchmarkWeftdParse`. This closed the gap that left `$LEVEL` and
parsed `$TS` broken for the weftd path even though the schema YAML is
already persisted on `log_groups` and returned to the agent on
onboarding. CloudWatch (poller) and the offline `weft ingest` CLI
already consumed the schema via `ingest.Parser`; weftd now does too.

When the flag is removed, move this plan to `docs/completed/`.

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

The four fields the schema today declares that drive content-derived
extraction in `ingest.Parser`:

- `timestamp_field` + `timestamp_format` — pull `ts_unix_ns` from a
  positional whitespace field or a JSON dot-path; fall back to
  ingestion time when unset.
- `level_field` + `level_pattern` — pull the level into the `Level`
  column; honour the optional regex extractor (e.g. Celery's
  `INFO/MainProcess` → `INFO`).
- `field_separator` — non-whitespace separators (CSV, pipe, etc.).
- `json_start_field` (prefix_json mode) — parse JSON from a known
  positional offset; `payload_json` ships as bytes on the `LogRow`.

These are the same four `ingest.Parser` already implements for the
CloudWatch + offline-CLI paths. **Reuse, don't fork.** Parser stays
in `ingest/` for now; a future plan can relocate it once the
CloudWatch Lambda forwarder removes the last server-side caller.

`expand_keys` is intentionally **not** in scope. It is declared in
the schema and round-tripped through the API but is currently inert
across the entire codebase — no DDL materializes the columns, no
parser fills them, no planner pushdown reads them. Wiring it up is a
separate plan that owns per-app DDL, schema migrations, and the
planner-side recognition. Listing it here would imply weftd changes
unblock pushdown, which they do not.

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

No new RPC. weftd already calls `GetSourceStatus` during onboarding
(`weftd/onboard.go:26-37`) and the response already carries
`schema_yaml`. We extend the existing response with a `schema_hash`
field (SHA-256 of the YAML bytes) and keep the loop running after
onboarding completes:

1. Onboarding finishes → first parser is built from the YAML the
   agent already received.
2. A 60s ticker per `(customer, app)` calls `GetSourceStatus`. If
   `schema_hash` matches the cached parser's hash, no-op. If it
   differs, build a fresh parser and atomically swap the pointer.
3. On parse failure (malformed line vs schema, missing-but-declared
   level field, etc.), increment a per-app counter and ship the row
   with `Level=""` / ingestion-time `ts` rather than dropping it.
   The agent never silently discards data because of schema drift.

A 60s ceiling on hot-reload is short enough to feel responsive while
a customer is editing a regex in the dashboard, and the per-app
round-trip is one unary RPC every 60s — negligible against the
streaming ingest the agent is already doing. If onboarding telemetry
shows operators want sub-second propagation, a "schemas changed"
push event is additive (one new field on `IngestStreamResponse` or a
streaming notification RPC); the cache shape doesn't change.

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
don't recompile per row. Schema swap on hash change creates a fresh
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
   *Parser` in weftd; build at onboarding completion, refresh only
   when `schema_hash` changes.
2. **Compiled regex only.** No `regexp.MatchString` per row; pre-
   compile in `NewParser` (already the case in `ingest/parse.go:24-30`).
3. **No allocations inside the hot loop where avoidable.** Reuse the
   `[]string` buffer for `strings.Fields`; reuse a single
   `LogRow` builder via a sync.Pool keyed by app.
4. **Bench-gated.** A `go test -bench BenchmarkWeftdParse` runs at PR
   time over a 50k-line sample (api/worker/edge/orders/billing/
   notifier shapes) and asserts:
   - `B/op < 600` per row (one allocation budget for fields slice +
     LogRow).
   - `ns/op < 4500` per row → ≥ 220k rows/sec → ≥ 47 MB/sec at
     ~216-byte rows. That's 9× our target throughput; gives us
     headroom for the field of customers running unusual workloads.

RSS is **not** gated in CI. Process RSS measured inside `go test`
includes the test binary, the testing runner, and bench-only
allocations — it is not a faithful proxy for the deployed weftd's
working set. The fleet-week telemetry gate in rollout step 4 is the
real RSS measurement.

## Failure modes

- **Coordinator unreachable at startup.** Onboarding's existing
  `WaitForConfiguration` loop (`weftd/onboard.go:95-112`) keeps
  retrying with a 5s tick; until the schema arrives, the file
  tailer hasn't started so there's nothing to parse. No data loss
  — same posture as today.
- **Refresh tick fails post-onboarding.** Agent keeps using the
  cached parser from the last successful fetch. Logs the error and
  retries on the next tick. Customer's queries continue working with
  the schema they had; new schema changes just take longer to
  propagate.
- **Schema changes mid-tail.** Next refresh tick picks up the new
  hash; the swap is per-app atomic. In-flight rows of the old
  parser ship with old fields. Acceptable.
- **Schema declares a field weftd can't satisfy** (e.g.
  `level_field: "5"` but the line only has 3 fields). The parser
  skips that field and continues — the row ships with `Level=""`.
  Counter increments. No crash, no drop.
- **Customer ships agents older than the coordinator.** The
  `schema_hash` field is added to the existing
  `GetSourceStatusResponse`, never retired. Old agents keep working
  in the no-schema fallback path they had before the upgrade. New
  extraction features only kick in for fleets that upgrade — same
  posture as every other agent vendor.
- **High-volume parse failures stay invisible.** A schema with the
  wrong `level_field` would silently produce 100% empty levels.
  weftd emits a periodic `[<app>] parse stats: N ok, M missing-level
  (P%)` log line so a misconfigured schema is greppable from the
  agent host without dashboard plumbing. Surfacing the same counter
  in `weft sources status` is a follow-up.

## Rollout

1. Land the proto change (`schema_hash` on `GetSourceStatusResponse`)
   and coordinator hash computation. Existing weftd unaffected
   because it ignores the new field.
2. Land the parser-cache + 60s refresh + parse-stats logging in
   weftd behind a feature flag (`WEFTD_PARSE_CLIENT_SIDE=1`). CI
   runs the bench gate.
3. Flip the demo's twelve agents on, re-run the multi-customer
   verification, confirm `awk '$LEVEL == "ERROR"'` returns the
   plausible 5-15% of rows depending on `ERROR_RATIO`.
4. Flip on by default after one fleet-week of telemetry confirming
   the RSS budget (≤ 80 MiB) holds under real workloads.
5. Remove the flag.

## Test plan

- **Unit (weftd):** parser-cache hash-based no-op refresh;
  parser-swap atomicity under concurrent line ingestion; malformed-
  line counter increments and the periodic stats line emits.
- **Unit (coordinator):** `GetSourceStatus` returns a stable
  `schema_hash` for unchanged YAML and a different hash after
  `ConfigureLogGroup` writes new YAML.
- **Integration:** the multi-customer demo (already in
  `dev/demo/`) is the on-host happy path; `awk '$LEVEL ==
  "ERROR"'` and `jq 'select(._level == "ERROR")'` both return non-
  zero counts after 30s of generator output.
- **Benchmark gate:** as above, in CI.

## Open questions

- **What happens when an app has zero rows but a schema exists?** No
  parser is built (nothing to parse). The fetch already happened at
  onboarding, so the YAML is in hand — first-row latency is just
  the parser construction. Acceptable.
- **Sub-60s hot-reload?** A 60s ceiling is the v1 ceiling. If the
  dashboard's live-edit UX shows operators want faster, we add a
  push notification (one new field on `IngestStreamResponse`
  signalling "your schema changed, refetch") rather than tightening
  the polling interval. The cache shape doesn't change.
