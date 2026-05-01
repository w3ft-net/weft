# Plan: end-to-end streaming output

## Status

**Wire + public API in place** as of phase 4 (commits `a5947cf`,
`49dd5e9`, `0202d16`, `8745e98`). The producer side of compute now
pushes lines into a channel; `Coordinator.Execute` returns
`*ExecuteResult { Stream, Notices, ... }`; `Server.Execute` pumps the
stream onto the wire batch-by-batch.

The middle layer still buffers — the Stream is currently a
slice-replay over a fully-materialized `*QueryResult`. To deliver
true incremental delivery (lines arrive at the client as soon as the
slowest active shard produces them), three additional changes are
required, in this sequence:

1. **Online k-way merge in compute** — eliminate the
   wait-for-all-sentinels stage in `LocalComputeJob.consume`.
2. **Forward compute stream from coord** — replace `c.query`'s
   QueryResult intermediate with a passthrough to the compute
   stream for the common case (zero-stage, no alert, no redirect).
3. **Streaming warp exec for stream-safe stages** — let staged
   pipelines (`grep`, `awk` without END, `sed`) emit lines as the
   bash pipeline consumes them.

Each step delivers some win on its own and is independently
testable. (1) before (2) is required: (2) without (1) just
relocates the buffer from coord into compute. (3) is optional —
useful for big results that survive a stream-safe filter, no help
for batch-only stages (`sort`, `wc`, `uniq`, `awk` with `END`).

## Why it exists

Today's chain (after phase 4):

```
shard → compute.LocalComputeJob.consume (BUFFERS all rows + sentinels)
      → mergeRowsFull (single pass over fully-populated map)
      → streamCh
      → coord.executeQuery drains streamCh into QueryResult
      → c.query post-processing (alert, redirect, …)
      → coord.Execute wraps QueryResult in ExecuteResult (slice replay)
      → server.Execute pumps stream onto wire
      → client
```

The `consume` step gates everything: no line moves toward the wire
until every shard has reported its full result *and* a sentinel.
For a query like `tail -n 5000 /log/foo.log` over 12 shards, that's
60k rows accumulated in coord-side memory before the first byte
reaches the client.

The win is twofold:

1. **Time-to-first-byte.** Once each shard has emitted its first
   row, the merge can pick the global lowest and ship it. With
   `tail`, the first line lands within milliseconds of the slowest
   shard's first row arriving.
2. **Memory pressure.** Coordinator no longer holds all rows — at
   most one row per shard is queued in the heap at any time. The
   server's batch buffer (500 lines) is the only post-stream
   buffer.

The cost is implementation complexity: the online merge is more
finicky than the synchronous pass, and `c.query` has to fork
between the streaming-safe path and the buffer-required paths
(alert, redirect, staged pipelines).

## Inherent buffering points (unavoidable)

Some pipeline shapes can't stream — calling them out explicitly so
the streaming path doesn't have to defend against them:

- **Cross-shard order requires one row per shard.** The k-way merge
  needs at least one queued row from every active cursor before it
  can emit. Latency floor is the slowest shard's first-row latency,
  not the total result latency. Online and unavoidable.
- **`sort` (terminal or anywhere)**, **`wc -l`**, **`uniq`** — these
  emit only at end-of-input. Streaming the *output* makes no sense
  because there is no output until the input is exhausted. The
  staged path keeps batch semantics here.
- **`awk` with an `END` block** — same as sort/wc. END blocks
  inherently terminal.
- **Alert (`| alert ...`)** — alert decisions look at counts and
  aggregates over the full result. Body must drain before alert
  fires. Coord-level buffering required.
- **Redirect (`> /tmp/...`)** — shellfs writes are not append-as-you-go
  from the coord side. Body must drain before write. Coord-level
  buffering required.
- **Multi-stage `compute` jobs** — until step 3 lands, warp's
  `bash.RunWithParallelism` is non-streaming. All staged pipelines
  buffer at the warp boundary regardless of whether the stages
  themselves are stream-safe.

The streaming path is thus: zero-stage log queries, no alert, no
redirect. That matches the most common shape (`cat /log/foo.log`,
`cat /log/foo.log | head -n N`, `cat /log/foo.log | grep ERROR`
with grep pushed down into SQL).

---

# Step 1 — Online k-way merge in compute

## What changes

`compute/local.go` `LocalComputeJob.consume` and `compute/merge.go`
`mergeRowsFull`.

Today:
```go
// consume.go
for sentinels < j.shardCount {
    msg := <-j.ch
    if msg.Sentinel { sentinels++; continue }
    perShard[msg.ShardID] = append(perShard[msg.ShardID], msg.Row)
}
merged := mergeRowsFull(perShard, j.mergeKey)  // single-shot
for _, r := range merged { j.streamCh <- toLine(r) }
```

After:
```go
// consume reads messages incrementally, pushing each msg into a
// per-shard cursor, and emits the smallest-leading row whenever
// every active cursor has at least one queued row OR a sentinel
// (signaling that cursor is done).

cursors := make(map[string]*onlineCursor)
heap := newOnlineHeap(j.mergeKey)
sentinels := 0

for sentinels < j.shardCount {
    msg := <-j.ch
    if msg.Sentinel {
        sentinels++
        cur := cursors[msg.ShardID]
        if cur != nil { cur.sentinel = true }
        // Sentinel may unblock the heap (if this shard was the
        // blocking one). Try to advance.
        emitReady(heap, cursors, j)
        continue
    }
    if msg.Row == nil { continue }
    cur, ok := cursors[msg.ShardID]
    if !ok {
        cur = &onlineCursor{shardID: msg.ShardID}
        cursors[msg.ShardID] = cur
        heap.add(cur)
    }
    cur.queue = append(cur.queue, msg.Row)
    emitReady(heap, cursors, j)
}
// All sentinels in: drain remaining queues.
emitRemaining(heap, cursors, j)
```

Where `emitReady` is the streaming-merge inner loop:
```go
// While every cursor in the heap has at least one queued row OR
// has been sentinel'd, pop the smallest leading row and emit.
// Stops when some cursor is "blocked" — has no queued rows and no
// sentinel yet.
func emitReady(heap *onlineHeap, cursors map[string]*onlineCursor, j *LocalComputeJob) {
    for {
        // Check: are all cursors ready (queued OR sentinel'd)?
        if !heap.allReady() { return }
        // All ready. Pop the smallest leading row.
        cur := heap.minLeader()
        row := cur.queue[0]
        cur.queue = cur.queue[1:]
        emit(j, row)
        // If cur is empty AND not sentinel'd, it's now blocked.
        // Heap order preserved either way.
        if len(cur.queue) == 0 && cur.sentinel {
            heap.remove(cur)  // cursor is done
        }
        if j.limit > 0 && j.emitted >= j.limit {
            return  // global cap reached
        }
    }
}
```

`onlineHeap` keeps cursors ordered by their *current* leading row.
When a cursor's leading row is consumed (popped), the cursor's new
leader (the next queued row) determines its new heap position. If
the cursor is empty and not sentinel'd, it's "blocked" — no further
emission until it gets more rows or its sentinel.

The heap interface looks roughly like:

```go
type onlineCursor struct {
    shardID  string
    queue    []*pb.ResultRow  // FIFO
    sentinel bool
}

type onlineHeap struct {
    cursors []*onlineCursor
    less    func(a, b *pb.ResultRow) bool  // from j.mergeKey
}

// allReady returns true iff every cursor in the heap has queue len > 0
// or sentinel == true. When false, the merge has to wait.
func (h *onlineHeap) allReady() bool { ... }

// minLeader returns the cursor whose leading queued row is smallest.
// Skips cursors that are sentinel'd with empty queues (those are
// effectively "done" — caller should remove them).
func (h *onlineHeap) minLeader() *onlineCursor { ... }
```

A simple unsorted slice + linear scan is fine — typical N = 2-12
shards, so heap-of-12 with O(N) operations is faster in practice
than a binary heap.

## Edge cases

- **Zero shards.** `j.shardCount == 0`. Today `consume` exits the
  for-loop immediately and produces no output. After: same.
- **One shard.** Single cursor. As soon as that cursor has a row OR
  sentinel, it's ready (alone). Emits each row immediately. Sentinel
  ends the stream. Strict improvement over today.
- **Slow shard.** If shard A has emitted 1000 rows but shard B is
  slow, the heap holds 1000 rows for A and 0 for B. No emission
  until B sends its first row OR sentinel. Memory grows linearly
  with the leader gap — same as today, just with shorter tails.
- **Empty shard.** Shard A immediately sends sentinel with no rows.
  The cursor is added with `sentinel=true, queue=[]` and the heap
  treats it as "done" (skip in minLeader). Other cursors emit
  freely.
- **Limit.** `j.limit > 0` truncates emission at N rows globally.
  Cursors past the limit get drained but their rows are dropped.
  (Or: stop the consume loop, send a synthetic sentinel? Simpler:
  count emitted and short-circuit.)
- **`mergeKey == ""` (concat path).** Falls back to "first available
  cursor wins" — emit FIFO across cursors as rows arrive. Equivalent
  to today's concat-in-arrival-order, just streaming.

## Testing

`compute/online_merge_test.go` — direct exercises of the merge
without going through `LocalComputeJob`.

Cases (all-positive — merge correctness under various arrival
orders):

```go
func TestOnlineMerge_LineNumAsc_FullyOrdered(t *testing.T) {
    // shard A: rows 1, 3, 5 in order
    // shard B: rows 2, 4, 6 in order
    // expected emission: 1, 2, 3, 4, 5, 6
}

func TestOnlineMerge_OneShardLeadsTheOther(t *testing.T) {
    // shard A sends 1, 2, 3 first
    // shard B sends 4, 5, 6 after
    // expected emission still in line_num order: 1, 2, 3, 4, 5, 6
    // (because A's 1, 2, 3 are buffered until B's first arrives)
}

func TestOnlineMerge_ShardSentinelMidstream(t *testing.T) {
    // shard A: rows 1, 5; sentinel after.
    // shard B: rows 2, 3, 4, 6; sentinel after.
    // After A sends 1: blocked (B not in heap yet).
    // After B sends 2: emit 1 (A's only queued, then re-blocked
    // because A queue is empty but not sentinel'd yet).
    // After A sentinels: remove A. Now B drains alone: 2, 3, 4.
    // After A's previous queue (5) is processed... wait, A sent 5
    // before sentinel, so before sentinel lands the queue is [1, 5].
    // Actual order depends on arrival timing — write the test with
    // explicit event ordering.
}

func TestOnlineMerge_EmptyShardSentinelOnly(t *testing.T) {
    // shard A: rows 1, 2, 3; sentinel after.
    // shard B: sentinel only, no rows.
    // expected emission: 1, 2, 3.
}

func TestOnlineMerge_LineNumDesc(t *testing.T) {
    // mergeKey = "line_num DESC"
    // shard A: 5, 3, 1 (descending)
    // shard B: 6, 4, 2
    // expected: 6, 5, 4, 3, 2, 1
}

func TestOnlineMerge_RawAsc(t *testing.T) {
    // mergeKey = "raw"
    // shard A: alpha, gamma
    // shard B: beta, delta
    // expected: alpha, beta, gamma, delta
}

func TestOnlineMerge_ConcatModeArrivalOrder(t *testing.T) {
    // mergeKey = ""
    // arbitrary arrival order; output is FIFO by arrival.
}

func TestOnlineMerge_GlobalLimit(t *testing.T) {
    // 3 shards × 10 rows each, limit=5.
    // Expected emission: exactly 5 rows, in mergeKey order.
}
```

Plus an end-to-end test through `LocalComputeJob`:

```go
func TestLocalComputeJob_OnlineStreaming(t *testing.T) {
    // Set up job. Send rows from shard A. Verify Stream.Next blocks
    // (use ctx with 50ms timeout, expect ctx.Err()).
    // Send first row from shard B. Now Stream.Next should return
    // shard A's first row immediately (smallest line_num).
    // Continue and verify the full ordering.
}
```

The block-then-unblock test pins the streaming property: the merge
emits BEFORE all shards have completed.

## Compatibility with existing tests

The existing `compute/merge_test.go` exercises `mergeRows` /
`mergeRowsFull` directly. Both functions keep working — they're
the synchronous batch helpers. `consume` no longer calls them; the
online heap handles the merge incrementally.

`mergeRowsFull` could be deprecated and deleted after the online
path is in production for a release cycle. Until then, it's fine to
keep — `merge_test.go` continues to pin its semantics, which the
online heap mirrors.

## Estimated scope

- ~150 lines of new code in `compute/online_merge.go` (new file:
  `onlineCursor`, `onlineHeap`, `emitReady`, `emitRemaining`).
- ~50 lines refactor in `compute/local.go` `consume`.
- ~200 lines of new test code.
- 0 lines change in callers — `LocalComputeJob.Stream()` already
  returns a channel-backed stream; consumers see the same surface.

Total: ~500 lines, half tests. One commit.

---

# Step 2 — Forward compute stream from coord

## What changes

`coordinator/coordinator.go` — `c.query` and `executeQuery` fork
into a streaming path and a buffering path based on whether
post-processing is needed.

Today's `c.query` (post-phase-4):
```go
func (c *Coordinator) query(...) (*QueryResult, error) {
    // resolve customer, expand /bin, expand globs
    plan, err := planner.Plan(pipeline)
    if errors.Is(err, planner.ErrNotPlannable) {
        // non-log: runLocal returns *QueryResult
        ...
    }
    result, err := c.executeQuery(plan, ...)  // *QueryResult — drains compute
    if plan.AlertArgs != nil { runAlertStage(result, ...) }
    if plan.RedirectTarget != "" { c.writeQueryResultToFS(result, ...) }
    return result, nil
}
```

After:
```go
func (c *Coordinator) execute(...) (*ExecuteResult, error) {
    // resolve customer, expand /bin, expand globs
    plan, err := planner.Plan(pipeline)
    if errors.Is(err, planner.ErrNotPlannable) {
        // non-log: runLocal stays buffered (warp limitation)
        return c.executeNonLog(...)
    }

    // Log query path. Decide: stream-direct or buffer-then-replay?
    needsBuffering := plan.AlertArgs != nil ||
                      (plan.RedirectTarget != "" && custID != 0) ||
                      len(plan.ComputeStages) > 0
    if needsBuffering {
        return c.executeLogBuffered(plan, ...)
    }
    return c.executeLogStreaming(plan, ...)
}

func (c *Coordinator) executeLogStreaming(plan, ...) (*ExecuteResult, error) {
    // Same shard-fanout setup as executeQuery, but:
    // - Pool.NewJob with stages=[] (zero-stage by precondition)
    // - Each shard goroutine pushes rows + sentinel as today
    // - Return ExecuteResult{Stream: job.Stream(), Notices: ...}
    // The caller (Server.Execute) drains the stream as compute
    // produces; no QueryResult intermediate.
}

func (c *Coordinator) executeLogBuffered(plan, ...) (*ExecuteResult, error) {
    // Existing executeQuery path: drain compute into QueryResult,
    // run alert/redirect, wrap in ExecuteResult via
    // queryResultToExecuteResult. Identical to today.
}
```

## What "needs buffering" means

The streaming path is taken iff *all* hold:

1. `plan.AlertArgs == nil` — no trailing `| alert`.
2. `plan.RedirectTarget == ""` — no trailing `> /tmp/...` (or
   `custID == 0`, in which case shellfs is unavailable anyway).
3. `len(plan.ComputeStages) == 0` — no post-merge bash transforms.

If any fail, fall through to the buffered path. The check is
constant-time on the plan struct.

## A subtle constraint: redirect

The current `c.query` writes `result.Rows = nil; result.Text = ""`
after `writeQueryResultToFS` so the user gets no terminal output.
The buffered path retains this. The streaming path doesn't enter
this branch (precondition rules redirects out), so no special-case
there.

Future cleanup: route redirects through warp's stdout via
`CallbackCommand.Redirects` (the phase-3 carryover noted in
commit `d5b30cc`). When that lands, redirect can flow through the
streaming path because warp's redirect target is a writer that the
shellfs FS can supply. Until then, buffer-then-write stays.

## What the streaming path looks like

```go
func (c *Coordinator) executeLogStreaming(
    plan *planner.QueryPlan, tr TimeRange,
    customer string, custID uint, userID string,
) (*ExecuteResult, error) {
    sql := planner.InjectCustomer(plan.DirectShardSQL, customer)
    var tier store.Tier
    if custID != 0 { tier = c.Store.TierForCustomer(custID) }
    matching := c.Registry.MatchingShards(tr, plan.FollowApp, tier)
    if len(matching) == 0 {
        return &ExecuteResult{
            Stream: compute.NewSliceLineStream(nil, nil),
        }, nil
    }
    // Source-app allowlist gate (unchanged).
    if plan.SourceApp != "" {
        if err := c.checkSourceAllowlist(customer, plan.SourceApp); err != nil {
            return nil, err
        }
    }

    // Stages are empty by precondition, so the compute job is
    // pure merge + global limit. Forward its Stream directly.
    job := c.Pool.NewJob(nil, len(matching), plan.ShardOrderBy, plan.Limit)
    var wg sync.WaitGroup
    wg.Add(len(matching))
    errs := make(chan error, len(matching))
    for _, s := range matching {
        go func(s shard.Shard) {
            defer wg.Done()
            defer job.SendSentinel(s.ID())
            rows, err := s.Query(sql)
            if err != nil {
                errs <- fmt.Errorf("shard %s: %w", s.ID(), err)
                return
            }
            for _, r := range rows {
                job.SendRow(r, s.ID())
            }
        }(s)
    }
    // Drive errs in a goroutine; wg.Wait() in main returns once all
    // shards have either sent their sentinel or errored.

    // Wrap the job's stream so we can surface per-shard errors as
    // stream errors.
    return &ExecuteResult{
        Stream:  newShardErrorAwareStream(job.Stream(), &wg, errs),
        Notices: nil,  // notices come from runLocal/shellfs; no path here
    }, nil
}
```

The `shardErrorAwareStream` wrapper surfaces shard-level errors
that aren't caught by `wg.Wait()` (since `wg.Wait()` runs after
`return`). It blocks `Next` until all shards complete on the first
read, *or* exposes the error via the stream's terminal error mode.

A simpler alternative: read shard errors before constructing the
stream:
```go
go func() {
    wg.Wait()
    close(errs)
}()
// Don't return until errs is closed and we've collected any errors.
// But that defeats streaming.
```

The best shape is to start the wg.Wait + errs drain in a goroutine
and surface its result via a sentinel error in the channel-backed
stream. The shard.Query call is synchronous within each goroutine,
so by the time the goroutine sends its sentinel to the job, it has
either succeeded (errs not written) or failed (errs written). The
stream can include logic to check errs after the channel closes
and surface as stream error.

## A crucial detail: shard.Query is still batch

`shard.Shard.Query(sql)` returns `[]*pb.ResultRow` synchronously —
it executes the DuckDB query and buffers the result. To unblock
that, shards would need a streaming Query RPC. That's out of scope
for step 2.

So the latency floor at step 2 is:
- Slowest shard's `Query` time (DuckDB execution + serialization).
- Plus the network hop (negligible if shards are co-located).

The merge-then-emit is incremental; the shard-side Query is not.
For 12-shard fan-out, the slowest shard dominates time-to-first-row,
not the merge itself. That's fine — it matches what the user
experiences as "shard-side latency."

A future shard streaming Query is a separate plan
(see `direct-to-compute.md` §3 for the existing
shard.QueryToCompute streaming RPC, which the
direct-to-compute path already uses).

## Testing

`coordinator/streaming_test.go` — new file.

```go
func TestExecute_StreamingPathFordwardsLogQueryDirectly(t *testing.T) {
    c := setupTestCoordinator(t)
    res, err := c.Execute(`cat vimgym.log`, fullRange, "kevlar", "")
    // Stream should be channel-backed (the underlying compute job's
    // streamCh), not a slice replay. Detection: ?
    //
    // Functional check: the stream's underlying type is what
    // LocalComputeJob.Stream returns. We can't easily assert that
    // without exposing internals. Pragmatic test: call Next within
    // 50ms of dispatch; if it blocks (slice replay would never
    // block once populated, channel would block until shards push),
    // it's streaming.
    //
    // Actually simpler: assert the result's Rows/Text are NOT
    // populated (since Stream is direct). If executeLogStreaming
    // chose its branch, ExecuteResult.Rows is empty.
    if len(res.Rows) > 0 {
        t.Errorf("streaming path should not pre-populate Rows; got %d", len(res.Rows))
    }
    drained, _ := res.Drain(context.Background())
    // ... assertions on drained.Rows ...
}

func TestExecute_BufferedPathOnAlert(t *testing.T) {
    c := setupTestCoordinator(t)
    res, err := c.Execute(`cat vimgym.log | alert above 100`, ...)
    // Alert forces buffering; Rows is pre-populated.
    if len(res.Rows) == 0 {
        t.Errorf("alert path should pre-populate Rows for backwards compat")
    }
}

func TestExecute_BufferedPathOnRedirect(t *testing.T) { ... }
func TestExecute_BufferedPathOnComputeStages(t *testing.T) { ... }
```

The "pre-populated Rows" detection isn't elegant but it pins the
branching without introspection. A cleaner alternative: make
`executeLogStreaming` set a known marker (e.g.
`res.streamSource == "compute-direct"`), but that's adding API for
testability.

End-to-end: existing `coordinator_test.go` cases continue to work
because the buffered fields stay populated for tests that read
them. Streaming-path tests must consume Stream directly.

## Estimated scope

- ~150 lines new in `coordinator/coordinator.go` (split executeQuery
  into streaming + buffered, add shardErrorAwareStream wrapper).
- ~80 lines new tests.
- 0 lines change in tests that read Rows/Text/Stderr — they hit the
  buffered path or the dual-populated ExecuteResult.

Total: ~250 lines. One commit, dependent on step 1.

## After this step lands

ExecuteResult's dual population (Stream + Rows/Text/Stderr) starts
to be a measurable cost. For streaming queries the buffered fields
stay empty (no QueryResult was built). For buffered queries they're
populated. Tests that mix don't notice; tests that assert "Rows
should be empty when streaming" pin the boundary.

A follow-up cleanup commit (out of scope for this plan) would:

- Migrate test field accesses to `res.Drain(ctx)` patterns.
- Delete Rows/Text/Stderr from ExecuteResult; keep only Stream and
  Notices.

This is mechanical and can be done in a separate sweep after step 2
proves stable.

---

# Step 3 — Streaming warp exec for stream-safe stages

## What changes

`warp/pkg/bash` and the `compute.LocalComputeJob.consume`
staged path.

Today:
```go
// Staged: feed merged rows into warp as one batch.
input := joinedMergedRows(merged)
stdout, _, _, err := bash.RunWithParallelism(pipeline, input, j.parallelism)
// Split stdout into Lines.
```

This is non-streaming because `bash.RunWithParallelism` runs the
whole pipeline to completion and returns `stdout` as a single
string. Even for stage-safe pipelines (`grep`, `awk` without END),
no line is emitted until everything finishes.

After:
```go
// stream-safe staged execution: warp exposes a Run that takes
// stdin/stdout as io.Reader/io.Writer, emits incrementally.
stdoutR, stdoutW := io.Pipe()
inputR := mergedLineReader(merged)  // io.Reader over the merged stream

go func() {
    defer stdoutW.Close()
    bash.RunStreaming(pipeline, inputR, stdoutW)
}()

// Read stdout line-by-line, push to streamCh.
scanner := bufio.NewScanner(stdoutR)
for scanner.Scan() {
    j.streamCh <- Line{Raw: scanner.Text()}
}
```

`bash.RunStreaming(pipeline, stdin, stdout)` is the new warp API.
It runs the pipeline with the supplied IO, streaming as bytes flow.
For stream-safe stages (grep, awk no-END, sed), output appears as
input flows. For batch-only stages (sort, wc, uniq), output appears
only after stdin closes — same as today, but the API is uniform.

## Which stages are stream-safe

Stream-safe means "emits output incrementally as input arrives, not
buffered to end-of-input." Bash builtins by behavior:

| Stage | Streaming? | Why |
|---|---|---|
| `grep` | Yes | Per-line filter, emits matches as input flows |
| `awk` (no END, no stateful aggregation) | Yes | Per-line action, no buffering required |
| `sed` (no `D`/`G`/`x` accumulators) | Yes | Per-line transform |
| `cut`, `head`, `tail -n +K` | Yes | Streaming projection / skip-N |
| `tee` | Yes | Splits to stdout + file |
| `tail -n N` (no -f) | No | Needs full input to know last N |
| `sort` | No | Must see all input before any output |
| `wc -l` (and most wc variants) | No | Single line at end |
| `uniq` | No (full) | Buffers until next-different line, but unbounded |
| `awk` with END | No | END runs after stdin closes |
| `jq` | Mostly yes | Top-level filters per JSON value |

Warp determines streaming-vs-batch per stage at parse time. The
pipeline runs streaming if every stage is streaming; otherwise
falls back to today's batch path for that pipeline.

(For mixed pipelines like `grep ERROR | sort`, the grep stage can
stream into sort's stdin, but sort can't emit until grep closes.
End-to-end the pipeline behaves as batch. Warp's existing pipe
machinery handles this: each command runs in its own process/
goroutine, output of one becomes input of next, the LAST stage
determines what reaches the consumer.)

## Warp surface change

Warp gains:
```go
// RunStreaming runs the pipeline with streaming stdin/stdout.
// Returns when stdin closes AND all stages have flushed.
//
// Each stage runs concurrently; bytes flow through pipes. Stages
// that buffer internally (sort, wc) emit only at end. Stages that
// stream (grep, awk-no-END) emit incrementally.
//
// Falls back to RunWithParallelism semantics if any stage in
// pipeline is unrecognized — preserves today's behavior for
// edge cases.
func RunStreaming(pipeline string, stdin io.Reader, stdout io.Writer) error {
    ...
}
```

This is a warp-side change. Substantial: warp's bash interpreter
already runs pipelines with concurrent goroutines via
`pkg/bash/exec.go`'s pipe wiring (each `SimpleCommand` in a pipeline
gets its own goroutine and stdin/stdout are connected via pipes).
The work is exposing the existing concurrent-execution path with
the right io.Reader/Writer signature, plus auditing stages for
"would this work if stdin is a slow streaming reader."

## Compute integration

```go
// In LocalComputeJob.consume staged path:

stagesPipeline := strings.Join(j.stages, " | ")

stdoutR, stdoutW := io.Pipe()
inputR := newMergedRowReader(j)  // streams from the online merge

errCh := make(chan error, 1)
go func() {
    defer stdoutW.Close()
    err := bash.RunStreaming(stagesPipeline, inputR, stdoutW)
    errCh <- err
}()

scanner := bufio.NewScanner(stdoutR)
for scanner.Scan() {
    j.streamCh <- Line{Raw: scanner.Text()}
}
if err := <-errCh; err != nil {
    j.streamErr = fmt.Errorf("compute: %w", err)
}
```

`newMergedRowReader` is an `io.Reader` that wraps the online merge
heap. As merge advances, it produces bytes (lines + newlines) on
demand. Warp reads from it; the merge advances. Heap and reader
fully decoupled.

This composes step 1 (online merge → reader) with step 3 (reader
→ streaming bash). Time-to-first-byte is now `slowest_shard +
slowest_stage_first_byte`, where stream-safe stages add ~0 latency
and batch stages (sort, wc) add their full execution time.

## Testing

Direct warp tests for `RunStreaming`:

```go
func TestRunStreaming_GrepEmitsAsInputFlows(t *testing.T) {
    // Pipe input slowly, assert grep output appears between writes.
}

func TestRunStreaming_SortBuffersToEnd(t *testing.T) {
    // Verify sort produces no output until stdin closes.
}

func TestRunStreaming_PipelineMixesStreamingAndBatch(t *testing.T) {
    // grep | sort: grep streams into sort, sort batches.
    // Output from sort appears after stdin closes.
}
```

Plus integration through `LocalComputeJob.consume`:

```go
func TestLocalComputeJob_StreamingStagedPipeline(t *testing.T) {
    // Pipeline: grep ERROR (stream-safe).
    // Send rows from shards; some match grep, some don't.
    // Assert matching rows arrive at Stream BEFORE all shards close.
}
```

## Estimated scope

Warp side: ~300 lines new (RunStreaming wrapper around existing
bash.Pipeline execution; the wiring exists, just needs the
public-API surface). Plus tests.

Compute side: ~50 lines refactor in consume staged path. Plus
tests.

Total: ~600 lines, half tests. Warp-side change so it's a separate
commit (or coordinated commit pair across `weft` and `warp` repos
since warp is a sibling Go module pinned by weft via replace
directive).

## Compatibility risk

Warp is shared with vimgym. RunStreaming is additive (doesn't change
RunWithParallelism), so vimgym's existing usage is unaffected. The
weft-side compute integration is the only consumer initially.

A separate question: should weft prefer RunStreaming over
RunWithParallelism universally? Probably yes once tested — the
streaming path is strictly more general (RunWithParallelism becomes
"buffer stdin, then RunStreaming, then collect stdout"). But that
migration can wait until streaming is in production for a release.

---

# Sequencing and dependencies

## Ordering rationale

```
Step 1 (online merge in compute)
  ↓ enables
Step 2 (forward stream from coord, common case)
  ↓ enables
Step 3 (streaming warp exec for stream-safe stages)
```

- **Step 1 before step 2**: step 2's "forward compute stream
  directly" only delivers a perf win if compute itself is streaming.
  Without step 1, step 2 just relocates the buffer from coord to
  compute — same end-to-end behavior, harder to debug.
- **Step 2 before step 3**: step 3 makes staged pipelines stream
  through compute. But step 3 routes its output through the same
  streamCh that step 2 forwards. Without step 2, step 3's win is
  invisible at the wire — the coord still buffers everything.
- **Step 1 + step 2 without step 3**: zero-stage queries stream
  end-to-end. Staged queries still buffer at compute (warp
  non-streaming). This is the 80% win — most user queries are
  zero-stage at the compute boundary because predicate pushdown
  has already collapsed `cat /log/foo.log | grep ERROR` into a
  zero-stage SQL query with a `regexp_matches` predicate.
- **All three**: every stream-safe pipeline shape streams
  end-to-end. Batch stages (sort, wc) inherently buffer.

## Independent verification

Each step has a per-step test that proves its win:

| Step | Test that pins the property |
|---|---|
| 1 | `TestLocalComputeJob_OnlineStreaming` — lines emerge before all shards close |
| 2 | `TestExecute_StreamingPathFordwardsLogQueryDirectly` — Rows is empty (stream-direct) |
| 3 | `TestRunStreaming_GrepEmitsAsInputFlows` — output between writes |

Plus end-to-end:

```go
func TestExecute_TimeToFirstByteUnderSlowShard(t *testing.T) {
    // Simulate one shard with a 200ms artificial delay before it
    // sends rows. Other shards send immediately. Measure time from
    // c.Execute call to first Stream.Next return.
    //
    // Without step 1: time is 200ms+ (waits for slow shard to
    // sentinel before merge runs).
    // With step 1: time is ~max(slow_shard_first_row, fast_shard_first_row)
    // = 200ms (because slow shard's first row gates the merge).
    //
    // Hmm — this test doesn't distinguish step 1 from baseline if
    // the slow shard is the one with line_num=1.
    //
    // Better: simulate one slow shard with HIGH line_nums (so its
    // rows aren't needed first). Without step 1, merge waits for
    // its sentinel. With step 1, merge emits the fast shard's low
    // line_nums as soon as the slow shard sends ITS first (high)
    // row, since the heap can then determine that the fast shard's
    // current head is smaller.
}
```

## Risk register

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Online merge has a deadlock under concurrent shard sends | Med | High | Direct unit tests for the heap, plus race detector in CI |
| Streaming path forgets a precondition check (alert with no body buffer) | Low | High | Explicit test for each precondition; default-deny (any plan field set → buffer) |
| Warp RunStreaming breaks vimgym | Low | High | Additive API; vimgym keeps using RunWithParallelism |
| Test surface migration delayed (Rows/Text/Stderr stay around) | High | Low | Acknowledged in step 2; cleanup commit follows naturally |
| shard.Query latency dominates and streaming benefit is invisible in real workloads | Med | Med | Measure first; if shard is the bottleneck, prioritize shard streaming over warp streaming |

## Out of scope

Things not addressed by this plan:

- **Shard streaming Query.** `shard.Shard.Query(sql)` is still
  batch — DuckDB executes the query and returns the full result
  before the shard's goroutine pushes to compute. For very large
  per-shard results, this is the dominating latency. Streaming
  shard Query is a separate plan; the direct-to-compute path
  already has shard.QueryToCompute as a streaming RPC, and the
  pattern would extend.
- **Phase-3 redirect cleanup.** `cat /log/foo.log > /tmp/notes`
  still goes through `writeQueryResultToFS` because warp's
  CallbackCommand doesn't carry redirects. Routing redirects
  through warp's stdout is a warp surgery noted in commit
  `d5b30cc` and is independent.
- **Bidirectional streaming (Follow tail).** `tail -f` already
  streams via the broker subscription path. This plan doesn't
  change Follow.
- **Backpressure tuning.** `streamCh` is buffered at 1024 and
  per-shard cursor queues are unbounded. For pathological cases
  (one shard emits 10x faster than others), memory could grow.
  Backpressure would need bounded per-cursor queues with shard-side
  flow control. Not a current production concern.
