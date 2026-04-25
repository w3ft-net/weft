---
id: manual-query-logs
title: Query your logs
web:
  section: query-logs
  order: 55
---

## Query your logs

Open the **Shell** or use the `weft` CLI. Log sources appear as
pseudofiles — use standard Unix pipeline idioms:

```
# Tail logs in real time
tail -f myapp.log

# Search for errors in the last hour
cat myapp.log | awk '$LEVEL == "ERROR"'

# Count requests by status code
cat nginx.log | jq '.status' | sort | uniq -c | sort -rn

# Grep across all sources
cat *.log | grep "connection refused"

# Time-bounded queries
cat myapp.log --since 2h --until 30m | head -100
```

The weft query planner analyzes your full pipeline and pushes
predicates down into SQL — you write familiar shell commands, weft
makes them fast.

## Meta-variables

Each row weft stores has a fixed set of indexed columns alongside
its raw text and JSON payload. Filtering on these columns is the
fastest path through the planner — predicates rewrite to direct
SQL `WHERE` clauses with row-group statistics pruning. Anything
not in the table below still works in awk/jq, but goes through the
slower JSON-extract path.

| awk | jq | Column | Source |
|---|---|---|---|
| `$APP` (alias `$SERVICE`) | `._app` (alias `._service`) | `app` | the log file's source name (`/log/<ns>/<app>.log`) |
| `$HOST` (alias `$INSTANCE`) | `._host` (alias `._instance`) | `instance` | the agent's hostname |
| `$LEVEL` | `._level` | `level` | parsed from the line per the source's schema |
| `$TS` | `._ts` | `ts` | row timestamp |
| `$DEPLOYMENT` | `._deployment` | `deployment` | weftd config `deployment:` value (k8s Deployment, AWS ASG, etc.) |
| `$ENV` | `._env` | `env` | bound to the agent token at mint time (`weft token create --env prod`) |
| `$VERSION` | `._version` | `version` | per-source `version_file` watched by the agent |
| `$REGION` | `._region` | `region` | detected from cloud metadata at agent startup |
| `$CLUSTER` | `._cluster` | `cluster` | k8s mode only: cluster name from the DaemonSet config |
| `$K8S_NAMESPACE` | `._k8s_namespace` | `k8s_namespace` | k8s mode only: pod's k8s namespace |
| `$POD` | `._pod` | `pod` | k8s mode only: pod name |
| `$CONTAINER` | `._container` | `container` | k8s mode only: container name |

A few examples:

```
# Errors in production for one specific deployment
cat /log/myco/prod/api.log | awk '$LEVEL == "ERROR" && $DEPLOYMENT == "api-east"'

# Same query in jq form against a JSON source
cat /log/myco/prod/api.log | jq 'select(._level == "ERROR" and ._deployment == "api-east")'

# Per-host memory metrics
tail /log/weft/metrics.log | jq 'select(._host == "shard-3" and .type == "memory") | .pct'

# Region-scoped grep
cat /log/myco/prod/api.log | awk '$REGION == "us-east-2"' | grep "timeout"
```

### Why two naming styles

awk meta-variables are uppercase short names (`$APP`, `$ENV`); jq
uses leading-underscore lowercase (`._app`, `._env`). The
underscore is the disambiguation convention — jq is case-sensitive
and real JSONL sometimes carries keys like `Pod` or `pod`, so
`._pod` always means "the indexed column" and `.pod` always means
"a key in this row's JSON payload."

`$SERVICE` / `._service` is an alias for `$APP` / `._app`, and
`$HOST` / `._host` is an alias for `$INSTANCE` / `._instance` — use
whichever name fits your team's vocabulary.

### Custom fields fall through to JSON-extract

Anything not in the table above lives in your row's JSON payload
and is still queryable — just slower:

```
cat /log/myco/prod/api.log | jq 'select(.feature_flag == "abc")'
```

The planner pushes that to `WHERE json_extract_string(payload,
'$.feature_flag') = 'abc'`, which works correctly but can't use
row-group statistics for skip-ahead — every row in the time range
is read and parsed. For dimensions you query constantly, prefer
the indexed columns.

## Per-tag averages over time

The combination of indexed dimensions, watches, and the multi-row
alert form gets you "avg cpu by host over the last hour" without
any new infrastructure. Three pieces compose:

1. A **watch** with a frequent schedule — the schedule is your
   bucket size. Every tick produces stdout that's stored as a row.
2. The watch body emits **one line per tag**, with the tag and a
   numeric value. Whitespace separated; tag first, value second.
3. `weft.avg <watch> <duration>` (or `weft.max`, `weft.min`,
   `weft.sum`, `weft.stddev`) reads back the run history.

A concrete example — average memory pressure per host, bucketed
every minute:

```
# /cron.d/memory-per-host
# schedule: every 1m
tail /log/weft/metrics.log --since 1m \
  | jq -r 'select(.type == "memory") | "\(._host) \(.pct)"'
```

That emits lines like:

```
shard-0    62.4
shard-1    58.1
api-east   71.9
```

Each line is a per-host bucket for this minute. To alert per host
when memory passes 80%, pipe into `alert` in multi-row mode:

```
# /cron.d/memory-alert
# schedule: every 1m
tail /log/weft/metrics.log --since 1m \
  | jq -r 'select(.type == "memory") | "\(._host) \(.pct)"' \
  | alert above 80 -k 2 -t 1 --notify=ops
```

`-k 2` says "field 2 holds the numeric value"; `-t 1` says "field
1 names this row" (the host) so each host opens and closes
incidents independently.

The same shape works with any indexed dimension as the tag —
swap `\(._host)` for `\(._deployment)`, `\(._pod)`, `\(._region)`,
or any combination (`"\(._region) \(._host) \(.pct)"` for region-
prefixed host names). For string dimensions there's no perf
penalty: each is a real column, queries push down with row-group
statistics.

Window aggregates over the watch's history (`weft.avg`,
`weft.max`, etc.) currently summarize the entire run's stdout as
one number, so the per-tag history is a follow-up. For now the
recipe above gives you per-bucket per-tag values; for the rolling
average across buckets, point a second watch at it via
`weft.latest`.

## Plan retention and the archive

Your plan determines how far back queries can read — Trial keeps a
day, Bolt keeps a year. The **Account** page shows your current cap;
runs older than that are filtered out of every query, even if the
underlying rows are still on disk.

Recent data lives on hot DuckDB shards; older data is exported to
Parquet on S3 and queried transparently. A `range 6m..` query
stitches hot and archived shards in one pass — there is no
separate "search archived logs" step, and no special syntax for
the older portion. The only user-visible effect is a small extra
latency on the cold side.

See `help builtins` in the shell for the full command list, and
`man range` for time-window syntax.
