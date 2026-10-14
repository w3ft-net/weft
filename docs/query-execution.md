# Query Execution

## Overview

When the user types a pipeline in the weft shell, the query planner analyzes the entire pipeline as a single unit, pushes down as much as possible into shard-level SQL, and executes the remainder as gpipe compute stages on the result stream.

## Pipeline lifecycle

```
cat vimgym.log | awk 'LEVEL == "ERROR" && $3 > 100' | jq '.payload' | sort | uniq -c
```

1. **Source resolution** — `cat vimgym.log` identifies the source app and time range
2. **Predicate extraction** — planner walks the AST of each pipeline stage greedily, pulling out everything it can express as SQL
3. **Pipeline splitting** — the first stage that can't be pushed down becomes the split point; everything from there on runs as gpipe
4. **Shard routing** — time range determines which shards to query
5. **Shard execution** — each shard runs the pushed-down SQL query and streams matching rows via gRPC
6. **Compute execution** — coordinator merges shard streams and runs the remaining gpipe stages
7. **Output** — results stream to the client

## Predicate extraction

The planner is greedy — it attempts to push down as much as possible. Pushable patterns:

### awk

`$N` reads the shard's `fields` column, which ingest splits from `raw` the
way awk splits `$0` (runs of spaces, tabs, newlines, carriage returns),
whatever the source's schema. Rows ingested before 2026-10-13 hold an
older, schema-dependent split, so a `$N` query reaching them may disagree
with awk.

| Expression | SQL |
|-----------|-----|
| `LEVEL == "ERROR"` | `WHERE level = 'ERROR'` |
| `SERVICE == "coordinator"` | `WHERE service = 'coordinator'` |
| `$4 == "ERROR"` | `WHERE list_extract(fields, 4) = 'ERROR'` |
| `$3 > 100` | Prefilter: `WHERE (TRY_CAST(list_extract(fields, 3) AS DOUBLE) > 100 OR list_extract(fields, 3) > '100' OR …)`; awk still runs, since it compares as numbers or as strings by the value |
| `{ print $2, $1 }` | `SELECT concat_ws(' ', list_extract(fields, 2), list_extract(fields, 1))` (missing fields as `''`) |
| `/timeout/` | `WHERE raw LIKE '%timeout%'` |
| `NR >= 10 && NR <= 50` | `LIMIT/OFFSET` |
| Compound `&&` / `\|\|` | `AND` / `OR` |

### jq

| Expression | SQL |
|-----------|-----|
| `select(.status == 500)` | `WHERE CAST(json_extract(payload, '$.status') AS INT) = 500` |
| `select(._service == "coordinator")` | `WHERE service = 'coordinator'` |
| `select(._level == "ERROR")` | `WHERE level = 'ERROR'` |
| `.a.b`, `{k: .x}`, `[.a, .b]` | `SELECT` the JSON text jq prints (compact; `null` when missing) |
| `-r .a` | The same, with a string printed as its unescaped text |

### grep/sed

| Expression | SQL |
|-----------|-----|
| `grep ERROR` | `WHERE raw LIKE '%ERROR%'` |
| `grep -i error` | `WHERE raw ILIKE '%error%'` |
| `sed '/pattern/...'` | Address pushdown: `WHERE raw LIKE '%pattern%'` |

### cut

`cut [-d D] -f LIST` splits `raw` on each `D` (a tab by default), which
isn't how `fields` splits, so it lowers to its own `SELECT`:
`string_split(raw, D)`, the listed fields in list order (`N`, `N-M`,
`N-`), joined by `D`. Any other flag (`-c`, `-s`) runs in compute.

### Aggregates

When nothing ahead of them in compute needs the rows, the shard computes
these and returns one row per distinct line with its count, instead of
the rows:

| Stages | Shard SQL | Compute prints |
|--------|-----------|----------------|
| `wc -l` | `count(*)` | the total |
| `sort -u`, `sort \| uniq` | `GROUP BY raw ORDER BY raw` | each line once |
| `sort \| uniq -c` | `GROUP BY raw ORDER BY raw`, `count(*)` | `uniq -c`'s `%7d %s` |

`sort -r` orders by `raw DESC`. The lines grouped are the row's `raw`, or
the projection before it (`jq -r .action | sort | uniq -c`), split on
newlines the way compute would feed them to the stage. Compute merges the
shards' rows on the line and sums the counts for each line, then runs the
stages after the aggregate (`sort -rn | head`) as usual. It doesn't
apply after a pushed line count (`tail -n 500 | wc -l` counts the merged
500 rows, not 500 per shard), after row stages, in follow mode, or when
the stage after it names a meta-variable.

### Filters after a sort or a projection

A `grep` or `sed -n '/re/p'` after a plain `sort` (`-r` and `-u` too)
moves ahead of it into the shard: sort only reorders lines, so
`sort | grep x` prints what `grep x | sort` does. After a projection the
shard computes (`jq -r .action | grep login`), the filter reads the
projected text, which is the SELECT's `raw`. Because compute splits
each row into lines before `sort` reads them, the shard does the same
(`unnest(string_split(raw, chr(10)))`) and filters each line, so a
multi-line row keeps only its matching lines. A keyed sort (`-n`, `-k`, a
file operand) keeps the filter in compute, as do a pushed line count
(`tail -n 50 | sort | grep x` greps the last 50 rows), follow mode and
trace sources, and so does a filter whose removal would leave a stage
naming a meta-variable right after the projection. A filter after a
`uniq` stays put: dropping a line can make two equal lines adjacent.

### Non-pushable stages

These become gpipe compute stages:

- `sort` with a key (`-n`, `-k`), `uniq` without a `sort` right before it, `wc` other than `-l`
- `sed 's/foo/bar/'` — substitution (modifies output, not a filter)
- `awk '{ print $NF }'` — a field by a computed index
- `jq '{ ts: ._ts, msg: .message }'` — object construction
- `awk` with `END` blocks — requires seeing all rows before producing output

## Pipeline splitting

The planner walks the pipeline left to right. The source command (`cat`, `head`, `tail`, `tail -f`) establishes the base query. Subsequent stages are examined:

1. If the stage is fully pushable (pure filter), fold it into the SQL WHERE clause
2. If the stage is partially pushable (e.g., awk with both a filter pattern and a print action), extract the pushable predicate and leave the action as a gpipe stage
3. Once a non-pushable stage is reached, everything from that point on becomes gpipe

Example:
```
cat vimgym.log | awk 'LEVEL == "ERROR"' | sed 's/foo/bar/' | sort
                  ^^^^^^^^^^^^^^^^^^^       ^^^^^^^^^^^^^^^^^^^^^^^
                  pushed down to SQL        gpipe compute stages
```

## Shard routing

The coordinator maintains a registry of active shards and their time windows. Given a time range:

1. Find all shards whose windows overlap the query range
2. For `tail -f`, also subscribe to the active (ingesting) shard's push channel
3. Fan out the pushed-down query to each matching shard in parallel
4. Merge results ordered by timestamp (or by line_num within a shard)

For `head -N` and `tail -N`, the coordinator can limit how many shards it queries and push LIMIT down to the shard level.

### A line count ends predicate pushdown

Pipeline order is meaning. A filter *before* a `head -N` / `tail -N` is pushed into
the shard SQL ahead of the LIMIT (`grep ERROR | tail -n 100` is the last 100 matching
rows). A filter *after* it is never pushed, selective or not: the shards return only
the LIMIT scan, the coordinator merges to the global top N, and every later stage runs
in compute on those N rows (`tail -n 500 | awk 'LEVEL == "ERROR"' | wc -l` is the number
of errors among the last 500 rows, so it can never exceed 500). The same holds for an awk
`NR` window. This is a correctness rule, not an optimization gap: pushing the filter would
turn "filter the last N" into "the last N that match". Cron pipelines are bounded by their
time range, so they should not use `-n` at all; a line count can silently truncate the
window. See `completed/cross-shard-post-limit-filter.md`.

## Shard ↔ coordinator protocol

gRPC streaming. The shard service exposes:

```protobuf
service ShardQuery {
  // One-shot query: stream matching rows then close
  rpc Query(QueryRequest) returns (stream LogRow);

  // Live tail: stream new rows as they're ingested
  rpc Tail(TailRequest) returns (stream LogRow);
}

message QueryRequest {
  string sql = 1;           // pushed-down SQL
  int64 limit = 2;          // 0 = unlimited
}

message TailRequest {
  string app = 1;           // app filter (empty = all)
  string where_clause = 2;  // pushed-down predicates
}

message LogRow {
  int64 line_num = 1;
  string raw = 2;
  string app = 3;
  string level = 4;
  int64 ts_unix_ns = 5;
  string instance = 6;
  bytes payload_json = 7;   // raw JSON bytes for jq stages
  repeated string fields = 8; // pre-split for awk $N
}
```

## Tail execution

`tail -f` uses a push model, not polling:

1. Client sends `tail -f vimgym.log | awk 'LEVEL == "ERROR"'`
2. Coordinator extracts predicates, identifies the active shard
3. Opens a `Tail` gRPC stream to the active shard with pushdown predicates
4. Active shard's ingest path notifies subscribers on each batch write
5. New matching rows stream through the gRPC channel to the coordinator
6. Coordinator runs any remaining gpipe stages and streams results to the CLI
7. On shard handoff (active window closes), coordinator detects the new active shard and transparently opens a new `Tail` stream

## Open questions

- **Multi-source pipelines:** `cat vimgym.log callbox.log | ...` — should this be supported as a union query, or should users use `warp.log` with meta-variable filters? Multi-arg cat is natural Unix but adds planner complexity.
- **Pushdown verification:** should we log/display what was pushed down vs. what runs as gpipe? Useful for debugging and for users understanding query performance.
- **Query cancellation:** user hits ctrl-c mid-query. Coordinator sends cancel to shards via gRPC context cancellation — straightforward, but need to make sure shard-side cleanup is correct.
