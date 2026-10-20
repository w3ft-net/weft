---
id: manual-metavars
title: Meta-variables
web:
  section: metavars
  category: weft-shell
  order: 57
---

# Meta-variables

Each stored row carries a fixed set of indexed columns beside its text and JSON
payload. Meta-variables expose them in awk and jq. Filtering on one is the
fast path: weft skips data that can't match instead of parsing every row.

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
| `VERSION`                  | `$version`              | `version`        | weftd `metavariables.version:` |
| `REGION`                   | `$region`               | `region`         | cloud metadata probe at agent startup                     |
| `CLUSTER`                  | `$cluster`              | `cluster`        | weftd k8s mode                                            |
| `K8S_NAMESPACE`            | `$k8s_namespace`        | `k8s_namespace`  | weftd k8s mode                                            |
| `POD`                      | `$pod`                  | `pod`            | weftd k8s mode                                            |
| `CONTAINER`                | `$container`            | `container`      | weftd k8s mode                                            |
| `NODE`                     | `$node`                 | `node`           | weftd `metavariables.node:` (never auto-derived) |
| `TRACE_ID`                 | `$trace_id`             | `trace_id`       | the SDK's trace ID, or schema-extracted from the row      |
| `SPAN_NAME`                | `$span_name`            | `span_name`      | on a span's lines, from the SDK header or the schema      |
| `SPAN_ID`                  | `$span_id`              | `span_id`        | on a span's lines, from the SDK header or the schema      |
| `PARENT_SPAN_ID`           | `$parent_span_id`       | `parent_span_id` | on a span's lines, the span that started it               |
| `DURATION_NS`              | `$duration_ns`          | `duration_ns`    | on the line that ends a span, its duration in nanoseconds |
| `EVENT_ID`                 | `$event_id`             | `event_id`       | shared by the lines of one multi-line event               |
| `LINE_NO`                  | `$line_no`              | `line_no`        | the line's 0-based position within its event              |

## Semantics

- A row without a dimension carries an empty string: `ENV == "prod"` does not
  match it, `ENV != "prod"` does. In jq `$line_no` and `$duration_ns` are
  numbers (`$duration_ns` is null on a row without one); the rest are strings.
- `EVENT_ID` / `LINE_NO` tie together the lines of a multi-line event (a stack
  trace): each line is its own row, sharing an `EVENT_ID`. A one-line event is
  line 0 with an empty `EVENT_ID`, so `LINE_NO == 0` selects the first line of
  every event and `EVENT_ID != ""` only lines of multi-line events.
- `NAMESPACE` is weft's token grouping (the `/log/<namespace>/...` segment).
  The Kubernetes namespace is `K8S_NAMESPACE`.
- `REGION`, `DEPLOYMENT`, `HOST`, `CLUSTER` and `POD` are query-time
  dimensions, not path segments. For which dimensions appear in the path, see
  [Paths](query-logs.md#paths).

## Naming

awk names are bare uppercase globals (`ENV`, `POD`) — never `$ENV`, which in
awk means "field number ENV". jq names are lowercase variables (`$env`,
`$pod`). `$pod` is always the indexed column and `.pod` always a key in the
row's JSON payload, even when a producer writes `Pod` or `POD`. A jq program
that rebinds a name (`. as $env | ...`) reads its own value.

## Speed

Meta-variable filters are fast: `awk 'ENV == "prod"'`, `awk 'POD ~ "api-7f9b"'`,
`jq 'select($level == "ERROR")'`. Anything else reads and parses every row in
the time range: `jq 'select(.feature_flag == "abc")'`, awk's numbered fields
(`$3 == "POST"`). A pipeline can mix both; the meta-variable filters narrow the
data first and the rest runs on what remains. For a dimension you query
constantly, ask for it to be indexed rather than filtering on the payload.
