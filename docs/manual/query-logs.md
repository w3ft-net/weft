---
id: manual-query-logs
title: Query your logs
web:
  section: query-logs
  category: query
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

# Most recent N lines (cheap)
tail -n 100 myapp.log
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
| `$NODE` | `._node` | `node` | stable workload identity (e.g. shard ID) — file-sourced via the agent's `dimensions:` config; never auto-derived |

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

## Per-tag values over time

Indexed dimensions + a frequent watch + the multi-row alert form
together give you "value by host (or service / region / pod) over
time" without any new infrastructure. The full recipe — including
per-tag incident dedup with `-k`/`-t` and per-tag debouncing — is
in **Alerting strategies**.

In short: write a watch whose body emits one line per tag (e.g.
`"\(._host) \(.pct)"` from `jq -r`) and pipe it through
`alert above N -k 2 -t 1 --notify=ops`. Each host opens and closes
incidents independently against its tag.

Window aggregates over the watch's history (`weft::avg`,
`weft::max`, etc.) currently summarize the entire run's stdout as
one number, so the per-tag *aggregated history* is a follow-up.
For now the recipe gives you per-bucket per-tag values; for the
rolling average across buckets, point a second watch at it via
`weft::latest`.

## Charting a query

Pipe any pipeline into `chart` to render the result as a chart in
the terminal — no need to first commit it to a saved watch. The
chart is the terminal stage of the pipeline (no commands after it).

```
# Time-bucketed p95 latency over the current range
cat /log/api.log \
  | chart -x .ts -y .latency_ms --bucket 1m -a p95

# Distribution of total latency by path
cat /log/api.log | jq '{path, latency}' \
  | chart -x .path -y .latency -a sum

# Plot a stream of numbers from jq
cat /log/api.log | jq '.cpu_pct' | chart
```

`-x` and `-y` accept either a 1-indexed whitespace column (`-x 1`)
or a `.`-prefixed JSON path (`-x .ts`). With no flags, x is the
row sequence and y is the last whitespace field of each line — so
something like `wc -l | chart` plots a single value, and a
multi-line stream of bare numbers from `jq` plots one point per
line. See `man chart` for the full flag surface (aggregation,
bucketing, chart kind, title).

## Saving a result set

Redirect a pipeline into `/tmp` to hold onto the output and slice it
a few different ways without re-running the whole query:

```
cat api.log | awk '$LEVEL == "ERROR"' > /tmp/errs
grep timeout /tmp/errs | sort | uniq -c | sort -rn
```

Files in `/tmp` live for 48 hours. See the **Saving results to /tmp**
section for storage limits and the full set of operations.

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
