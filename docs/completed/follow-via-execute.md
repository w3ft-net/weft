# Plan: unify Follow into Coordinator.Execute as a continuous stream

## Status

Done. `Coordinator.ExecuteFollow(ctx, pipeline, tr, customer, userID)`
returns a `*ExecuteResult` whose `Stream` is a `concatStream`
splicing the historical batch into a `brokerStream` over
`TailBroker.Subscribe`. `Server.Execute` collapsed to a single pump
loop that dispatches to `ExecuteFollow` when `req.Follow` is set,
otherwise `Execute`. New types live in
`coordinator/concat_stream.go` and `coordinator/broker_stream.go`;
unit tests in `coordinator/concat_stream_test.go` and
`coordinator/broker_stream_test.go`, plus an end-to-end transition
test in `coordinator/execute_follow_test.go`.

## Why

Follow mode (`tail -f /log/foo.log`) is implemented as a
post-script in the gRPC handler:

```go
// coordinator/service/server.go: Server.Execute
func (s *Server) Execute(req, stream) error {
    // 1. Run the historical batch via Coord.Execute → drain stream → wire.
    result, err := s.Coord.Execute(req.Pipeline, ...)
    // ... pump result.Stream onto wire ...

    // 2. If req.Follow, subscribe to broker and pump live rows
    //    indefinitely, applying the BuildLiveFilter pipeline-stage
    //    re-runner to each broker row.
    if !req.Follow || s.Coord.Tail == nil { return nil }
    plan, err := planner.Plan(req.Pipeline)
    matcher := &coordinator.RowMatcher{App: plan.FollowApp, Patterns: plan.FollowPatterns}
    filter := coordinator.BuildLiveFilter(plan)
    id, ch := s.Coord.Tail.Subscribe(stream.Context(), matcher)
    defer s.Coord.Tail.Unsubscribe(id)
    for {
        select {
        case <-ctx.Done(): return nil
        case row, ok := <-ch:
            if !ok { return nil }
            liveLines := filter(row)
            if len(liveLines) == 0 { continue }
            outLines := make([]*pb.Line, len(liveLines))
            for i, ln := range liveLines {
                outLines[i] = &pb.Line{LineNum: row.LineNum, Raw: ln}
            }
            stream.Send(&pb.CoordinatorExecuteResponse{Lines: outLines})
        }
    }
}
```

Two paths in one method:

- **Batch path** — drains `result.Stream` from `Coord.Execute`,
  batches into `Lines`, sends.
- **Follow path** — re-runs `planner.Plan`, sets up broker
  subscription, pumps broker rows, applies `BuildLiveFilter` per
  row, ships as `Lines` indefinitely.

Both produce wire-format `Line` events. Both can fail mid-stream
(client disconnects, ctx cancels). Both use the same
`stream.Send`. Yet the server forks based on `req.Follow` and
re-plans the pipeline a second time to pull out follow metadata.

After phase 4, `LineStream` is the right shape for either. A
unified `Coord.Execute` could emit historical lines first, then
transition to a broker-backed line source until ctx cancels. The
server's pump loop would never know the difference.

The win:

1. **One pump loop.** `Server.Execute` reads `result.Stream` until
   it returns `(_, false, nil)`. Whether the stream emits 100
   historical lines and exits, or 100 historical lines and then
   broker rows forever, doesn't change the loop.
2. **Single planner.Plan call.** Today's follow path re-plans to
   read `FollowApp` and `FollowPatterns`. Coord already has the
   plan from the historical batch — reuse it.
3. **Symmetry with Execute.** `Execute` returns
   `*ExecuteResult { Stream, Notices }`. A `Follow=true` request
   returns the same shape, just with a stream that keeps emitting.
   Clients consume identically.

## What changes

### Coordinator.Execute signature

Add a `follow bool` parameter (or, cleaner, a `req` struct):

```go
type ExecuteRequest struct {
    Pipeline string
    TimeRange TimeRange
    Customer string
    UserID   string
    Follow   bool
}

func (c *Coordinator) Execute(req ExecuteRequest) (*ExecuteResult, error)
```

(Or keep four positional args plus a follow bool — bikeshed.
The struct is more extensible.)

When `Follow=false`, behavior is unchanged from today.

When `Follow=true`:

1. Plan the pipeline. Build the LineStream that emits the
   historical batch (same as today).
2. Wrap that stream in a `concatStream` that, after the historical
   stream exhausts, switches to a broker-backed stream.
3. Return the wrapped stream as `ExecuteResult.Stream`.

### concatStream

A composite LineStream:

```go
type concatStream struct {
    primary  compute.LineStream
    next     func() compute.LineStream  // called when primary exhausts
    current  compute.LineStream
    advanced bool
}

func (s *concatStream) Next(ctx context.Context) (compute.Line, bool, error) {
    if !s.advanced {
        line, ok, err := s.primary.Next(ctx)
        if ok { return line, true, nil }
        if err != nil { return line, false, err }
        // primary exhausted: advance to next.
        s.current = s.next()
        s.advanced = true
    }
    return s.current.Next(ctx)
}

func (s *concatStream) Close() error { ... }
```

This is general-purpose — could be used elsewhere too.

### brokerStream

A LineStream that wraps a TailBroker subscription:

```go
type brokerStream struct {
    ch     <-chan *pb.LogRow
    filter func(*pb.LogRow) []string  // BuildLiveFilter result
    sub    func()  // Unsubscribe
    queue  []compute.Line  // when filter() emits multiple lines for one row
}

func (s *brokerStream) Next(ctx context.Context) (compute.Line, bool, error) {
    if len(s.queue) > 0 {
        line := s.queue[0]
        s.queue = s.queue[1:]
        return line, true, nil
    }
    for {
        select {
        case <-ctx.Done():
            return compute.Line{}, false, ctx.Err()
        case row, ok := <-s.ch:
            if !ok {
                return compute.Line{}, false, nil
            }
            lines := s.filter(row)
            if len(lines) == 0 { continue }
            s.queue = make([]compute.Line, len(lines))
            for i, ln := range lines {
                s.queue[i] = compute.Line{LineNum: row.LineNum, Raw: ln}
            }
            // pop first, save rest in queue
            line := s.queue[0]
            s.queue = s.queue[1:]
            return line, true, nil
        }
    }
}

func (s *brokerStream) Close() error {
    s.sub()  // unsubscribe
    return nil
}
```

### Coordinator's follow assembly

Inside `Coord.Execute` for `Follow=true`:

```go
plan, err := planner.Plan(req.Pipeline)
if err != nil { return nil, err }

// Historical batch (same as non-follow).
historical := executeBatch(plan, ...) // returns LineStream

// Live broker stream — created lazily when historical exhausts.
liveFactory := func() compute.LineStream {
    matcher := &RowMatcher{App: plan.FollowApp, Patterns: plan.FollowPatterns}
    filter := BuildLiveFilter(plan)
    ctx := context.Background()  // or thread through
    id, ch := c.Tail.Subscribe(ctx, matcher)
    return &brokerStream{
        ch:     ch,
        filter: filter,
        sub:    func() { c.Tail.Unsubscribe(id) },
    }
}

return &ExecuteResult{
    Stream:  &concatStream{primary: historical, next: liveFactory},
    Notices: nil,
}, nil
```

### Server simplification

`Server.Execute` collapses to one loop:

```go
func (s *Server) Execute(req, stream) error {
    customer, err := callerCustomer(stream.Context())
    if err != nil { return err }

    tr := tomorrowRange(req)  // existing helper
    res, err := s.Coord.Execute(coordinator.ExecuteRequest{
        Pipeline: req.Pipeline,
        TimeRange: tr,
        Customer: customer,
        UserID: req.UserId,
        Follow: req.Follow,
    })
    if err != nil { /* log + return */ }

    // Pump loop — same for batch and follow modes.
    ctx := stream.Context()
    notices := convertNotices(res.Notices)
    var batch []*pb.Line
    var stderrAcc strings.Builder
    firstBatchSent := false

    flush := func(final bool) error { /* same as today */ }

    for {
        line, ok, err := res.Stream.Next(ctx)
        if err != nil { return err }
        if !ok { break }
        if line.Stderr {
            stderrAcc.WriteString(line.Raw)
            stderrAcc.WriteByte('\n')
            continue
        }
        batch = append(batch, &pb.Line{Raw: line.Raw, LineNum: line.LineNum})
        if len(batch) >= 500 {
            flush(false)
        }
    }
    return flush(true)
}
```

The `if req.Follow` branch goes away. The follow-specific
infrastructure (`RowMatcher`, `BuildLiveFilter` direct calls,
broker subscribe/unsubscribe wiring) all moves into Coordinator.

## Testing

Existing follow tests live in `coordinator/live_filter_test.go` and
`integration/`. After the refactor:

- `coordinator/concatstream_test.go` (new) — direct tests of
  `concatStream`'s primary→next transition. Doesn't require a live
  broker.

- `coordinator/brokerstream_test.go` (new) — direct tests of
  `brokerStream`. Synthesize a TailBroker channel with test rows;
  assert filter is applied; assert ctx cancellation closes the
  stream.

- Update `coordinator_service_test.go` follow test (if any) to
  consume the unified stream rather than fork.

- Integration: existing `tail -f` flows in `integration/` keep
  working unchanged. New: assert that during follow mode, the
  client receives historical lines first, then live broker rows
  appear after a synthetic ingest.

## Estimated effort

- `concatStream`, `brokerStream`: ~150 lines + tests. Half a day.
- Coord.Execute follow assembly: ~80 lines. Hour or two.
- Server.Execute simplification: ~80 lines deleted. Hour.
- Test updates: maybe an hour.
- Total: ~1d.

## Risk

- **ctx threading.** Today's `Server.Execute` uses
  `stream.Context()` for the broker subscription. The unified
  Coord.Execute needs to receive that ctx (currently Coord doesn't
  take a ctx). Either: change the Coordinator API to take ctx, or
  use a context.Background() for the broker subscription (no
  cancellation propagation, leaks until grpc-server-side ctx
  cancellation closes the channel anyway).

  The clean fix is adding ctx to the Coord.Execute API. Worth
  doing, ~10-line change across calls.

- **First-line latency in concat mode.** When the historical
  batch is empty (no matching log rows), the user expects to see
  live rows immediately. concatStream advances to broker as soon
  as primary exhausts, so this works — but verify no spurious
  delay.

- **Follow + non-log pipeline.** Today's server doesn't validate
  that follow only applies to log queries. After the refactor,
  Coord.Execute should reject `Follow=true` for non-log pipelines
  (broker subscribes to log shards, not to anything else).

## Non-goals

- **Materializing follow into a database.** Today's broker is
  in-memory and per-coordinator. Persisting follow output across
  restarts is a separate concern (cron-style watches handle that
  via cron_results storage).

- **Backpressure between historical and live.** If the client
  consumes slowly during the historical phase, broker rows pile up
  before the transition. Today's broker has bounded subscriber
  channels (lossy on overflow). Same behavior after the refactor.
  See `streaming-backpressure.md` for tuning.

- **Removing per-row LiveFilter execution.** `BuildLiveFilter`
  re-runs the user's post-source pipeline stages against each
  broker row to keep the live tail's projections consistent with
  the historical batch (`tail -f log | cut -d' ' -f3`). The
  refactor preserves this. Optimizing it (or factoring out into
  compute) is independent.

## Cross-link

- `end-to-end-streaming.md` step 2 — once Coord.Execute returns
  the compute stream directly, this plan composes cleanly:
  `concatStream{primary: directComputeStream, next: brokerStream}`.
- `streaming-backpressure.md` — broker channels are a backpressure
  source; the bounded-queue work there applies.
