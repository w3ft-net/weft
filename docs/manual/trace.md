---
id: manual-trace
title: Tracing a request
web:
  section: trace
  category: weft-shell
  order: 60
---

## Tracing a request

`trace` follows one request through every service it touched. Give it a trace
ID and it returns every log line carrying that ID, from every source you can
read, in time order. Add `--spans` for a per-operation breakdown of where the
request's time went, or ask `trace spans` how an operation performs across all
requests.

```
trace 4bf92f3577b34da6a3ce929d0e0e4736
trace 4bf92f3577b34da6a3ce929d0e0e4736 --spans
trace spans
trace spans db.query --by service
```

Like `cat`, `trace` reads the current [`range`](../shell/man-range.md), so a trace older than
the window needs a wider one first.

### Getting trace IDs into your logs

A line has a trace ID when the weft SDK wrote it, with the ID your request
already carries. The SDKs set up the rest:

- **Join a trace.** The Go and Python SDKs adopt the `traceparent` header of an
  incoming HTTP request, or start a new trace when there is none. Go:
  `weft.TraceMiddleware`. Python: `weft.WSGITraceMiddleware` and
  `weft.ASGITraceMiddleware`.
- **Carry it across calls.** `weft.TraceHeaders` (Go) and `weft.trace_headers`
  (Python) give the headers for an outgoing HTTP request. For gRPC, install the
  `weft.GRPC` interceptors (Go) or `weft.grpc` (Python) on both ends.
- **Time the work.** `weft.StartSpan` (Go) and `weft.start_span` (Python) log a
  span with a name, an ID, its parent and, when it ends, its duration. A span
  started inside another becomes its child.
- **Log through the SDK.** Lines written through `slog` with the request's
  context (Go), or the standard `logging` module (Python), carry the trace and
  the current span automatically. See the SDK READMEs for the details of each.

Trace IDs and spans are columns on every row: `TRACE_ID`, `SPAN_NAME`,
`SPAN_ID`, `PARENT_SPAN_ID` and `DURATION_NS` (see [meta-variables](metavars.md)).

### trace ID

```
$ trace 4bf92f3577b34da6a3ce929d0e0e4736
# 7 line(s) across 3 service(s), 2026-10-19T14:02:11Z to 2026-10-19T14:02:12Z
           [acme/api]      GET /orders/812
██         [acme/orders]   loading order 812
           [acme/orders]   db.query took 21ms
```

The first line counts the lines, the services they came from and the time they
span. Each row after it has a badge for the service that wrote it, in a color
that stays the same for that service, and a bar showing how long it was since the
previous line, so a stall shows up as a long bar.

Pipe it like any other source, and the badges and summary go away, leaving the
lines themselves. Every row carries `$namespace` and `$service` if you need to
tell the sources apart:

```
trace 4bf92f3577b34da6a3ce929d0e0e4736 | grep -i error
trace 4bf92f3577b34da6a3ce929d0e0e4736 | jq -r '$service + ": " + .msg'
```

### trace ID --spans

```
$ trace 4bf92f3577b34da6a3ce929d0e0e4736 --spans
NAME          BAR         COUNT     TOTAL  ERRORS
db.query      ██████████      3      38ms       0
orders.load   ███             1      11ms       0
api.handle    ██              1       7ms       0
```

One row per span name in the trace. `COUNT` is how many times it ran. `TOTAL` is
**self time**: the span's duration minus the time its children covered, with
concurrent children counted once. It answers "where did this request's time
actually go", where a parent span's duration mostly reflects the work it handed
off. `ERRORS` counts the span's error lines.

Self time is only as complete as the trace: a child that logged nothing, or
that is older than the current `range`, leaves its time on its parent.

In the terminal, `trace <id> --spans` opens an interactive view:

| Key | Does |
|---|---|
| `↑` `↓` or `j` `k` | move |
| `g` `G` | first, last |
| `Enter` | list the selected span's occurrences, with the trace each came from and its self time |
| `Esc` | back to the list from the occurrences; leave the view from the list |
| `q` | leave the view |

Piped, it gives one JSON line per finished span or span error, ready for `jq`:

```
{"name":"db.query","namespace":"acme","service":"orders","trace_id":"4bf9…","span_id":"a1","parent_id":"b2","dur_ns":21000000,"ts_ns":1760882531000000000,"level":"INFO"}
```

`ts_ns` is when the span ended, so it started at `ts_ns - dur_ns`.

### trace spans

```
$ trace spans
NAME          BAR         COUNT     AVG     P50     P99     MIN     MAX  ERRORS
db.query      ██████████  18420   500µs   410µs   2.3ms    90µs   8.1ms       3
api.handle    ███          9102   1.2ms   1.1ms   4.8ms   410µs   9.9ms       0
```

Every span name in the current `range`, aggregated across all traces. Add a name
to look at one, or `--by service` to split each name by the service that ran
it, since the same name in two services is usually two different things:

```
trace spans db.query
trace spans --by service
```

The shards compute these in the database, so the cost follows the number of
span names, not the number of spans. `COUNT`, `ERRORS`, `AVG`, `MIN` and `MAX`
are exact. **`P50` and `P99` are approximate**: within 1% of the true value.

`trace spans` has no self time, since it sees spans one name at a time and can't
see their children; use `trace <id> --spans` for that.

The interactive view works like the one above. `Enter` on a name lists its
slowest occurrences, each with the trace ID to hand to `trace`.

```
trace spans db.query --samples
```

prints the same list: the 50 slowest finished occurrences of a name, one JSON
line each in the shape above.

#### Using the results

Piped, `trace spans` gives one JSON line per name (or per name and service):

```
{"name":"db.query","count":18420,"errors":3,"total_ns":9210000000,"avg_ns":500000,
 "p50_ns":410000,"p99_ns":2300000,"min_ns":90000,"max_ns":8100000}
```

With `--by service`, `namespace` and `service` are added. So you can filter,
reshape, chart or alert on it like any other source:

```
trace spans | jq -r 'select(.p99_ns > 1000000000) | .name'
trace spans db.query | jq '.p99_ns / 1000000' | alert above 250 --notify=ops
```

The second line is a cron-ready check that pages when `db.query`'s 99th percentile
passes 250ms.

### What counts as a span

A finished span is a row with a `DURATION_NS`; it counts toward `COUNT` and the
timings. A span's start line has no duration and isn't counted. A row at level
`ERROR` that names a span is a span error and counts toward `ERRORS`. A source
with no span rows, such as one that logs plain text without the SDK or a schema
that extracts spans, has nothing for `trace spans` to find.
