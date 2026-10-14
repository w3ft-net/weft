# Line storage: one row per line, grouped by event

## Status

🟡 Designed 2026-10-14, not started. Decided: one stored row per line
for text events; JSON-schemaed rows are never split; no migration of
existing rows; ingest normalizes line endings.

## Problem

A log event can hold several lines: a stack trace in one CloudWatch
event, or (after [weftd-multiline](weftd-multiline.md)) a Python
traceback weftd stitched together. Today it's one row whose `raw`
contains newlines, and the shard and compute disagree about what that
row is:

- Compute writes each row's text into the stage pipe, so grep, sed, cut
  and awk see one line at a time. `grep -v DEBUG` prints the trace's
  other lines; `grep '^at '` matches each frame.
- The shard tests the whole row as one string. `NOT regexp_matches(raw,
  'DEBUG')` drops the whole trace if any line has DEBUG; `^` and `$`
  match only at its ends; awk's `$1` is the row's first field
  (`fields` splits the whole row), not each line's; a cut or awk
  projection prints once per row, not once per line.
- Compute's row stages (leading filters when a later stage reads the
  payload or meta-variables) keep or drop the whole row, so
  `grep -v DEBUG | awk '{ print HOST }'` still prints the DEBUG line.

The harness proved each of these (`TestDifferentialLineSplits`'s rows of
several lines; the probes behind `docs/known-issues.md`'s "cut and awk
projections run once per row"). The user-facing semantics are grep's:
a filter on a multi-line event acts on its lines.

Splitting rows into lines at query time (an `unnest` under every pushed
filter and projection, per-line field splits in SQL, a line number to
keep a row's lines in order) fixes the symptoms but puts the cost and
the complexity on every query, forever. Storing lines removes the
mismatch instead: a row is a line, to the shard and to grep alike.

## Design

### Row shape

A text event of N lines is stored as N rows. Each row's `raw` is one
line; `fields` is that line's split (`awkFields`, unchanged); every
other column — `ts`, `level`, the meta-variables, `trace_id` — is the
event's, copied to each line. Two new columns tie them together:

```sql
event_id  UUID,     -- shared by an event's lines; NULL for a one-line event
line_no   INTEGER   -- 0-based position in the event; NULL for a one-line event
```

Most events are one line, so both are NULL there and cost nothing
after compression. A multi-line event's lines repeat the same values
run after run, which DuckDB and parquet compress well. `event_id` is a
UUIDv7: unique across coordinators (N=2 in prod) without coordination,
and time-ordered.

`LogRow` gains `event_id` and `line_no` so split rows reach shards and
the DLQ with them.

### Which rows split

- **Text rows split.** Any row without a JSON payload.
- **JSON-schemaed rows never split.** A row whose schema parsed a JSON
  payload stays one row, whatever its `raw` holds: jq reads the payload
  once per event, and a pretty-printed JSON document isn't a sequence
  of independent lines.
- Span columns (`trace_id`, `span_id`, …) are the event's, like the
  rest, so a text event that carries a span copies it to each line.
  The trace sources must then count one span per `event_id`, not per
  row; check `trace`/`trace_spans` when building phase 1.

### Normalization (before the split)

- `\r\n` → `\n`, and a trailing `\r` on any line is dropped. Closes
  `docs/known-issues.md`'s "trailing `\r` that warp's line readers
  drop" for new rows: there's no CR left to disagree about.
- One trailing `\n` is dropped, so `"c\n"` is the one line `c`, not `c`
  and an empty line. Blank lines inside an event stay as empty rows
  (`grep '^$'` sees them, as it would in the file).
- An event that's empty after this is one empty row, as today.

### Where it happens

`coordinator/service/server.go`'s `ingestBatch` is the one seam every
producer (weftd, CloudWatch, syslog, HTTP) passes through, and it feeds
both the shards' ring buffer and live tail (`TailFanout.Publish`).
Splitting there means history and `tail -f` see the same rows, and an
event's lines go to the same shard: the drain routes by (customer,
namespace, service), which the lines share.

Usage metering counts bytes, so splitting changes it only by the
newlines it removes. Anything that counts rows (shard row counts, the
`ingest-governance` quotas) counts lines; that plan should say so.

The DLQ stores rows after the split, so re-ingest doesn't split again
(a row with an `event_id`, or without a newline, has nothing to split).

weftd's wire shape from [weftd-multiline](weftd-multiline.md) doesn't
change: it joins an event's lines with `\n` and the coordinator splits
them. weftd never mints event ids, so the coordinator never trusts a
client's.

### Order

An event's lines share `ts`. Queries that order by time order by
`(ts, event_id, line_no)` (NULLs first, so one-line events are
unaffected), and compute's k-way merge breaks ts ties the same way: an
event's lines live on one shard, but two events with the same `ts` on
two shards would otherwise interleave. `tail`'s DESC inner query and
the ASC outer re-sort use the reversed and forward triples. The shard
returns `event_id` and `line_no` alongside raw when the merge needs
them.

## What it changes in the planner

- Text filters (grep, sed -n, awk patterns including `$N`) and cut/awk
  projections need nothing: a row is a line, so today's per-row SQL is
  exact. The cut/awk-projection known issue closes for text rows.
- `Query.Lines` and the line split under `Query.Aggregate` stay, for
  the rows that can still hold newlines: a JSON row's `raw`, and a
  projection's output (`jq -r` of a string with a newline). They're
  exact either way; they just stop being the common case.
- Compute's row stages see single lines for text rows, so keep/drop is
  the line's fate. A JSON row is still one row to them.
- `PlannerVersion` bumps for the new ORDER BY.

## Existing data

Not migrated. Rows ingested before the change keep their newlines, with
today's behavior, until retention ages them out. `event_id`/`line_no`
are NULL on them, which orders them as one-line events.

Hot shards get the columns by `ALTER TABLE ADD COLUMN` (`logColumns`
already names columns, for migrated shards). Archived parquet written
before the change lacks them: the archive and cold-shard readers must
read old files with the columns as NULL (`union_by_name`, or a
per-file projection), or the new ORDER BY fails on them. Verify both
paths against a pre-change archive.

## What it enables (not in this plan)

Sentry-like grouping. With `event_id`, an event is
`string_agg(raw, chr(10) ORDER BY line_no) GROUP BY event_id`, so:

- **Whole events for matching lines.** `grep ValueError` finds the last
  line of a traceback; a stage like `events` (or a grep flag) could
  expand each match to its event:
  `WHERE event_id IN (SELECT event_id … WHERE <match>)`.
- **Fingerprinting.** Normalize an event's frames (strip line numbers,
  addresses, ids) and hash them into an issue key; count and first/last
  seen per issue.

Both are planner and shell features on top of the stored shape; they
need their own plans.

## Phases

1. **Storage and ingest.** `event_id`/`line_no` columns (hot shards,
   archive writer, cold-shard reader), `LogRow` fields, normalization
   and the split in `ingestBatch`, with tests over CloudWatch-shaped
   multi-line events, CRLF input, JSON rows that must not split, and the
   live tail seeing the same rows as the shard.
2. **Order.** `(ts, event_id, line_no)` in the shard's time orderings
   and compute's merge; harness rows of several lines with equal `ts`
   across shards.
3. **Cleanup.** Narrow `docs/known-issues.md` (the CR entry and the
   projection entry apply only to pre-change rows and JSON rows), and
   update the docs that describe a row as an event
   (`docs/query-execution.md`, `docs/shard-lifecycle.md`'s schema,
   weftd-multiline's "Emitted row shape").

## Open questions

- **JSON rows with newlines in `raw`.** A pretty-printed JSON event
  stays one row, so cut/awk/grep over it still see several lines in
  compute and one string in the shard. Rare in practice (NDJSON is the
  norm); options are compacting `raw` at ingest (it changes what the
  user sees) or leaving text pushdown off those rows.
- **Retention window for the mixed period.** How long pre-change rows
  with newlines stay queryable, and whether that's worth a note in the
  shell docs.
