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

The summary always covers the whole trace, even when you narrow the list with a
scope: `trace acme/api` picks the traces that touched `acme/api`, then shows each
one in full.

Piped, you get one JSON line per trace, so you can go hunting for slowness with jq:

```
{"trace_id":"4bf92f3577b34da6a3ce929d0e0e4736","start_ns":1760882531000000000,"end_ns":1760882532200000000,"dur_ns":1200000000,"lines":7,"errors":2,"services":["acme/api","acme/orders","acme/payments"],"root":"GET /orders/812"}
```

```
trace | jq -r 'select(.errors > 0) | .trace_id'
```

Alternatively, if you are looking at a particularly troubling log line via some
1-liner involving jq or AWK, you can get your trace id via
[metavariable](metavars.md).

### A single request

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

Like a bare `trace`, the form above returns JSON when piped, with `$namespace` and `$service` present as metavars:

```
trace --id 4bf92f3577b34da6a3ce929d0e0e4736 | grep -i error
trace --id 4bf92f3577b34da6a3ce929d0e0e4736 | jq -r '$service + ": " + .msg'
```

### Digging into spans for a request

```
$ trace --spans --id 4bf92f3577b34da6a3ce929d0e0e4736
NAME                                   COUNT     TOTAL ERRORS
api.handle (acme/api)               █      1       7ms      0
db.query (acme/orders)     ██████████      3      38ms      0
db.query (acme/payments)           ██      1       9ms      -
orders.load (acme/orders)          ██      1      11ms      0
```

In the browser, running `trace --spans --id <id>` will open a modal. In the
terminal, it opens an interactive view. Both will let you drill down on
specific spans. Piped, it again gives one JSON line per finished span or span
error:

```
{"name":"db.query","namespace":"acme","service":"orders","trace_id":"4bf9…","span_id":"a1","parent_id":"b2","dur_ns":21000000,"ts_ns":1760882531000000000,"level":"INFO"}
```

`ts_ns` is when the span ended, so it started at `ts_ns - dur_ns`.

### Global span tracking

```
$ trace --spans
NAME                                 COUNT       AVG       P50       P99       MIN       MAX ERRORS
api.handle (acme/api)    ██████████   9102     1.2ms     1.1ms     4.8ms     410µs     9.9ms      0
db.query (acme/orders)         ████  18420     500µs     410µs     2.3ms      90µs     8.1ms      3
```

`trace` can take the `--span` flag without a trace id to analyze span
statistics across all traces in your current range. Add `--name` to look at
just one, and a scope to look at just one service:

```
trace --spans --name db.query
trace --spans acme/orders
```

`COUNT`, `ERRORS`, `AVG`, `MIN` and `MAX` are exact. `P50` and `P99` are
approximate (within 1% of the true value).

#### Using the results

The fact that `trace` results yield JSON means you can filter, reshape, chart
or alert on it like any other source:

```
trace --spans --name db.query | jq '.p99_ns / 1000000' | alert above 5000 --notify=ops
```

Throw that in a [cron](crons.md) and you get paged if the 99th percentile of you
queries creeps past five seconds.
