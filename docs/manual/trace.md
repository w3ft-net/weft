---
id: manual-trace
title: Tracing
shell:
  const: ManTrace
  topics:
    - "man trace"
    - "trace --help"
  banner: "TRACE(1)"
web:
  section: trace
  category: analysis-alerting
  order: 45
---

## Tracing

The `trace` command aggregates lines across services in all your namespaces. A
bare `trace` lists your most recent traces. Give it a trace ID with `--id` and it
returns every log line carrying that ID in chronological order. The `--spans`
flag gives you a per-operation breakdown: of one request's duration per stage
with `--id`, or of how an operation performs across all requests without it.
All provided, of course, that you've annotated your log lines and set up your
schema to parse those annotations.

```
trace [<namespace>[/<service>]]                       the recent traces
trace --id <id> [<scope>]                             one request's log lines
trace --spans --id <id> [<scope>]                     where one request's time went
trace --spans [<scope>]                               how operations behave across requests
trace --spans --name <span> [--samples] [<scope>]     one operation
```

The optional `<scope>` is a namespace or a `namespace/service`, and narrows any
form: `trace acme/api` lists the traces that touched `acme/api`, `trace --spans
acme/api` aggregates only that service's spans, and `trace --id <id> acme/api`
shows only that service's lines of the request. Narrow by time with
[`range`](../shell/man-range.md): `trace` reads the current range, so a trace
older than the window needs a wider one first.

### Getting trace IDs into your logs

If you're using one of the [weft
SDKs](https://github.com/orgs/w3ft-net/repositories), you'll get a lot of the
tracing work for cheap-to-free. They will:

- adopt the `traceparent` header of an incoming HTTP request, or start a new
  trace when there is none, using included middlewares
- provide convenience methods for conveyors those values to another service in
  headers via HTTP or with interceptors over gRPC
- track span names and durations
- serialize all the above values onto each log line emitted by SDK-instrumented
  methods 

However, if you've already got a logging setup you're happy with and it
contains those values, you can just create a schema that knows how to extract
those values.

### Finding a trace

A bare `trace` lists the last 50 traces in the current `range`, newest first:

```
$ trace
 AGE     DUR LINES ERRORS ROOT            SERVICES                             TRACE
 20s    1.2s     7      2 GET /orders/812 acme/api, acme/orders, acme/payments 4bf92f3577b34da6a3ce929d0e0e4736
  3m   840ms    12      0 POST /checkout  acme/api, acme/orders                9c1d02e87a5b4f3d8e6a1b2c3d4e5f60
 11m     5ms     1      -                 acme/worker                          e07a4b9d1c2f43a58b6d7e8f90a1b2c3
```

`AGE` is how long ago the trace's last line was written, and `DUR` is the time
from its first line to its last. `LINES` and `ERRORS` count every line of the
trace, on every service, and `SERVICES` lists the services that wrote one. `ROOT`
is the name of the span that started the request, when there is one. The `TRACE`
column is last because it is the one you copy: hand it to `--id`. In the browser,
click a row to open that trace.

`ERRORS` counts lines at level `ERROR`. A `-` means none of the trace's lines
carried a log level at all, so there's no way to know.

The summary always covers the whole trace, even when you narrow the list with a
scope: `trace acme/api` picks the traces that touched `acme/api`, then shows each
one in full.

Piped, you get one JSON line per trace, ready for jq (`errors` is `null` where
the table shows `-`):

```
{"trace_id":"4bf92f3577b34da6a3ce929d0e0e4736","start_ns":1760882531000000000,"end_ns":1760882532200000000,"dur_ns":1200000000,"lines":7,"errors":2,"services":["acme/api","acme/orders","acme/payments"],"root":"GET /orders/812"}
```

```
trace | jq -r 'select(.errors > 0) | .trace_id'
```

### One request

```
$ trace --id 4bf92f3577b34da6a3ce929d0e0e4736
# 7 line(s) across 3 service(s), 2026-10-19T14:02:11Z to 2026-10-19T14:02:12Z
                 acme/api      GET /orders/812
   +3ms          acme/orders   loading order 812
  +24ms        █ acme/orders   db.query took 21ms
  +80ms     ████ acme/payments charging card ending 4242
 +160ms ████████ acme/payments charge ok
  +40ms       ██ acme/orders   order 812 confirmed
  +12ms          acme/api      200 OK
```

The first column shows time elapsed since the previous line (the first line
thus having no value there). On a narrow enough terminal, the time column is
dropped.

The table above is only how `trace` prints to a terminal. Put a pipe after it
and you get the log lines themselves, without the summary, bars or badges, so
you can filter them like any other source. Each row still knows where it came
from: `$namespace` and `$service` hold the service that wrote it, so you can
filter or print them:

```
trace --id 4bf92f3577b34da6a3ce929d0e0e4736 | grep -i error
trace --id 4bf92f3577b34da6a3ce929d0e0e4736 | jq -r '$service + ": " + .msg'
```

### Where one request's time went

```
$ trace --spans --id 4bf92f3577b34da6a3ce929d0e0e4736
NAME                                   COUNT     TOTAL ERRORS
api.handle (acme/api)               █      1       7ms      0
db.query (acme/orders)     ██████████      3      38ms      0
db.query (acme/payments)           ██      1       9ms      -
orders.load (acme/orders)          ██      1      11ms      0
```

Spans are grouped by name *and* service, because the same name in two services
is usually two different things (both `db.query` rows above). `TOTAL` is each
group's self time: its own duration minus time spent in its child spans. The bar
is scaled to the largest `TOTAL`, so it points at the real bottleneck of the
request.

`ERRORS` counts a span's error lines: log lines at level `ERROR` that carry the
span's name. The SDK writes one when you record an error on a span. An ordinary
`ERROR` log line with no span name isn't counted, and neither is an error nobody
recorded on the span, so a `0` means no span reported an error, not that the
request succeeded. A `-` means none of that service's lines carried a log
level at all, so there's no way to know. Without the SDK, your schema has to
extract both the span name and the level for this column to fill in.

In the browser, running `trace --spans --id <id>` will open a modal. In
the terminal, it opens an interactive view. Both will let you drill down on
specific spans. Piped, it gives one JSON line per finished span or span error,
ready for jq:

```
{"name":"db.query","namespace":"acme","service":"orders","trace_id":"4bf9…","span_id":"a1","parent_id":"b2","dur_ns":21000000,"ts_ns":1760882531000000000,"level":"INFO"}
```

`ts_ns` is when the span ended, so it started at `ts_ns - dur_ns`.

### How operations behave across requests

```
$ trace --spans
NAME                                 COUNT       AVG       P50       P99       MIN       MAX ERRORS
api.handle (acme/api)    ██████████   9102     1.2ms     1.1ms     4.8ms     410µs     9.9ms      0
db.query (acme/orders)         ████  18420     500µs     410µs     2.3ms      90µs     8.1ms      3
```

Every span name in the current `range`, aggregated across all traces and
grouped by the service that ran it, since the same name in two services is
usually two different things. The bar is scaled to the largest `AVG`. Add
`--name` to look at just one, and a scope to look at just one service:

```
trace --spans --name db.query
trace --spans acme/orders
```

`COUNT`, `ERRORS`, `AVG`, `MIN` and `MAX` are exact. `P50` and `P99` are
approximate (within 1% of the true value).

`trace --spans` has no self time here, since it sees spans one name at a time
and can't see their children; use `trace --spans --id <id>` for that.

The interactive view works like the one above. `Enter` on a name lists its
slowest occurrences, each with the trace ID to hand to `--id`.

```
trace --spans --name db.query --samples
```

prints the same list: the 50 slowest finished occurrences of a name, one JSON
line each in the shape above.

#### Using the results

Piped, `trace --spans` gives one JSON line per name and service:

```
{"name":"db.query","namespace":"acme","service":"orders","count":18420,"errors":3,"total_ns":9210000000,"avg_ns":500000,
 "p50_ns":410000,"p99_ns":2300000,"min_ns":90000,"max_ns":8100000}
```

`errors` is `null` where the table shows `-`: no line carried a log level, so
the count is unknown, not zero. So you can filter, reshape, chart or alert on it
like any other source:

```
trace --spans | jq -r 'select(.p99_ns > 1000000000) | .name'
trace --spans --name db.query | jq '.p99_ns / 1000000' | alert above 250 --notify=ops
```

The second line is a cron-ready check that pages when `db.query`'s 99th percentile
passes 250ms.

### What counts as a span

A finished span is a row with a `DURATION_NS`; it counts toward `COUNT` and the
timings. A span's start line has no duration and isn't counted. A row at level
`ERROR` that names a span is a span error and counts toward `ERRORS`. A source
with no span rows, such as one that logs plain text without the SDK or a schema
that extracts spans, has nothing for `trace --spans` to find.

### Not supported

- More than one trace ID per call, or a partial ID or pattern. The ID is
  matched whole.
- Following a trace live (`trace -f`), or narrowing by time with an option. Use
  `range` for time, and pipe to `grep` or `jq` for the rest.
- More than one scope per call.
- `--samples` without a span name, and `--name` together with `--id`.
- Merging a span name across services: `trace --spans` always groups by service.
- Self time in `trace --spans` without `--id`; use `trace --spans --id <id>` for
  that.

A trace ID is passed with `--id`. A bare word is a scope, so `trace
4bf92f3577b34da6a3ce929d0e0e4736` fails with `unknown source: 4bf9…`.

Other options are rejected with `trace: unknown option <flag>`.
