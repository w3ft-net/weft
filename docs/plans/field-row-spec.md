# Field row spec: sending a compute stage only the fields it reads

Status: plan, 2026-10-31. Nothing here is built. It follows `field-types.md`, whose exact
plan (`QueryPlan.Typed.Exact`) already lowers an awk filter-and-print over typed fields to
SQL alone.

## Problem

An awk stage that stays in compute and reads only a few fields still receives every whole
line from the shard. `projectFields` (`planner/fieldproj.go`) shrinks that today by having
the shard build a *synthetic text record* (`a _ c` for a program that reads `$1` and `$3`).
It has three faults:

- the SQL is built as a string in the backend (`c.fieldSelect`), after the IR passes, and
  re-parses the compute stage's own text, which the IR contract forbids;
- it tokenizes `raw` with `regexp_extract_all` at every position, although the row's
  `fields` column already holds the split;
- compute then splits the text it was just handed.

## Design

The request carries a **row spec**: the positions the stage reads (`$1`, `$3`) and whether
it reads `NF` or `$NF`. The shard selects `list_extract(fields, N)` as plain columns, plus
`len(fields)` when `NF` is read, and returns them as a repeated field on the result row.
Compute rebuilds the record the stage reads: placeholders for skipped positions, the real
count for `NF`, the real last field for `$NF`. A small warp API that sets an interpreter's
fields directly would also skip `setRecord`, the largest per-row cost in the profile.

The spec is an IR node on the awk stage (produced by the frontend, gated by a pass,
rendered by the backend), so `explain` shows it and nothing re-reads a compute string.

## Which cases use it

Only awk stages that remain in compute and read a few fields: multiple rules, `printf` with
arithmetic, an `END` that uses arrays, and the keyed sum or max/min until those lower. A
program whose pattern and print both lower (`awk '$3 > 100 {print $1, $3}'` on a typed
shard) has no stage and no record. Shard-side keyed aggregates (the deferred part of
`field-types.md`) would remove most of the remaining cases, so Deploy 3 can make this work
less valuable than it looks.

## Deploy rules

Every deploy rolls **coordinator, then compute, then shards**, so at every moment a request
sender is the same age as its receiver or newer, and a row sender (the shard) is older.

- A newer sender may add fields to a request. An older receiver ignores them and does what
  it did before, which must still be correct.
- A newer receiver of rows must accept the old row form and the new one.
- A shard launched mid-rollout runs the new binary against a compute that may be old, so a
  shard may only change behavior when the request it receives asks for it.

This is the pattern `ShardDescriptor.sql` and the typed variant already use.

## Deploy 1: add

Ships together with the local commits that are not yet deployed (typed filters, exact
plan, strict-decimal classifier, warp bump, benchmarks).

- **No warp release.** Compute rebuilds the text record from the fields (`compute.RebuildRecord`);
  a warp `SetFields` API that skips `setRecord` is a later optimization.
- **Coordinator:** the planner emits the row-spec IR node and a second SQL, `field_sql`,
  that selects the field columns (no regexp). `ExecuteQueryRequest` gains `field_spec` and
  `field_sql`. The request's `sql` stays the current text-record SQL.
- **Compute:** forwards `field_spec` and `field_sql` on the shard Query call. Accepts a
  `ResultRow` with `fields` filled (rebuilds the record) or empty (uses `raw`, as now).
- **Shard:** a Query that carries `field_sql` runs it and fills `fields`; otherwise it runs
  `sql` as now.

Mixed states during the roll, each correct:

| Coordinator | Compute | Shard | Result |
|---|---|---|---|
| new | old | old | compute ignores the new fields; today's plan runs |
| new | new | old | shard ignores `field_sql`; returns text records; compute reads them as `raw` |
| new | new | new | shard runs `field_sql`; compute rebuilds the record |
| new | old | new (launched mid-roll) | the Query has no `field_sql`; shard runs `sql` |

Verification before moving on: the difftest corpus runs the stage over all three row forms
and compares with the reference stage; `BenchmarkAwkFilterPrint`-style benchmark for a
stage that stays in compute, with the shard SQL, the wire bytes and the whole query.

Rollback: revert the commit and redeploy. Nothing persisted changes.

## Gate between Deploy 1 and Deploy 2

Every shard must run Deploy 1's binary, including any left on an older AMI (last time two
did). Check `binary_sha` across `shard_records` for every non-archived shard. This is for
speed, not correctness: a shard that is still old would return whole lines, which compute
accepts.

## Deploy 2: remove

Coordinator only in effect.

- Delete `projectFields`, `syntheticRecordSQL`, `fieldSelect` and the regexp.
- `sql` becomes the plain unprojected query; `field_sql` is the only projected form.
- Update `TestFieldProjectionShrinksTheWire` to assert the new wire shape.

Rollback: revert and redeploy; Deploy 1's compute and shard still accept the old form.

## Deploy 3: shard-side aggregates (optional, separate)

Keyed sum, max and min as shard `GROUP BY`, as designed in `field-types.md`. Needs the
integer-range facts that were left out on purpose, the aggregate node, and a wire for
per-shard group stages. Decide whether to build it before Deploy 1 by counting which
pipelines still reach `projectFields`.

## Open points for Deploy 1

### 1. How the shard maps `field_sql` onto `ResultRow.fields`

`shard.ScanResultRows` (`shard/scan.go`) is positional and string-typed: column 0 is `raw`
(a non-null `string`), column 1 is `ts`, and every further column is scanned into a
`sql.NullString` and routed by name: `event_id`, `line_no`, anything else into
`ResultRow.MetaVars`. A DuckDB `LIST` column would not scan into a `NullString`, so the
fields cannot ride as one array column without changing the scan.

Proposal: keep the scan string-based and add two name conventions to its switch, so the
SQL stays plain and legible:

    SELECT '' AS raw, ts, event_id, line_no,
           list_extract(fields, 1) AS field_1,
           list_extract(fields, 3) AS field_3,
           len(fields)             AS nf
    FROM logs WHERE ...

- `field_<N>` appends to `ResultRow.fields` in column order. The request's `field_spec`
  says which positions those are, so the row carries values only. A NULL (the row has
  fewer fields) is an empty entry.
- `nf` sets `ResultRow.field_count`, and is always selected when a spec is present. Its
  presence is what tells compute "this row is in field form", so a row with no fields
  (`nf = 0`) is distinguishable from a row in raw form. That needs presence on the wire:
  `optional int32 field_count`.
- `raw` is `''` in field form, not NULL, because column 0 scans into a plain `string`.
- Ordering and the merge key are untouched: `ts`, `event_id` and `line_no` are the same
  columns as today.

New proto fields: `ResultRow.fields` (repeated string), `ResultRow.field_count`
(optional int32), `ExecuteQueryRequest.field_spec` and `.field_sql`, and the matching
fields on the shard Query request. All are additions, so older receivers skip them.

### 2. Multi-line rows

Ingest stores a multi-line text event as one row per line (`ingest.SplitLines`, called by
`ingestBatch` since 2026-10-14; `docs/query-execution.md`, "Rows are lines"). So a stored
text row is a line, `fields` is that line's split, and `list_extract(fields, N)` is awk's
`$N` for it. `TestDifferentialMultiLineEventsLoweredFields` and
`...ExactPlan` pin this: events of several lines, with blank lines, a trailing newline and
CRLF, go through `SplitLines` into the shards, and the lowered `$N` pipelines and the exact
plan match awk.

The row spec therefore needs no multi-line case. The field form is unconditional, and the
unnest (`unnest(string_split(raw, chr(10)))`) and `contains(raw, chr(10))` that an earlier
draft carried are not needed.

Rows that can still hold a newline are the residue the docs already name:

- rows ingested before 2026-10-14, whose `fields` is also the older schema-dependent split
  (`awkFields`, `docs/query-execution.md`), and so already disagree with awk on `$N`;
- a JSON line that is its own payload, which is never split (jq reads the payload once per
  event), if its `raw` contains a newline.

For these the field form is no worse than the lowered `$N` pipelines are today. It is
wrong where they are wrong, until the pre-2026-10-14 rows age out of retention. The old
projection handled them (it unnested), so the removal in Deploy 2 gives that up; check
the retention horizon before Deploy 2 and say so in its commit.
