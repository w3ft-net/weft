# Plan: Ingest governance rules

Per-customer rules evaluated before each row hits a shard. Three actions: `drop`, `sample N%`, `redact <regex>`. Cost and PII controls for a single-tenant log shop that today persists everything.

## Why add this

Two things are painful in the current model:

1. **Cost.** Every ingested row hits a shard and counts against the customer's byte allowance. There's no way to say "drop DEBUG rows from `api`", or "sample 1% of 200-response lines from `nginx`". Customers pay to store noise they don't want.
2. **PII.** If a credit-card-shaped substring shows up in a log line, it's persisted as-is. There's no pre-shard redaction, and retroactively masking rows on disk is a nightmare compared to never writing the raw value in the first place.

Both problems are standard "ingest pipeline" territory in every mature log SaaS (Datadog Logs Pipelines, Splunk props/transforms, Cribl Stream). We don't need anything that ambitious — an ordered rule list with three actions covers the bulk of real demand.

## Architecture

A per-customer, ordered list of rules evaluated inside the drain goroutine immediately before the row is handed to an INGESTING shard's `Ingest` RPC. First matching rule wins; `drop` short-circuits subsequent evaluation on that row.

```
Ingest entry point (weftd gRPC / HTTP-pushed adapters — CloudWatch, Heroku, syslog)
  → RingBuffer
    → StartDrain goroutine
      → for each row:
          for each rule in customer.rules (ordered):
            if rule.match(row):
              switch rule.action:
                case drop:       metering++; skip row
                case sample N%:  keep with probability N/100
                case redact rx:  row.raw = rx.ReplaceAll(row.raw, mask)
                                 row.payload_json = redactJSON(...)
              break
      → shard.Ingest(batch)
```

Rules live in a new `ingest_rules` table loaded into coordinator memory on startup; a goroutine polls for changes (or the API handler broadcasts an invalidation) and reloads.

## Rule shape

```yaml
# /etc/ingest.conf (YAML, one customer, edit from the shell or the web UI)

rules:
  - match: { app: "api", level: "DEBUG" }
    action: drop

  - match: { app: "nginx" }
    field: ".status"
    equals: 200
    action: { sample: 1 }      # keep 1% of successful nginx rows

  - match: { app: "*" }
    action:
      redact:
        pattern: '\b\d{13,16}\b'
        mask: "****"

  - match: { app: "metrics" }
    action: count
    metric: ingest.drops.metrics_debug
```

The `match` block is a conjunction of simple predicates over `app` (glob), `level` (string), and one optional `field` (JSON dot-path) predicate. Keeping match simple is intentional — if you need the full planner's expressiveness, write a watch; governance rules are supposed to be cheap per-row.

The `count` action (last example) is the "log-to-metric" adjacency I flagged in the tier-2 list — same rule engine, different terminal action. Ship the first three in v1; `count` in v2.

## Components to build

1. **`store.IngestRule`** — new model: `{customer_id, ordinal, yaml_body, created_at}`. One row per customer; the YAML is parsed into a typed struct at load time.
2. **`coordinator/ingest_rules.go`** — parsing, compile-time regex compile, `Eval(row *pb.LogRow) RuleOutcome`, hot reload.
3. **Drain hook** — single call into the rule engine from the existing drain path. Bench the overhead with zero rules (should be a nil-check). With typical 3-5 rules and compiled regex, target <1µs/row.
4. **Metering** — drop/sample/redact counts roll up into `usage_hourly` with a new `ingest_bytes_dropped` column and per-rule counters surfaced in `/etc/usage`:
   ```
   Dropped this billing cycle: 124 MB (rule: "drop api DEBUG")
                                89 MB (rule: "sample nginx 200 @ 1%")
   ```
5. **Shell integration** — `vi /etc/ingest.conf` edits the YAML; save-time validation rejects bad regex, unknown actions, ambiguous matches.
6. **Web UI** — Account → Ingest Rules: form-based editor mirroring the YAML, with a "dry run on last 1000 rows" preview so users can see what a rule would have dropped before enabling it.
7. **Audit trail** — rule edits go into a `rule_history` table (folds into the audit-log plan when that ships).
8. **Docs** — `docs/manual/ingest-rules.md` with one example per action and a "debugging a rule" section showing the dry-run preview.

## Estimated effort

- `IngestRule` model + YAML parse + validate: **0.75 day**
- Eval engine + hot reload: **0.75 day**
- Drain hook + benchmark: **0.25 day**
- Metering columns + `/etc/usage` surface: **0.5 day**
- Shell integration + save-time validation: **0.5 day**
- Web UI editor + dry-run preview: **1.5 days**
- Docs + tests: **0.5 day**

**Total: ~4.5 days** for a shippable v1 with all three actions.

## Non-goals

- **No streaming anomaly-based drop.** Rules only. If a customer wants "drop anything statistically rare", that's a product feature on top, not governance.
- **No cross-row / windowed rules.** Pure per-row evaluation; anything that needs row history belongs in a watch.
- **No rehydration of dropped rows.** Drop is drop — the row is never on disk. Customers who want "drop cheaply but keep a tail for audit" are better served by the sample action with a low percent.
- **No per-rule tiering.** All plans get the same rule engine; tier caps live on byte budgets, not on rule counts.
- **No mutation beyond redact.** No "rewrite app", "synthesize new field", etc. Keeping the action set small keeps the mental model small.
