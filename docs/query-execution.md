# Query Execution

## Overview

When the user types a pipeline in the weft shell, the query planner analyzes the entire pipeline as a single unit, pushes down as much as possible into shard-level SQL, and executes the remainder as gpipe compute stages on the result stream.

## Pipeline lifecycle

```
cat vimgym.log | awk '$LEVEL == "ERROR" && $3 > 100' | jq '.payload' | sort | uniq -c
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

| Expression | SQL |
|-----------|-----|
| `$LEVEL == "ERROR"` | `WHERE level = 'ERROR'` |
| `$SERVICE == "coordinator"` | `WHERE service = 'coordinator'` |
| `$3 > 100` | `WHERE CAST(list_extract(fields, 3) AS DOUBLE) > 100` |
| `/timeout/` | `WHERE raw LIKE '%timeout%'` |
| `NR >= 10 && NR <= 50` | `LIMIT/OFFSET` |
| Compound `&&` / `\|\|` | `AND` / `OR` |

### jq

| Expression | SQL |
|-----------|-----|
| `select(.status == 500)` | `WHERE CAST(json_extract(payload, '$.status') AS INT) = 500` |
| `select(._service == "coordinator")` | `WHERE service = 'coordinator'` |
| `select(._level == "ERROR")` | `WHERE level = 'ERROR'` |

### grep/sed

| Expression | SQL |
|-----------|-----|
| `grep ERROR` | `WHERE raw LIKE '%ERROR%'` |
| `grep -i error` | `WHERE raw ILIKE '%error%'` |
| `sed '/pattern/...'` | Address pushdown: `WHERE raw LIKE '%pattern%'` |

### Non-pushable stages

These become gpipe compute stages:

- `sort`, `uniq`, `wc` — aggregation/reordering
- `sed 's/foo/bar/'` — substitution (modifies output, not a filter)
- `awk '{ print $2, $1 }'` — output formatting
- `jq '{ ts: ._ts, msg: .message }'` — object construction
- `awk` with `END` blocks — requires seeing all rows before producing output

## Pipeline splitting

The planner walks the pipeline left to right. The source command (`cat`, `head`, `tail`, `tail -f`) establishes the base query. Subsequent stages are examined:

1. If the stage is fully pushable (pure filter), fold it into the SQL WHERE clause
2. If the stage is partially pushable (e.g., awk with both a filter pattern and a print action), extract the pushable predicate and leave the action as a gpipe stage
3. Once a non-pushable stage is reached, everything from that point on becomes gpipe

Example:
```
cat vimgym.log | awk '$LEVEL == "ERROR"' | sed 's/foo/bar/' | sort
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

1. Client sends `tail -f vimgym.log | awk '$LEVEL == "ERROR"'`
2. Coordinator extracts predicates, identifies the active shard
3. Opens a `Tail` gRPC stream to the active shard with pushdown predicates
4. Active shard's ingest path notifies subscribers on each batch write
5. New matching rows stream through the gRPC channel to the coordinator
6. Coordinator runs any remaining gpipe stages and streams results to the CLI
7. On shard handoff (active window closes), coordinator detects the new active shard and transparently opens a new `Tail` stream

## Open questions

- **Multi-source pipelines:** `cat vimgym.log callbox.log | ...` — should this be supported as a union query, or should users use `warp.log` with meta-variable filters? Multi-arg cat is natural Unix but adds planner complexity.
- **Pushdown verification:** should we log/display what was pushed down vs. what runs as gpipe? Useful for debugging and for users understanding query performance.
- **Cross-shard aggregation:** `sort` and `uniq -c` across multiple shards require a merge step. For `sort`, merge-sort on the coordinator is straightforward. For `uniq -c`, partial counts from each shard need summing. How far do we take distributed aggregation vs. just collecting all rows at the coordinator?
- **Query cancellation:** user hits ctrl-c mid-query. Coordinator sends cancel to shards via gRPC context cancellation — straightforward, but need to make sure shard-side cleanup is correct.
