# Meta-variables

Meta-variables expose row-level dimensions as fast-path filters in
awk and jq pipelines. Where a payload field would resolve through
`json_extract`, a meta-variable maps directly to a real column on the
shard — order-of-magnitude faster, and the row-group min/max
statistics stay useful.

## Reserved set

| awk                        | jq                       | Column           | Source                                                    |
|----------------------------|--------------------------|------------------|-----------------------------------------------------------|
| `$APP` *(alias `$SERVICE`)* | `._app` *(alias `._service`)* | `app`            | log filename, source declaration, or HTTP ingest payload  |
| `$HOST` *(alias `$INSTANCE`)* | `._host` *(alias `._instance`)* | `instance`       | weftd `os.Hostname()`                                     |
| `$LEVEL`                    | `._level`                | `level`          | schema-extracted from the row                             |
| `$TS`                       | `._ts`                   | `ts`             | schema-extracted from the row                             |
| `$DEPLOYMENT`               | `._deployment`           | `deployment`     | weftd `deployment:` config (or k8s/ASG)                   |
| `$ENV`                      | `._env`                  | `env`            | bound to the agent token at mint time                     |
| `$VERSION`                  | `._version`              | `version`        | weftd `version.file:` (live) or `version.env:` (snapshot) |
| `$REGION`                   | `._region`               | `region`         | cloud metadata probe at agent startup                     |
| `$CLUSTER`                  | `._cluster`              | `cluster`        | weftd k8s mode                                            |
| `$K8S_NAMESPACE`            | `._k8s_namespace`        | `k8s_namespace`  | weftd k8s mode (parsed from `/var/log/pods/…`)            |
| `$POD`                      | `._pod`                  | `pod`            | weftd k8s mode                                            |
| `$CONTAINER`                | `._container`            | `container`      | weftd k8s mode                                            |
| `$NODE`                     | `._node`                 | `node`           | weftd `dimensions.node:` (file-sourced)                   |

A row that wasn't tagged with a given dimension stamps `NULL` —
queries on `$ENV == "prod"` neither include nor exclude such rows;
they simply don't match the predicate.

### Naming convention

awk meta-variables are all-caps short flat names — `$ENV`, `$POD`,
`$DEPLOYMENT` — matching the pre-existing `$LEVEL` / `$APP` /
`$INSTANCE` shape.

jq meta-variables use leading-underscore lowercase — `._env`,
`._pod`. The underscore disambiguates from real JSON payload keys:
`.pod` is whatever the producer wrote into the row body; `._pod` is
the column. Real JSONL sometimes carries `Pod` / `PoD` / `POD` keys,
so the underscore is more reliable than relying on case alone.

The one verbose name is `k8s_namespace` (`$K8S_NAMESPACE` /
`._k8s_namespace`). The shorter `namespace` is unavailable because
weft already uses "namespace" for the customer's logical
token-grouping in the path (`/log/<namespace>/...`).

## Path layout

The pseudofile path encodes the namespace and (optionally) the env:

```
/log/<namespace>/<app>.log
/log/<namespace>/<env>/<app>.log
```

Tokens minted with `weft token create --namespace foo --env prod`
produce rows that resolve to the 3-level path; tokens without `--env`
produce rows at the 2-level path. A namespace can hold both layouts
side by side as different tokens reauth over time.

```
weft> ls /log/myco
prod/                  ← env-bearing token
staging/               ← env-bearing token
api.log                ← legacy token without env
```

`cat /log/myco/api.log` reads the env-less stream; `cat
/log/myco/prod/api.log` reads the env-prod stream; `cat
/log/myco/*/api.log` matches across every env that has one. No
backfill, no synthetic placeholder env.

Region is intentionally **not** part of the path layout — it's a
query-time filter (`awk '$REGION == "us-east-2"'`) only. Same for
deployment, host, cluster, pod: those are query-time dimensions, not
filesystem axes.

## Pushdown

Each reserved meta-variable rewrites to a direct column predicate:

```
awk '$ENV == "prod"'                  → WHERE env = 'prod'
jq 'select(._env == "prod")'          → WHERE env = 'prod'
awk '$POD ~ "api-7f9b"'               → WHERE pod LIKE 'api-7f9b%'
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

A `cron.d` watch with `$HOST` carrying the dimension makes
"avg memory pct by host over 1h" a one-liner:

```
# /cron.d/memory-per-host
# schedule: every 1m
tail -n 200 /log/myco/metrics.log \
  | jq -r 'select(._level == "INFO" and .type == "memory") | "\(._host) \(.pct)"'
```

Each tick emits one row per host. Multi-row alerts (`alert above 80
-k 2 -t 1`) page per host independently because `$HOST` is a real
column the alert grouping understands.

### Filter by env across deployments

```
cat /log/myco/prod/*.log \
  | jq 'select(._level == "ERROR" and ._deployment == "api")' \
  | head -50
```

The path narrows to env=prod; the column predicate further narrows
to deployment=api; the resulting shard query is two indexable WHERE
clauses, no JSON parsing.

### Compare a single deployment across regions

```
cat /log/myco/api.log \
  | jq 'select(._deployment == "api")' \
  | jq -r '"\(._region) \(._level)"' \
  | sort | uniq -c
```

`$REGION` is populated from cloud metadata at agent startup; the
rollup gives a level-by-region histogram for the deployment.
