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

# Search for errors in the default range (the last day; narrow it with `range 1h..`)
cat myapp.log | awk 'LEVEL == "ERROR"'

# Count requests by status code
cat nginx.log | jq '.status' | sort | uniq -c | sort -rn

# Grep every source in a namespace
cat myapp.log | grep "connection refused"

# Most recent N lines (cheap)
tail -n 100 myapp.log
```

weft analyzes the full pipeline and uses its filters to narrow what it
reads.

A hard line count (`head -n N`, `tail -n N`) ends that narrowing for
the filters after it. `tail -n 500 | awk 'LEVEL == "ERROR"' | wc -l`
counts the errors among the last 500 rows (at most 500), while
`grep ERROR | tail -n 5` filters first and gives the last 5 matches.
Crons read a time range, and a line count can silently truncate it, so
omit `-n` in a cron.

Globs are allowed only to pick log kinds inside one service:
`cat /log/myco/prod/api/*.log`. A glob that would read across services
or namespaces — `cat *.log`, `cat /log/myco/prod/*.log`,
`/log/myco/*/api/main.log` — is refused with an error. To read
everything in a namespace, use `cat <namespace>.log`.

`cat /log/myco/api/main.log` (three segments) reads that service across
every env, and `ls /log/myco/api` lists its kinds;
`cat /log/myco/prod/api/main.log` (four segments) reads one env.

## Meta-variables

Each row weft stores has a fixed set of indexed columns alongside
its raw text and JSON payload. Filtering on these columns is the
fastest path through the planner, because weft can skip data that
can't match. Anything not in the table below still works in awk/jq,
but is slower.

| awk | jq | Column | Source |
|---|---|---|---|
| `NAMESPACE` | `$namespace` | `namespace` | the `<namespace>` segment in `/log/<namespace>/<service>/<kind>.log` |
| `SERVICE` | `$service` | `service` | the log file's source name (the `<service>` in `/log/<ns>/<service>/<kind>.log`) |
| `KIND` | `$kind` | `kind` | the stream kind (`main`, `heartbeats`, `metrics`, `runtime`) |
| `HOST` (alias `INSTANCE`) | `$host` (alias `$instance`) | `instance` | the agent's hostname |
| `LEVEL` | `$level` | `level` | parsed from the line per the source's schema |
| `TS` | `$ts` | `ts` | row timestamp |
| `DEPLOYMENT` | `$deployment` | `deployment` | weftd config `deployment:` value (k8s Deployment, AWS ASG, etc.) |
| `ENV` | `$env` | `env` | bound to the agent token at mint time (`weft token create --env prod`) |
| `VERSION` | `$version` | `version` | weftd `version.file:` (live) or `version.env:` (snapshot) |
| `REGION` | `$region` | `region` | detected from cloud metadata at agent startup |
| `CLUSTER` | `$cluster` | `cluster` | k8s mode only: cluster name from the DaemonSet config |
| `K8S_NAMESPACE` | `$k8s_namespace` | `k8s_namespace` | k8s mode only: pod's k8s namespace |
| `POD` | `$pod` | `pod` | k8s mode only: pod name |
| `CONTAINER` | `$container` | `container` | k8s mode only: container name |
| `NODE` | `$node` | `node` | stable workload identity (e.g. a workload ID) — file-sourced via the agent's `dimensions:` config; never auto-derived |

A few examples:

```
# Errors in production for one specific deployment
cat /log/myco/prod/api/main.log | awk 'LEVEL == "ERROR" && DEPLOYMENT == "api-east"'

# Same query in jq form against a JSON source
cat /log/myco/prod/api/main.log | jq 'select($level == "ERROR" and $deployment == "api-east")'

# Per-host memory metrics
tail /log/myco/prod/api/metrics.log | jq 'select($host == "web-3" and .type == "memory") | .pct'

# Region-scoped grep
cat /log/myco/prod/api/main.log | awk 'REGION == "us-east-2"' | grep "timeout"
```

### Why two naming styles

awk meta-variables are uppercase short names (`SERVICE`, `ENV`); jq
uses lowercase variables (`$service`, `$env`). jq is case-sensitive
and real JSONL sometimes carries keys like `Pod` or `pod`, so
`$pod` always means "the indexed column" and `.pod` always means
"a key in this row's JSON payload."

The source coordinates address three separate columns —
`NAMESPACE`, `SERVICE`, `KIND` — mirroring the path layout
`/log/<namespace>/<service>/<kind>.log`. `HOST` / `$host` is an
alias for `INSTANCE` / `$instance` — use whichever name fits your
team's vocabulary.

### Custom fields fall through to JSON-extract

Anything not in the table above lives in the row's JSON payload
and is queryable at lower speed:

```
cat /log/myco/prod/api/main.log | jq 'select(.feature_flag == "abc")'
```

weft reads and parses every row in the time range for this filter. For
dimensions you query constantly, use the indexed columns.

## How jq reads a row

jq reads each row's JSON payload: the whole line for a JSON log, or the
JSON part of a line that has text around it (a timestamp prefix, a
CloudWatch envelope, Heroku's `key=value` metrics). The meta-variables
(`$level`, `$host`, `$ts`, ...) are there too.

A jq program made only of `select(...)` filters rows, and prints the log
lines it keeps, exactly as they were:

```
# The matching log lines, timestamp prefix and all
cat /log/myco/prod/api/main.log | jq 'select(.status == 500)'

# The payload JSON of those lines instead
cat /log/myco/prod/api/main.log | jq 'select(.status == 500) | .'
```

Anything else prints jq's own output, one value per line. Objects and
arrays print on a single line, so the output stays one row per line for
`grep`, `wc -l` and `alert`.

The payload stays attached to a row while stages only filter rows or
count them (`grep`, an awk pattern, `head`, `tail`), so this works the
same:

```
tail -n 500 /log/myco/prod/api/main.log | grep checkout | jq '.latency_ms'
```

Once a stage changes the text (`sort`, `awk '{ print ... }'`, `sed s/.../`),
the next stage reads that text, as in any shell.

jq compares values the way jq does: `select(.status == 500)` doesn't
match the string `"500"`, and a row without the field has `null` there,
which sorts below every number, so `select(.latency_ms < 100)` also
keeps rows that have no `latency_ms`.

## Per-tag values over time

Indexed dimensions, a frequent cron and the multi-row alert form
together produce "value by host (or service / region / pod) over
time". The full recipe — including
per-tag incident dedup with `-k`/`-t` and per-tag debouncing — is
in **Alerting strategies**.

In short: write a cron whose body emits one line per tag (e.g.
`"\($host) \(.pct)"` from `jq -r`) and pipe it through
`alert above N -k 2 -t 1 --notify=ops`. Each host opens and closes
incidents independently against its tag.

Window aggregates (`weft::avg`, `weft::max`, etc.) summarize the
whole run's stdout as one number. The recipe produces per-bucket,
per-tag values; for a rolling average across buckets, point a second
cron at it via `weft::latest`.

## Charting a query

Pipe any pipeline into `chart` to render the result as a chart in
the terminal. The chart is the terminal stage of the pipeline; no
commands follow it.

```
# Time-bucketed p95 latency over the current range
cat /log/myco/api/main.log \
  | chart -x .ts -y .latency_ms --bucket 1m -a p95

# Distribution of total latency by path
cat /log/myco/api/main.log | jq '{path, latency}' \
  | chart -x .path -y .latency -a sum

# Plot a stream of numbers from jq
cat /log/myco/api/main.log | jq '.cpu_pct' | chart

# Lines per minute from a plain-text log, to spot a gap
cat /log/myco/api/main.log \
  | chart -x '1-2::time(%Y/%m/%d %H:%M:%S)' --bucket 1m
```

`-x` and `-y` accept either a 1-indexed whitespace column (`-x 1`)
or a `.`-prefixed JSON path (`-x .ts`). With no flags, x is the
row sequence and y is the last whitespace field of each line — so
something like `wc -l | chart` plots a single value, and a
multi-line stream of bare numbers from `jq` plots one point per
line. An RFC3339 `-x` is read as a time on its own; for epoch
seconds or any other stamp, cast it: `-x 1::time`, or
`-x '1-2::time(%Y/%m/%d %H:%M:%S)'` with strftime codes, where `1-2`
joins two columns. With `--bucket`, minutes with no rows chart as 0.
See `man chart` for the full flag surface (aggregation, bucketing,
`--type`, title).

## Saving a result set

Redirect a pipeline into `/tmp` to keep the output and query it
several ways:

```
cat api.log | awk 'LEVEL == "ERROR"' > /tmp/errs
grep timeout /tmp/errs | sort | uniq -c | sort -rn
```

Files in `/tmp` live for 48 hours. See the **Saving results to /tmp**
section for storage limits and the full set of operations.

## Plan retention and the archive

Your plan determines how far back queries can read. See the
**Account** page for your plan's limits; data older than that is
filtered out of every query, even if it is still stored.

Older data is archived and queried transparently. A `range 180d..`
query reads recent and archived data in one pass with the
same syntax. The only user-visible effect is a small extra latency
on the cold side.

See `help builtins` in the shell for the full command list, and
`man range` for time-window syntax.
