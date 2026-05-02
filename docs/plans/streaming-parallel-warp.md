# Plan: streaming + chunked parallelism in warp

## Status

Designed, not started. Cross-link from
`docs/plans/end-to-end-streaming.md` step 3 — that step landed
`bash.RunStreaming` using warp's stage-level pipeline parallelism
(each stage's own goroutine, connected by `io.Pipe`). What it did
NOT land was *intra-stage* (chunked) parallelism: one stage running
on N workers in parallel, with the input split across them. Today
warp has chunked parallelism, but only via `execParallelPipeline`,
which slurps stdin first — incompatible with streaming.

This plan covers extending warp so a single API call can simul-
taneously stream AND parallelize within each stage. The work lives
entirely in warp; no weft-side changes are required to the
coordinator or compute layers.

## Why

Today's two execution paths in warp:

| Path | Stage-level parallelism | Intra-stage parallelism | Streams |
|---|---|---|---|
| `execPipelineMulti` (Parallelism ≤ 1) | yes (one goroutine per stage) | no | yes |
| `execParallelPipeline` (Parallelism > 1) | no (segments run in series) | yes (chunks → N workers → merge) | no — slurps stdin |

Weft's compute path uses `RunStreaming`, which always picks
`execPipelineMulti`. That gives stage-level parallelism for free
(`grep | awk | sort` runs three goroutines on three cores). But
when one stage processes a large stream — say `awk` over 10M lines
— it's bottlenecked on a single core.

Real workloads where this matters:

- Wide regex `awk` patterns over high-volume logs after pushdown
  trims the WHERE clause but leaves a large remainder.
- `jq` filters with expensive selectors over JSONL streams.
- `sed` substitutions where the regex is non-trivial.

For all of these, the win is stage-internal CPU parallelism. The
existing chunked path does this, but only for in-memory inputs.
This plan extends streaming so chunking happens lazily.

## Design

### Stage classification (already partly there)

Reuse `pkg/bash/classify_aggregating.go`. Each stage falls into
one of four categories:

1. **Per-line stateless** — `grep`, `cut`, `tr`, `cat`, `rev`,
   `awk` without END/cross-line state, `sed` with no hold-space
   use, `jq` (line-delimited mode). Embarrassingly parallel: any
   line can run on any worker, outputs reassembled in original
   order.
2. **Order-rearranging reducible** — `sort`. Each worker sorts its
   chunk, a final k-way merge produces the global order. Already
   implemented in `execParallelSort`; refactor to consume a stream
   instead of a string.
3. **Aggregating reducible** — `wc`, `awk` with END, `sort | uniq
   -c` (reduces to (key, count) pairs). Each worker computes a
   partial aggregate; a reducer combines them. Already imple-
   mented in `execParallelWc` and `execParallelAwkReduce`;
   refactor to streaming.
4. **Inherently sequential** — `head -N`, `tail -N`, `tac`,
   `uniq -c`, `tee` to files, `sed` with hold space, `awk` with
   `getline` or shared arrays. Bypass chunking; run on a single
   worker with stdin/stdout passthrough.

The classifier returns one of these tags per stage. The pipeline
runner uses the tag to pick the right execution shape.

### The streaming chunk dispatcher

For per-line stateless and reducible stages, a dispatcher
goroutine reads the stage's stdin and fans out to N workers. The
unit of dispatch is a *block* of K lines (K=64 or K=256 — tune
empirically), not individual lines, to amortize goroutine wakeup
and channel send cost.

```
                  ┌──────────────┐
                  │ stdin reader │  ← previous stage's stdout pipe
                  └──────┬───────┘
                         │ blocks of K lines, tagged with seq #
                         ▼
                  ┌──────────────┐
                  │ work queue   │  buffered chan, capacity = N × 2
                  └──┬─────┬─────┘
            ┌────────┘     │     └────────┐
            ▼              ▼              ▼
         worker 0       worker 1   …  worker N-1
            │              │              │
            └────┬─────────┴───────┬──────┘
                 │ tagged outputs  │
                 ▼                 ▼
              ┌──────────────────────┐
              │ ordered reassembler  │  emits blocks in seq order
              └──────────┬───────────┘
                         │
                         ▼
                   stdout pipe → next stage
```

Each block carries a monotonic sequence number assigned by the
dispatcher. Workers process blocks independently and push tagged
outputs onto a results channel. The reassembler holds outputs in a
small buffer keyed by sequence number, emitting block N+1 only
after block N has emitted. For per-line stateless stages this is
sufficient — line order within a block is preserved by the worker,
order across blocks is preserved by the reassembler.

For reducible stages (sort/wc/awk-END), there is no per-block
output to reassemble — workers produce partial results, a final
reducer combines them after the dispatcher reaches EOF. The
existing `execParallelSort` / `execParallelWc` /
`execParallelAwkReduce` reducers carry over with the reader-side
swapped from `splitLines(string)` to a streaming line reader.

### Per-worker state isolation

Each worker is a fresh `Interpreter` instance for the stage,
created via `interp.fork()`. Variables, file maps, and any other
interpreter state are copied at fork time and isolated thereafter.
Stages that mutate process-wide state (file system writes via
`tee`) are in the "inherently sequential" category and don't fan
out.

The `awk`/`sed`/`jq` packages already have stateless `Run` entry
points that take an interpreter or no shared state. Each worker
calls into them with its own goroutine-local state.

### Stage execution shape

The new top-level execution function:

```go
// RunStreamingParallel runs a script with end-to-end streaming AND
// per-stage chunked parallelism. parallelism > 1 enables N workers
// per chunkable stage; parallelism ≤ 1 falls through to existing
// RunStreaming behavior (stage-level only).
//
// stdin/stdout/stderr are forwarded through the pipeline. Each
// stage runs in its own goroutine; chunkable stages additionally
// fan out to N workers internally.
func RunStreamingParallel(
    script string,
    stdin io.Reader,
    stdout io.Writer,
    stderr io.Writer,
    parallelism int,
) (exitCode int, err error)
```

Internal pipeline construction:

1. Parse the script.
2. For each stage, look up the classification tag.
3. Build the inter-stage pipe chain (stage N's stdout = stage N+1's stdin).
4. For each stage, spawn its execution goroutine:
   - Inherently sequential → existing single-worker path.
   - Per-line stateless → dispatcher + N workers + reassembler.
   - Reducible → dispatcher + N workers + reducer.
5. Wait for the last stage to flush; close its stdout; wait for
   wg.

`RunStreaming` (the existing API) becomes a thin wrapper:
`RunStreamingParallel(..., 1)`.

### Threshold

For small inputs, parallelism overhead exceeds the win. The
dispatcher runs single-threaded until it has read enough to fill
one block per worker (roughly K × N lines); below that threshold,
it falls through to the single-worker path. This avoids
pessimizing short pipelines without adding configuration knobs.

## Implementation

Three commits, in this order:

### Commit 1 — streaming chunk dispatcher

`pkg/bash/parallel_streaming.go` (new file):

- `streamingDispatcher` struct (line reader, work queue, sequence counter).
- `streamingReassembler` struct (small ordered buffer, in-order emit).
- `runChunkedStreamingStage(cmd Command, stdin io.Reader, stdout
  io.Writer, parallelism int) (exitCode int, err error)` for
  per-line stateless stages.
- Tests: `parallel_streaming_test.go` exercising the dispatcher
  and reassembler in isolation.

~300 lines + ~200 lines tests.

### Commit 2 — streaming reducers

Refactor `execParallelSort` / `execParallelWc` /
`execParallelAwkReduce` to consume a streaming line reader instead
of a string. They keep their reduce-shape (per-worker partial,
final merge); only the input side changes.

`pkg/bash/parallel.go` becomes shorter as the duplicated
"split input into chunks" logic is extracted into a single
streaming chunker. The batch entry points
(`execReducibleSegment`) wrap the streaming version with a
`strings.NewReader`.

~150 lines refactor + ~150 lines tests.

### Commit 3 — top-level entry + classification glue

`pkg/bash/bash.go`: add `RunStreamingParallel`. Reuse the existing
classification logic from `classify_aggregating.go` (extend if
necessary). `RunStreaming` becomes a 1-line wrapper.

`pkg/bash/exec.go` `execPipelineMulti`: when parallelism > 1, each
stage is dispatched to either a chunked-streaming runner or the
single-worker fallback based on its tag.

~100 lines + ~150 lines tests including a few end-to-end pipelines
exercising the full stack.

## Testing

Unit tests:

- Dispatcher correctness: lines in → blocks out, sequence numbers monotonic.
- Reassembler correctness: out-of-order blocks in → in-order emit.
- Per-stage adapters: each chunkable stage produces the same
  output as the single-worker path on the same input. Run for
  many input shapes (empty, single line, exactly K lines, K+1,
  N×K, mid-block EOF).

Integration tests:

- `cat | grep ERROR | head -10` — mixed chunkable + sequential stages.
- `cat | awk '$3 == "vimgym"' | sort` — per-line + reducible.
- `cat | jq '.x' | wc -l` — line-delimited jq + aggregating.
- `cat | sed 's/foo/bar/' | uniq -c` — sed + sequential.

Race tests: every commit's test suite runs under `-race`. The
existing pre-existing race in `execParallelSort` (parallel.go:1043
↔ exec.go:746) gets fixed as a side-effect of refactoring through
a shared streaming chunker — the race comes from forking
interpreters in tight loops, and the refactor moves fork to
once-per-worker at startup.

Property tests: ~100 random pipelines, run with parallelism=1 and
parallelism=4, assert byte-identical output. This is the strongest
correctness signal for chunked parallelism.

Performance tests:

- Synthetic 10M-line input piped through `awk '/[a-z]+/ {print $1}'`.
- Compare wall time at parallelism=1, 2, 4, 8.
- Expect roughly linear speedup up to memory-bandwidth limits.

## Risks

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Order reassembly bugs cause occasional wrong-order output | Med | High | Property tests with 4-worker vs 1-worker comparison; include random delays in workers to expose race |
| State leakage between workers | Low | High | Worker forks at startup, never shares interpreter state; assert via test that mutating a var in worker 0 doesn't affect worker 1 |
| Memory pressure: many concurrent stages × many workers each | Med | Med | Bounded work queue (capacity = N × 2 per stage); unbounded queues forbidden |
| Performance pessimization on short inputs | Med | Low | Threshold gate — don't fan out below K × N lines; falls through to single-worker |
| Stage classification miss (chunking a stage that isn't safe) | Low | High | Conservative classifier — unknown stages are sequential by default; only stages explicitly tagged as chunkable get fanned out |
| Vimgym surface change | Low | Low | API is purely additive; existing `Run`, `RunWithParallelism`, `RunStreaming` all unchanged |

## Out of scope

- **Distributed parallelism.** Workers are goroutines on one host. A
  distributed compute pool (per `compute-scaling.md`) is a separate
  plan.
- **Adaptive worker count.** Parallelism is fixed at the API call.
  Auto-tuning based on stage progress is future work.
- **Stage fusion across pipes.** `awk '...' | grep '...'` could
  theoretically fuse into one fused interpreter to skip the pipe;
  warp's existing `fuseSegments` already does this for the batch
  path. Extending fusion to streaming is a separate optimization.
- **Backpressure semantics across stages.** Today the pipe between
  stages is `io.Pipe` which provides natural backpressure (writer
  blocks when reader is slow). Same applies after this plan.

## Cross-links

- `end-to-end-streaming.md` — the parent plan; step 3 added
  `RunStreaming` (stage-level streaming only). This plan adds the
  intra-stage layer.
- `streaming-backpressure.md` — out-of-scope for this plan but
  relevant: bounded queues here interact with the per-shard queue
  bounds there.
- Warp internal: `pkg/bash/parallel.go` and
  `pkg/bash/classify_aggregating.go` are the existing chunked-
  parallel and classification code; this plan refactors them.

## Estimated effort

- Commit 1 (dispatcher + reassembler): ~1 day.
- Commit 2 (streaming reducers): ~1 day.
- Commit 3 (top-level wiring + integration tests): ~1 day.
- Property + perf tests + bug iteration: ~1 day.

Total: roughly 4 days of focused work. Half is tests.
