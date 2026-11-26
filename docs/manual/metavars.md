---
id: manual-metavars
title: Meta-variables
web:
  section: metavars
  category: weft-shell
  order: 59
---

# Meta-variables

Each stored row carries a fixed set of indexed columns beside its text and/or JSON
payload. They are exposed to AWK and jq under the variable names below.

| awk                        | jq                       | Source                                                    |
|----------------------------|--------------------------|-----------------------------------------------------------|
| `NAMESPACE`                | `$namespace`            | the `<namespace>` path segment / agent token's namespace  |
| `SERVICE`                  | `$service`              | source declaration, log filename, or HTTP ingest payload  |
| `KIND`                     | `$kind`                 | the stream kind (`main`, `heartbeats`, `metrics`, `runtime`) |
| `HOST` *(alias `INSTANCE`)* | `$host` *(alias `$instance`)* | the agent's hostname |
| `LEVEL`                    | `$level`                | schema-extracted from the row |
| `TS`                       | `$ts`                   | schema-extracted from the row |
| `DEPLOYMENT`               | `$deployment`           | weftd `deployment:` config (or k8s/ASG) |
| `ENV`                      | `$env`                  | bound to the agent token at mint time |
| `VERSION`                  | `$version`              | weftd `metavariables.version` |
| `REGION`                   | `$region`               | cloud metadata probe at agent startup |
| `NODE`                     | `$node`                 | weftd `metavariables.node` |
| `TRACE_ID`                 | `$trace_id`             | the SDK's trace ID, or schema-extracted from the row      |
| `SPAN_NAME`                | `$span_name`            | on a span's lines, from the SDK header or the schema      |
| `SPAN_ID`                  | `$span_id`              | on a span's lines, from the SDK header or the schema      |
| `PARENT_SPAN_ID`           | `$parent_span_id`       | on a span's lines, the span that started it               |
| `DURATION_NS`              | `$duration_ns`          | on the line that ends a span, its duration in nanoseconds |
| `EVENT_ID`                 | `$event_id`             | shared by the lines of one multi-line event |
| `LINE_NO`                  | `$line_no`              | the line's 0-based position within its event |
