# Plan: backpressure tuning for the streaming path

## Status

Designed, not started. Tuning concern that surfaces only once
`end-to-end-streaming.md` step 1 (online k-way merge) lands —
before that, all rows are buffered before merging anyway, so
backpressure doesn't apply.

## Why

The streaming path introduces several queues, each unbounded by
default. For the workloads we run today (single-digit shards,
moderate result sizes), unbounded is fine. The plan captures the
mechanism for when it isn't.

### Queue 1: per-shard cursor queues (online merge)

Once `LocalComputeJob.consume` switches to an online heap (step 1
of the streaming plan), each shard has its own queue of arrived-
but-not-yet-merged rows. The heap can only emit when every active
cursor has at least one row queued. If shard A is fast and shard
B is slow, A's rows pile up waiting for B's first row.

```
shard A queue:  [row1, row2, ..., row1000]   ← grows unbounded
shard B queue:  []                            ← blocks heap emission
```

For pathological cases (one shard 100x faster, very large result),
A's queue can grow to gigabytes before B catches up.

### Queue 2: streamCh

`LocalComputeJob` pushes Lines into `streamCh chan Line` buffered
at 1024. The gRPC server pumps them onto the wire. If the wire is
slow (slow client, network congestion), `streamCh` fills and
producers (the consume goroutine pushing merge output) block.

```go
streamCh = make(chan Line, 1024)  // ← fixed today
```

This is OK behavior when bounded — backpressure flows naturally
from the slow client back through the gRPC stream into the consume
loop. But the bound is hard-coded; large compute results with slow
clients hit it.

### Queue 3: gRPC stream batches

`Server.Execute` builds a 500-line batch buffer:

```go
const batchSize = 500
var batch []*pb.Line
```

Bounded by the batchSize constant. Flushes when full or at end-of-
stream. No memory issue.

## What changes

### Bound per-shard cursor queues

In `compute/online_merge.go` (the file `end-to-end-streaming.md`
step 1 will create), introduce a configurable per-shard queue
limit:

```go
type LocalComputePool struct {
    Parallelism      int
    PerShardQueue    int  // NEW; default 256
    StreamBuffer     int  // NEW; default 1024
}

type onlineCursor struct {
    shardID  string
    queue    []*pb.ResultRow
    sentinel bool

    // queueLimit blocks Send when len(queue) >= queueLimit.
    queueLimit int
}
```

When a shard goroutine sends to a full cursor's queue, the send
blocks. Backpressure propagates back to the goroutine running
`shard.Query` — which is currently synchronous, so the goroutine
just sits waiting. (Future work: streaming shard.Query lets the
shard slow down its read.)

For the gRPC path, this means: a slow consumer downstream blocks
streamCh receives → blocks consume's heap emission → blocks the
fastest cursor's queue from draining → blocks fast shard goroutines
from sending more rows → flow control reaches back to shard.Query.

### Configurable streamCh size

Pool option drives the buffer:

```go
func (p *LocalComputePool) NewJob(...) ComputeJob {
    bufferSize := p.StreamBuffer
    if bufferSize == 0 { bufferSize = 1024 }
    return &LocalComputeJob{
        // ...
        streamCh: make(chan Line, bufferSize),
    }
}
```

Default unchanged. Tunable for high-throughput deployments or
memory-constrained ones.

### Slow-consumer timeout (optional)

For fail-fast semantics, an optional per-job idle timeout: if
`Stream.Next()` isn't called for N seconds, the stream errors
with `ErrSlowConsumer`. Killed jobs free their queues immediately.

```go
func (s *channelLineStream) Next(ctx context.Context) (Line, bool, error) {
    if s.idleTimeout > 0 {
        ctx, cancel := context.WithTimeout(ctx, s.idleTimeout)
        defer cancel()
    }
    select {
    case <-ctx.Done():
        if errors.Is(ctx.Err(), context.DeadlineExceeded) && s.idleTimeout > 0 {
            return Line{}, false, ErrSlowConsumer
        }
        return Line{}, false, ctx.Err()
    case line, ok := <-s.ch:
        // ... unchanged ...
    }
}
```

This is **opt-in**, not default. The gRPC server's natural ctx
cancellation (when the client disconnects) is enough for most
cases.

### Metrics

Per-job high-water-marks for diagnostics:

```go
type LocalComputeJob struct {
    // ... existing ...
    maxQueueDepth atomic.Int64  // largest cursor queue len observed
    maxStreamCh   atomic.Int64  // largest len(streamCh) observed
}
```

Exposed via the `weft.metrics` builtin or just logged at job end.

## Testing

```go
func TestOnlineMerge_BoundedQueueBlocksFastShard(t *testing.T) {
    // pool.PerShardQueue = 10
    // shard A sends 100 rows back-to-back; shard B sends 0 (no
    // sentinel either).
    // Assert: A's goroutine blocks after sending 10 rows
    //         (queue limit reached, B not in heap, no emission).
    // Send B's first row; assert A unblocks (heap can advance,
    // pop A's first row, A's queue drops to 9).
}

func TestOnlineMerge_StreamChFillsCausesProducerBlock(t *testing.T) {
    // pool.StreamBuffer = 4
    // Don't drain Stream. Send rows.
    // After 4 rows queued in streamCh, the consume goroutine
    // should block on push (verify via channel capacity tracking).
}

func TestStream_SlowConsumerTimeout(t *testing.T) {
    // Build job with IdleTimeout=100ms.
    // Don't call Next. After 100ms, Next returns ErrSlowConsumer.
}

func TestExecute_BackpressureUnderLargeQuery(t *testing.T) {
    // Integration: 3 shards, each returns 10k rows, 1 fast 2 slow.
    // Memory peak should be bounded by:
    //   3 cursors × PerShardQueue rows + StreamBuffer + batchSize
    //   = 3 * 256 + 1024 + 500 ≈ 2300 row-equivalents
    // Not 30k.
}
```

## Estimated effort

- Online merge with bounded queues: ~50 lines on top of step 1.
- Pool option wiring: ~30 lines.
- Slow-consumer timeout: ~50 lines (mostly tests).
- Metrics: ~30 lines.
- Tests: ~150 lines.
- Total: ~half a day, dependent on `end-to-end-streaming.md` step 1.

## When to land it

Defer until production hits a memory event traceable to one of
these queues. Today's workloads (~6 shards, modest result sizes,
co-located clients) don't stress them. Triggers for taking this
plan off the shelf:

- A coordinator OOM where heap profile shows row buffer growth.
- A user complaint about "it took 30 seconds before any output."
- A deployment plan to scale shard count past ~30 (the merge's
  per-cursor head buffer becomes meaningful).

Until then, the unbounded defaults match the unbounded scale of
problems we're solving.

## Non-goals

- **Reordering rows under backpressure.** When a queue blocks,
  upstream waits. Output ordering is preserved.

- **Spilling to disk.** No "too big for memory? write to disk"
  fallback. If memory bounds matter, configure smaller queues or
  use a streaming shard.Query (separate plan). Disk spill is
  complex and not justified for a feature that currently has no
  customer.

- **Cross-job fairness.** Multiple concurrent ExecuteResult
  streams compete for goroutines + memory like any other Go
  workload. The compute pool's `Parallelism` setting controls
  bash-side concurrency; nothing here changes that.

## Cross-link

- `end-to-end-streaming.md` step 1 (online k-way merge) — this
  plan tunes the queues introduced there.
- `compute-scaling.md` — if the compute tier scales horizontally,
  per-job memory caps become more important (one bad job
  shouldn't crash the whole pool).
- `follow-via-execute.md` — broker subscriber channels have their
  own bounded buffers; same backpressure principles apply.
