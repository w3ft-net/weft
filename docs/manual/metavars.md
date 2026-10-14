---
id: manual-metavars
title: Meta-variables
web:
  section: metavars
  category: query
  order: 60
---

# Meta-variables

Meta-variables expose row-level dimensions as fast filters in awk and
jq pipelines. Filtering on a meta-variable is much faster than filtering
on a field inside the row's JSON payload, because weft can skip data
that can't match.

## Reserved set

| awk                        | jq                       | Column           | Source                                                    |
|----------------------------|--------------------------|------------------|-----------------------------------------------------------|
| `NAMESPACE`                | `$namespace`            | `namespace`      | the `<namespace>` path segment / agent token's namespace  |
| `SERVICE`                  | `$service`              | `service`        | source declaration, log filename, or HTTP ingest payload  |
| `KIND`                     | `$kind`                 | `kind`           | the stream kind (`main`, `heartbeats`, `metrics`, `runtime`) |
| `HOST` *(alias `INSTANCE`)* | `$host` *(alias `$instance`)* | `instance`       | the agent's hostname                                      |
| `LEVEL`                    | `$level`                | `level`          | schema-extracted from the row                             |
| `TS`                       | `$ts`                   | `ts`             | schema-extracted from the row                             |
| `DEPLOYMENT`               | `$deployment`           | `deployment`     | weftd `deployment:` config (or k8s/ASG)                   |
| `ENV`                      | `$env`                  | `env`            | bound to the agent token at mint time                     |
| `VERSION`                  | `$version`              | `version`        | weftd `version.file:` (live) or `version.env:` (snapshot) |
| `REGION`                   | `$region`               | `region`         | cloud metadata probe at agent startup                     |
| `CLUSTER`                  | `$cluster`              | `cluster`        | weftd k8s mode                                            |
| `K8S_NAMESPACE`            | `$k8s_namespace`        | `k8s_namespace`  | weftd k8s mode                                            |
| `POD`                      | `$pod`                  | `pod`            | weftd k8s mode                                            |
| `CONTAINER`                | `$container`            | `container`      | weftd k8s mode                                            |
| `NODE`                     | `$node`                 | `node`           | weftd `dimensions.node:` (file-sourced)                   |
| `EVENT_ID`                 | `$event_id`             | `event_id`       | shared by the lines of one multi-line event               |
| `LINE_NO`                  | `$line_no`              | `line_no`        | the line's 0-based position within its event              |

`EVENT_ID` and `LINE_NO` tie together the lines of a multi-line event
(a stack trace, a Python traceback): each line is its own row, and they
share an `EVENT_ID`. A one-line event is line 0 and has an empty
`EVENT_ID`, so `LINE_NO == 0` selects the first line of every event and
`EVENT_ID != ""` selects only lines that belong to a multi-line event.
In jq `$line_no` and `$duration_ns` are numbers (`$duration_ns` is null on a row without one); the other meta-variables are strings.

A row without a given dimension carries an empty string. `ENV == "prod"` does not match it, but `ENV != "prod"` does.

### Naming convention

awk meta-variables are all-caps short flat names — `ENV`, `POD`,
`DEPLOYMENT`, `LEVEL`, `SERVICE`, `INSTANCE`. The source coordinates
are three columns — `NAMESPACE`, `SERVICE`, `KIND` — each addressed
directly.

Meta-variables are written bare, without a `$` prefix: `$NAME` in awk is
field-by-variable-index syntax (`$n` where `n` is `NAME`'s value) and
cannot carry a string like `"web-1"`. A bare `HOST` is an ordinary awk
global, like awk's built-in `NR`/`NF`/`FILENAME`.

jq meta-variables are lowercase jq variables — `$env`, `$pod`. A
variable can't be mistaken for a payload key: `.pod` is whatever the
producer wrote into the row body; `$pod` is the column. Real JSONL
sometimes carries `Pod` / `PoD` / `POD` keys, so the `$` marks the
column reliably in any case. A program that rebinds one of the names
(`. as $env | ...`) reads its own value.

`NAMESPACE` is weft's logical token grouping — the `<namespace>`
segment in the path (`/log/<namespace>/...`). The Kubernetes
namespace is a distinct dimension named `K8S_NAMESPACE`
(`$k8s_namespace`).

## Path layout

The pseudofile path encodes the namespace, service, and stream
kind, plus the env when the producing token carries one:

```
/log/<namespace>/<service>/<kind>.log          env-less token
/log/<namespace>/<env>/<service>/<kind>.log    env-bearing token
```

`<kind>` is the stream: `main` for ordinary application logs (the
default a producer ships unless it says otherwise), plus
`heartbeats`, `metrics`, and `runtime` for services that emit
telemetry. Every segment is always present; segment count alone
distinguishes the env-less form from the env-bearing one.

Tokens minted with `weft token create --namespace foo --env prod`
produce rows that resolve to the 4-segment path; tokens without
`--env` produce rows at the 3-segment path. A namespace can hold both
layouts side by side, depending on how each token was minted.

```
ls /log/myco
```

lists the env directories (`prod/`, `staging/`) next to any env-less
service directories (`api/`).

`cat /log/myco/prod/api/main.log` reads one env. The three-segment
`cat /log/myco/api/main.log` reads that service across every env,
including the env-less stream, and `ls /log/myco/api` lists the kinds
it carries. To read a whole namespace, use `cat myco.log` and filter
on `ENV`: `cat myco.log | awk 'ENV == "prod"'`. A glob that would
read across services or envs, such as `/log/myco/*/api/main.log`,
`/log/myco/prod/*.log` or `cat *.log`, is refused with an error;
globs are allowed only to pick kinds inside one service
(`/log/myco/prod/api/*.log`).

Region is a query-time filter (`awk 'REGION == "us-east-2"'`) and is
absent from the path layout. Deployment, host, cluster and pod are also
query-time dimensions.

## Speed

Filtering on a reserved meta-variable is the fast path, in awk
(`awk 'ENV == "prod"'`, `awk 'POD ~ "api-7f9b"'`) and in jq
(`jq 'select($level == "ERROR")'`). A filter on any other field, such
as `jq 'select(.feature_flag == "abc")'`, reads and parses every row in
the time range. awk's numbered fields (`awk '$3 == "POST"'`) also run
against every row. One pipeline can mix both: the meta-variable filters
narrow the data first and the rest runs on what remains.

A hard line count (`head -n N` or `tail -n N`) stops later filters from
narrowing the read: `tail -n 500 | awk 'LEVEL == "ERROR"' | wc -l`
counts the errors among the last 500 rows, while
`awk 'LEVEL == "ERROR"' | tail -n 5` gives the last 5 errors.

## Recipes

### Per-host average over a window

A cron in `/cron.d` that emits the host next to each value produces
"avg memory pct by host". A cron reads its own time range,
so omit `-n` here: a line count can silently truncate the window.

```
# /cron.d/memory-per-host
# schedule: */1 * * * *
cat /log/myco/prod/api/metrics.log \
  | jq -r 'select($level == "INFO" and .type == "memory") | "\($host) \(.pct)"'
```

Each tick emits one `<host> <pct>` row per memory sample. Multi-row
alerts (`alert above 80 -k 2 -t 1`) page per host independently
because `-t 1` tags each row by its first field (the host).

### Filter by env across deployments

```
cat myco.log \
  | awk 'ENV == "prod" && LEVEL == "ERROR" && DEPLOYMENT == "api"' \
  | head -50
```

The `ENV` and `DEPLOYMENT` filters are fast-path filters, so weft
narrows to env=prod and deployment=api without parsing each row's
JSON. For JSON log streams the same filter reads
`jq 'select($env == "prod" and $deployment == "api")'`.

### Compare a single deployment across regions

```
cat /log/myco/prod/api/main.log \
  | awk 'DEPLOYMENT == "api" {print REGION, LEVEL}' \
  | sort | uniq -c
```

`REGION` is populated from cloud metadata at agent startup; the
rollup gives a level-by-region histogram for the deployment.
