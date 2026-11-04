# Custom tags: user-defined key/value dimensions on incoming rows

> **Status (2026-11-04): design settled in an interview with Paul; nothing built. No open decisions; the "Still to verify" list is what remains before building.**
> Related: `field-types.md` (the
> async classifier this reuses), `field-row-spec.md` (how shards send compute only
> the fields it reads), `schema-rollout.md` (the schema the wizard configures),
> `query-limits.md`, `cost-and-pricing.md`.

## Problem

A row carries a fixed set of dimensions: namespace, service, kind, env, level,
instance, deployment, version, region, cluster, k8s namespace, pod, container, node,
and the trace/span values. A customer cannot add their own: `team`, `customer_plan`,
`build`, `tenant`. The only "tags" in the product today are cron output tags (50 per
run on Swatch, 100 on Yard), which describe a cron's *result*, not an incoming row.
Every competitor in the pricing comparison has this. It is the largest missing
feature in the offering.

## Goals and non-goals

Goals: a customer can mark a value in a sample line (a JSON field or a delimited
chunk of text, exactly as they do for trace ID today) and have it stored on every
matching row as a tag, filterable and readable from the shell.

Not in v1 (decided):

- **Tags set by an SDK per event.** The wire and `weft-go` change comes later.
- **Retro-tagging.** A new or changed rule applies to rows ingested afterwards; older
  rows, in hot shards and archives, read as no tag.
- **Tags as an access boundary.** No role or token is scoped by tag. Row-level
  security stays at the org (`customer_id`), so a tag can never be mistaken for one.
- **Typed tags.** Values are strings. A number can be added later without
  rewriting stored data.
- **A wizard flow for static tags.** A tag that is the same on every row from a
  source is set through the schema-configure API and its CLI. The wizard's job is
  extraction from the line.

## Decisions (Paul, 2026-11-04)

| Question | Decision |
|---|---|
| Value type | Strings only |
| Storage | One `tags MAP(VARCHAR,VARCHAR)` column on `logs` |
| Billing | Tag bytes do not count toward included ingest GB; the raw line does |
| Sources in v1 | Extraction from the line (JSON path or positional field); static tags via the configure API |
| Query surface | awk builtin `tag("team")` (changed 2026-11-04 from `TAGS["team"]`: the awk interpreter takes only scalar variables), jq `$tags.team` |
| Where extraction runs | The coordinator, for every source; not only the weftd agent |
| Old archives | Detect per archive whether it has `tags`; substitute NULL when it doesn't |
| Over-limit record | The existing ingestion errors view, with a new `kind` |
| Unknown tag in a query | Fails before dispatch when no shard in range has seen the key; carried on the existing field-fact heartbeat rails (Design 9). The brief stale window after a key first appears is accepted |
| Caps | Same on every plan |
| Over a limit | Keep the row, drop the extra tags, record an ingest error, send a system notification |
| Conflicts | Extracted beats static; built-in names are reserved |
| Tags per row | 5 (measured: +6-18% hot storage, ~280 ms per 5M rows to filter; 20 tags cost +32-63% and ~1 s) |
| Distinct values | 5k per key, 50k total per customer per shard; soft enforcement (below) |
| Shared shards | Per-customer budgets, plus an operator alert when a shard's total passes about 150k |
| Extraction grammar | The existing one, with arbitrary names. Same code path as trace ID, not a parallel one |
| Where users configure | The existing LLM-assisted schema wizard (web and CLI); the schema YAML is not exposed |
| Wizard suggestions | The wizard proposes candidate tags from the sample |
| Key names | Identifier-style, because `-` breaks jq (`$tags.my-key` parses as subtraction) |
| Existing data | New rows only |

## Why the cardinality limit is 5k per key

Measured with the `duckdb` CLI (1.5.2) on 5M rows of ~155-byte lines plus a map of
five string tags; all keys at the same cardinality. Script: `bench/tags/cardinality.py`.

| Distinct values per key | Parquet (zstd) | Hot DB | Tag filter, all threads | Tag filter, 1 thread | Group by tag |
|---|---|---|---|---|---|
| no tags | 351 MB | 362 MB | n/a | n/a | n/a |
| 10 | 351 | 384 | 276 ms | 1,378 ms | 308 ms |
| 1k | 352 | 405 | 292 | 1,390 | 304 |
| 3k | 354 | 416 | 284 | 1,399 | 314 |
| 10k | 360 | 428 | 291 | 1,427 | 318 |
| 30k | 369 | 448 | 493 | 2,463 | 620 |
| 100k | 417 | 449 | 589 | 3,056 | 1,015 |
| 5M (unique) | 426 | 479 | 606 | 2,994 | 800 |
| mix 20/3/8/4/500 | 352 | 406 | 283 | 1,398 | 301 |

- The knee is between 10k and 30k distinct values per key, about 50k to 150k across
  the column. Below it, Parquet grows by under 3% and filters are flat; above it,
  filters are about 2x slower, group-by 2-3x, Parquet up 5-19%.
- **All keys share one value column**, so the limit that matters is total distinct
  values, not per key. 5k per key and 50k total keeps a shard at or below the last
  measured point with no penalty (5 keys at 10k = 50k).
- A tag filter cannot prune row groups: there is no zone map on map values, so it
  always reads the tag columns (about half the cost of a regex over the raw line).
- With 5 tags per row the hot database grows 6% at 10 distinct values per key, 12% at
  1k and 18% at 10k (362 MB to 384, 405 and 428 MB).
- **The number of tags per row matters more than their cardinality.** Same method,
  20 tags per row:

  | 20 tags, distinct values per key | Parquet | Hot DB | Tag filter, all threads | Tag filter, 1 thread | Group by tag |
  |---|---|---|---|---|---|
  | 10 | 351 MB | 479 MB (+32%) | 903 ms | 4,965 ms | 1,059 ms |
  | 1k | 352 | 555 (+53%) | 1,032 | 4,631 | 1,008 |
  | 2.5k (50k total) | 356 | 590 (+63%) | 1,042 | 4,587 | 1,036 |

  Parquet barely moves, but the hot database grows 32-63% and a tag filter is about
  3x slower than with 5 tags, and slower than a regex over the raw line (475 ms on
  the same data). A high cap protects storage in archives, not hot shards or query
  time. That is why the per-row cap is 5.

## Design

### 1. Data model and limits

A row has `tags: map<string,string>`.

- **Count:** at most 5 per row (decided; 20 tags cost +32-63% hot storage and ~1 s to
  filter 5M rows, against +6-18% and ~280 ms for 5). Raising it later is a
  config change; lowering it would orphan existing rules. **Key:** `[a-z][a-z0-9_]{0,63}`, lowercased on entry.
  No dashes or dots (jq `.a.b` and `.a-b` both mean something else). The wizard
  suggests a normalized name when a JSON key like `user-plan` is marked.
  **Value:** at most 256 bytes. An empty extracted value is no tag.
- **Reserved names** are rejected when the rule is saved: `namespace`, `service`,
  `kind`, `env`, `level`, `instance`, `host`, `deployment`, `version`, `region`,
  `cluster`, `k8s_namespace`, `pod`, `container`, `node`, `trace_id`, `span_name`,
  `span_id`, `parent_span_id`, `duration_ns`, `ts`, `raw`, `fields`, `payload`,
  `customer_id`, `event_id`, `line_no`, `tags`. The list comes from the same table
  the meta-variables use (`planner/jq.go`'s `metaVarTable`), so a new column cannot
  silently become shadowable.
- **Precedence:** extracted beats static when both give a non-empty value.
- **Over the count limit:** the row is kept; rules past the 5th (by rule order) are
  not applied. The wizard refuses a sixth rule at configuration time, so this only
  happens to a schema stored before the limit or edited around the wizard.

### 2. Distinct-value caps (soft)

Per `field-types.md`, a classifier runs beside the append and never inside it.

- It tracks, per shard and **per customer**, a set of 64-bit hashes of tag values
  for each key. Worst case is 8 B x 5k x 5 keys = 200 KB per customer per shard.
- At 5k distinct values for a key, or 50k across the customer's keys, the key is
  flagged `CAPPED` for that shard. Ingest then stops adding **new** values for the
  key (existing values keep tagging); the row is kept.
- A burst can overshoot by a batch or two before the flag lands. Accepted, as in
  `field-types.md`: no undo, no recompute.
- **Shared shards.** Shards hold several customers, and the performance knee
  is about the whole column, not one customer. Per-customer budgets stop one customer
  starving another, but ten customers at 50k would be 500k, past the knee. Decided:
  per-customer caps, plus a shard-wide gauge that alerts operators when a shard's
  total passes about 150k. With few customers per shard it should rarely trip, and
  the alert says when to revisit the rule.

### 3. Extraction: one code path

Today `ingest/extract.go` reads a fixed list of values (`valueID`: timestamp, level,
trace_id, span_*, duration_ns). Each is `<name>_field` (1-based index, negative from
the end, or JSON path), an optional `<name>_pattern`, and an `ExtractSpec`
(prefix, suffix, chars, cut_at, fallback). `compileValue` builds a `compiledValue`
from that triple, and `FieldSpans` records where in the line each value came from so
the UI highlights exactly what the parser read.

**Where it runs: the coordinator, for every source.** Schema-driven parsing exists only
in the weftd agent today (`weftd/parser_cache.go`; the coordinator has no row-parsing
path, and CloudWatch, syslog and SDK rows are not parsed against a schema). Tags
are extracted in the coordinator, where each row is already validated and stamped,
so every sender gets them and an agent that predates tags stops mattering. The agent
keeps parsing the original values client-side; tags are the coordinator's. The
coordinator loads the same schema the agent does (`GetSourceStatus` schema hash) and
caches one compiled parser per source. Cost: about 0.3-0.8 us per row for a handful of
values (`ingest` benchmarks), i.e. roughly 1-2% of a core at 10k rows/s; 20 regex
rules per row are not measured and would be several times that.

Tags reuse all of it:

- The schema gains a `tags` list; each entry is a name plus the same
  field/pattern/`ExtractSpec` triple. The stored shape is an internal detail since
  the YAML is not exposed, but it should keep one value per rule so the existing
  checker and the "stripped of exactly what its spec says" rule apply unchanged.
- `valueID` and `FieldSpans` become open-ended: a tag takes a slot after the fixed
  ones. `compileValue` and `compiledValue.extract` are not duplicated; the tag
  path calls them. A test must fail if anyone forks them.
- **Static tags** are schema entries with a constant value and no field. They are
  set through the schema-configure API and its CLI flag, not the wizard.
- The legacy extractors in `parse.go` stay for the original values. Tags always take
  the span-reporting path (`Extract` spec present), so there is no "legacy tag".

### 4. The wizard

The probe machinery (`ingest/probe.go`) is keyed by a fixed `ProbeField`. Tags add a
parameterised field, `tag:<name>`, so:

- The user marks the value on sample lines and names the tag. The probe returns an
  extractor exactly as it does for `trace_id`, with the built-in search first, the
  LLM proposer second, and the real parser verifying every candidate.
- **Suggestions:** the existing `Propose` mode is extended so the model looks at the
  sample and returns candidate tags (name, extractor). Each is verified by the real
  parser against the sample before it is shown, as every proposal is today.
- `ProbeResult.SchemaPatch` already returns the keys to merge into the configure
  request; tags return a `tags` entry instead of `*_field` keys.
- The preview shows the extracted value per sample line, the number of distinct
  values in the sample, and a warning if a value looks high-cardinality (every
  line unique). The sample is small, so this is a hint, not a guarantee.
- Web and CLI wizard (`cmd/weft/setup`, `api/sources_probe.go`) share this API.

### 5. Wire, shard and archives

- `LogRow` gains `map<string,string> tags`. It is additive: old receivers ignore it
  and stay correct.
- The coordinator is **authoritative** (as with namespace): it validates names,
  counts and sizes whatever the sender did, and drops violators.
- Shard: `ALTER TABLE logs ADD COLUMN IF NOT EXISTS tags MAP(VARCHAR, VARCHAR)` in
  `migrationSteps`, no backfill. Existing rows are NULL.
- **Old archives.** `ParquetReadExpr` has no `union_by_name`, so a query that names
  `tags` over an archive without the column fails. Each shard's archive is uniform
  (one shard wrote all its files), so the fix is per archive: the cold-shard Lambda
  reads the schema of one file (a metadata read) and, when `tags` is absent, rewrites
  tag references to `NULL` before running the query. Archives that have the column
  pay nothing. Hot shards get the column from the migration. `union_by_name` was
  rejected because it makes every cold query reconcile every file's footer.
- Parquet export carries the column as is; dictionary encoding does the compaction.

### 6. Query surface

- awk: the builtin `tag("team")`. A missing key returns the empty string. jq:
  `$tags.team`; a missing key reads as `null`. (An awk array was the first choice,
  `TAGS["team"]`, but the awk interpreter accepts only scalar variables
  (`SetVar(name, value string)`); a builtin needs no array support.)
- Planner: `metaVarTable` maps a name to a column. A tag is a map lookup, so it needs
  its own case: `tag("k") == "v"` lowers to `tags['k'] = 'v'`, and `tag("k") == ""`
  lowers to `COALESCE(tags['k'], '') = ''` so an absent key matches, as in awk.
  Regex and `!=` follow the existing meta-variable lowerings. `tag()` with a
  non-constant argument does not lower and runs in compute.
- The shell interpreter ("warp") is shared with another product and must stay
  weft-agnostic, so the builtin cannot be called `tag`. It needs a generic way for
  the embedder to register a native function that reads the current record (here, the
  row's tags), which weft then registers as `tag`. **Verify first**: the record
  context a native function sees, and whether jq can bind `$tags` as an object the
  same way it binds `$level`.
- Compute transport: tags go to compute only when the pipeline references `tag(` or
  `$tags`, the same way `field-row-spec.md` sends only the fields a stage reads.
- Group-by on a tag (`awk '{c[tag("team")]++}'`) is the keyed-aggregate work in
  `query-limits.md`; it comes free once that lowers, and is not a v1 dependency.
- Line scan budget: a tag filter reads the same rows, so it adds nothing to rows
  scanned.
- Docs: `docs/manual` and `docs/shell` pages for meta-variables, the wizard, and
  `man explain`'s advice for tag predicates.

### 7. Over-limit events and notifications

An over-limit event is: tag count dropped, key or value rejected, key `CAPPED`.

- Each is recorded in the existing ingestion errors view with a new `kind`
  (`tag_limit`), carrying the source, tag key and which limit. The recorder
  (`coordinator/ingestion_errors.go`) is built for failed batches today, with shard
  id, row count and dead-letter path, so it needs a kind column (a store migration)
  and a tag-event constructor; the table's other fields stay empty for these.
- It also pages the org's reserved **`system` alias** (`store.SystemAlias` via
  `Notifier.Send`, as `CronExecutor.pageSystem` does for cron failures). The shard
  process has no `Notifier`, so the shard reports the event and the coordinator
  sends. Throttle: once per (customer, key, limit kind) per shard, so a busy source
  cannot flood the alias. An org without the alias gets nothing, as today.

### 8. Billing and the cost model

- Metered ingest stays the raw line (verified: `len(row.Raw)` in `coordinator/drain.go`).
- Cost model (`scripts/costmodel`): the hot ratio (1.07) rises by 6-18% with 5 tags
  per row (about 12% at 1k distinct values per key), which moves the hot $/GB by the
  same factor; the Parquet ratio is unchanged at or below 10k distinct values per
  key. With the cap at 5, `hot_ratio` should be about 1.2 for a tagged customer.

### 9. Seen tags: failing a query on a tag that does not exist

A scan for a key no row has costs the same as one for a key that exists (measured:
~300 ms per 5M rows with 5 tags per row either way, because DuckDB has no per-key
statistics for a map column). Telling the user instead is both cheaper and kinder,
and it rides the rails the field facts in `field-types.md` already built.

**Fact.** Per shard, per source key (customer, namespace, service, kind, env): the set
of tag keys any row has carried. It only grows, so deltas are additions.

**Reporting (unchanged mechanism, new payload).** The classifier that counts distinct
values (Design 2) already sees every tag key, so it records first sight at no extra
cost. `ShardHeartbeatRequest` gains a repeated delta of newly seen keys, each with an
epoch. The coordinator folds it into the shard's registry entry
(`FoldFieldFacts`) and writes it through to Postgres (`ApplyShardFieldFacts`), the
shard resends until `HeartbeatResponse` echoes the epoch, sealed shards persist
their final set, live shards rebuild on open, and the entry leaves with its shard.
Old shards send nothing and old coordinators drop the field; both read as
"unknown", which is today's behavior, so there is no rollout ordering.

**Dispatch check.** The planner stays pure: it returns the constant keys a pipeline
names (`Plan.TagKeys`, from `tag("k")` and `$tags.k`). In `executeQuery`, which
already holds the shard descriptors, the coordinator fails the query **before
dispatch** when every shard in range has a known key set and none contains the key.
Rules:

- A shard whose set is **unknown** (old shard, facts not yet rebuilt) blocks the
  failure: the query runs. Shards created before the `tags` column existed are known
  empty by construction, so the first tagged query on an old source is not blocked
  by history.
- Several sources (a glob over envs or services): fail only if the key is missing
  from **all** of them. Sources without it read as NULL.
- The time range is not part of the check: a key seen only last week returns zero
  rows yesterday, not an error.
- The error names the source and lists the keys it knows:
  `tag "sandwich" has not been seen on api/main (known: region, team)`.
- Crons and alerts that hit this fail like other runtime errors: the cron is
  disabled and the `system` alias is paged. Document it.

**Accepted staleness.** After a key first appears, the coordinator learns of it at
the next heartbeat. A query in that window can fail wrongly. This includes the gap
between saving a rule and the first ingested row that carries the tag: a rule is
only ever created from a value marked on a sample line, and a static tag from the
configure API, so there is no "defined but never seen" state to special-case. Accepted (Paul,
2026-11-04), the same trade the typed-field facts make in the other direction.

**Skipping shards (optional, same facts).** For a predicate that cannot match
without the key (`tag("k") == "v"` for a non-empty `v`, or a regex that needs a
character), a shard known not to have the key is provably empty for it and need not
be dispatched. For a cold shard that saves the Lambda invocation and its fixed
per-call cost (~0.75 s billed). Forms where an absent key matches (`== ""`, `!=`)
are never skipped. Not a partial result: the skipped shard has no matching row.

## Rollout

Deploy order is coordinator, compute, then shard (additive wire fields; old
receivers stay correct).

1. **Storage and wire, no UI.** `LogRow.tags`, column migration, coordinator
   validation, per-archive NULL substitution in the cold-shard Lambda. Tags can only
   come from tests and the API.
2. **Extraction.** Generalise `valueID`/`FieldSpans`, add the `tags` rules to
   the parser, run it in the coordinator for every source, differential test against
   the existing fixed values (a tag rule shaped like trace_id extracts what trace_id
   extracts).
3. **Query surface.** The shell interpreter's native-function hook (ships first and
   stays weft-agnostic), the `tag()` builtin registration, planner lowering, jq
   `$tags`, compute transport, differential harness cases (below).
4. **Caps and notifications.** Classifier, `CAPPED` flag, ingest errors, system alias.
   **Seen-tag facts** ride the same classifier: the heartbeat delta, registry fold,
   Postgres write-through, then the dispatch check. Shard skipping follows once the
   facts are trusted.
5. **Wizard.** `tag:<name>` probe field, candidate proposals, preview, web and CLI.
6. **Static tags via the configure API and CLI, docs, pricing page.**

**weftd:** no release is needed. Tags are extracted in the coordinator, so agents that
predate tags are unaffected. (Agents parse client-side by default, with a 60-second
schema refresh; they ignore a `tags` entry in the schema they receive, because the
YAML decode is not strict. A test must pin that.)

## Tests

- Ingest: table-driven extraction for text and JSON, negative indexes, patterns and
  `ExtractSpec`; reserved names; count, key and value limits; empty value.
- Differential (`planner/difftest`): `tag("k")` predicates through the real plan and
  the all-compute reference, over rows with the tag, without it, with an empty
  value, and over multi-line rows, byte-identical.
- Shard: migration idempotence, a legacy archive without the column, mixed layouts.
- Classifier: `CAPPED` at 5k per key and 50k total, existing values still tag.
- Seen-tag facts: delta folded and acked; lost delta resent; restart reloads from
  Postgres; unknown shard never blocks; all-known-and-absent fails before dispatch
  with the known keys listed; a glob with the key on one source runs; skipping never
  applies to `== ""` or `!=`.
- Notification: one page per (customer, key, limit) per shard.
- Wizard: probe round-trip for a named tag; high-cardinality warning.

## Verified (2026-11-04)

- **Billing:** metered ingest is `len(row.Raw)` (`coordinator/drain.go`), so tag bytes
  cannot enter `usage_hourly`. A test should pin it.
- **Old archives:** no `union_by_name` today; handled per archive (Design 5).
- **Awk:** the interpreter takes scalar variables only; hence the `tag()` builtin.
- **Ingest errors:** the recorder is for failed batches; it needs a `kind` (Design 7).
- **Parsing sites:** schema-driven parsing exists only in weftd; the coordinator
  extracts tags for all sources (Design 3).
- **20 tags per row:** measured (benchmark section); Parquet barely changes, hot DB
  +32-63%, tag filter ~3x slower than 5 tags.

## Still to verify

1. The record context a native function sees in the shell interpreter, and whether
   jq can bind `$tags` as an object like `$level`.
2. Coordinator CPU for 20 regex tag rules per row at production row rates.
3. How the cold-shard Lambda reads one file's schema cheaply, and its cost per cold
   query (the file listing is already pruned by partition).
4. The ingestion errors table migration and what customers see of it today.
5. Whether 5 tags with 5k values per key at production row rates stays under the
   knee on real shards (the benchmark is synthetic and per file).
6. Where the shard field-fact tables land in the cells and DynamoDB split
   (`cells-dynamo-rollout.md`): they hang off the shard registry, which is moving.
7. What the planner reports for a non-constant tag argument (`tag($1)` does not
   lower and cannot be checked, so it runs and is never failed by Design 9).

## Risks

- A tag rule that is high-cardinality by mistake (a request id) slows every query on
  the shard before the cap lands. Mitigations: the preview warning, the soft cap, the
  system page.
- Coordinator-side extraction puts per-row regex work on the ingest path. It must
  stay off the append (it runs before it, like validation) and have a benchmark gate.
- A reserved-name list that drifts from the real columns. Mitigation: derive it from
  the one table.
- Planner equivalence for absent tags (`NULL` against `""`) is the subtle part; the
  differential cases above exist to pin it.
