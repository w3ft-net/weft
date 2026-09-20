---
id: manual-metavars
title: Meta-variables
web:
  section: metavars
  category: query
  order: 60
---

# Meta-variables

Meta-variables expose row-level dimensions as fast-path filters in
awk and jq pipelines. Where a payload field would resolve through
`json_extract`, a meta-variable maps directly to a real column on the
shard — order-of-magnitude faster, and the row-group min/max
statistics stay useful.

## Reserved set

| awk                        | jq                       | Column           | Source                                                    |
|----------------------------|--------------------------|------------------|-----------------------------------------------------------|
| `NAMESPACE`                | `._namespace`            | `namespace`      | the `<namespace>` path segment / agent token's namespace  |
| `SERVICE`                  | `._service`              | `service`        | source declaration, log filename, or HTTP ingest payload  |
| `KIND`                     | `._kind`                 | `kind`           | the stream kind (`main`, `heartbeats`, `metrics`, `runtime`) |
| `HOST` *(alias `INSTANCE`)* | `._host` *(alias `._instance`)* | `instance`       | weftd `os.Hostname()`                                     |
| `LEVEL`                    | `._level`                | `level`          | schema-extracted from the row                             |
| `TS`                       | `._ts`                   | `ts`             | schema-extracted from the row                             |
| `DEPLOYMENT`               | `._deployment`           | `deployment`     | weftd `deployment:` config (or k8s/ASG)                   |
| `ENV`                      | `._env`                  | `env`            | bound to the agent token at mint time                     |
| `VERSION`                  | `._version`              | `version`        | weftd `version.file:` (live) or `version.env:` (snapshot) |
| `REGION`                   | `._region`               | `region`         | cloud metadata probe at agent startup                     |
| `CLUSTER`                  | `._cluster`              | `cluster`        | weftd k8s mode                                            |
| `K8S_NAMESPACE`            | `._k8s_namespace`        | `k8s_namespace`  | weftd k8s mode (parsed from `/var/log/pods/…`)            |
| `POD`                      | `._pod`                  | `pod`            | weftd k8s mode                                            |
| `CONTAINER`                | `._container`            | `container`      | weftd k8s mode                                            |
| `NODE`                     | `._node`                 | `node`           | weftd `dimensions.node:` (file-sourced)                   |

A row that wasn't tagged with a given dimension stamps `NULL` —
queries on `ENV == "prod"` neither include nor exclude such rows;
they simply don't match the predicate.

### Naming convention

awk meta-variables are all-caps short flat names — `ENV`, `POD`,
`DEPLOYMENT` — matching the pre-existing `LEVEL` / `SERVICE` /
`INSTANCE` shape. The source coordinates split into three columns —
`NAMESPACE`, `SERVICE`, `KIND` — each addressed directly rather
than rolled into one conflated value.

Bare, not `$`-prefixed: `$NAME` in real awk is field-by-variable-index
syntax (`$n` where `n` is `NAME`'s value), unrelated to a named
variable's own value — it can't carry a string like `"web-1"`. A bare
`HOST` is an ordinary awk global, exactly like awk's own built-in
`NR`/`NF`/`FILENAME`.

jq meta-variables use leading-underscore lowercase — `._env`,
`._pod`. The underscore disambiguates from real JSON payload keys:
`.pod` is whatever the producer wrote into the row body; `._pod` is
the column. Real JSONL sometimes carries `Pod` / `PoD` / `POD` keys,
so the underscore is more reliable than relying on case alone.

`NAMESPACE` is weft's own logical token-grouping — the
`<namespace>` segment in the path (`/log/<namespace>/...`). The
Kubernetes namespace, a distinct dimension, takes the verbose
`K8S_NAMESPACE` (`._k8s_namespace`) so the two never collide.

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
layouts side by side as different tokens reauth over time — segment
count alone disambiguates them, so a reader token spanning several
envs never has to guess.

```
weft> ls /log/myco
prod/                  ← env-bearing token
staging/               ← env-bearing token
api/                   ← env-less token
```

`cat /log/myco/api/main.log` reads the env-less stream; `cat
/log/myco/prod/api/main.log` reads the env-prod stream; `cat
/log/myco/*/api/main.log` matches across every env that has one. No
backfill, no synthetic placeholder env.

Region is intentionally **not** part of the path layout — it's a
query-time filter (`awk 'REGION == "us-east-2"'`) only. Same for
deployment, host, cluster, pod: those are query-time dimensions, not
filesystem axes.

## Pushdown

Each reserved meta-variable rewrites to a direct column predicate:

```
awk 'ENV == "prod"'                  → WHERE env = 'prod'
jq 'select(._env == "prod")'          → WHERE env = 'prod'
awk 'POD ~ "api-7f9b"'               → WHERE pod LIKE 'api-7f9b%'
jq 'select(._level == "ERROR")'       → WHERE level = 'ERROR'
```

For non-reserved fields, the existing JSON-extract pushdown still
applies — slower than a column predicate, but still pushdown:

```
jq 'select(.feature_flag == "abc")'
  → WHERE json_extract_string(payload, '$.feature_flag') = 'abc'
```

A pipeline mixing both becomes a single `WHERE` with the column
predicate (fast) AND the JSON-extract predicate (slower); DuckDB
plans it as one stage.

## Recipes

### Per-host average over a window

A cron in `/cron.d` with `HOST` carrying the dimension makes
"avg memory pct by host over 1h" a one-liner:

```
# /cron.d/memory-per-host
# schedule: every 1m
tail -n 200 /log/myco/api/metrics.log \
  | jq -r 'select(._level == "INFO" and .type == "memory") | "\(._host) \(.pct)"'
```

Each tick emits one row per host. Multi-row alerts (`alert above 80
-k 2 -t 1`) page per host independently because `HOST` is a real
column the alert grouping understands.

### Filter by env across deployments

```
cat /log/myco/prod/*/main.log \
  | jq 'select(._level == "ERROR" and ._deployment == "api")' \
  | head -50
```

The path narrows to env=prod; the column predicate further narrows
to deployment=api; the resulting shard query is two indexable WHERE
clauses, no JSON parsing.

### Compare a single deployment across regions

```
cat /log/myco/api/main.log \
  | jq 'select(._deployment == "api")' \
  | jq -r '"\(._region) \(._level)"' \
  | sort | uniq -c
```

`REGION` is populated from cloud metadata at agent startup; the
rollup gives a level-by-region histogram for the deployment.
