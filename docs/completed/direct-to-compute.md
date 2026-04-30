# Direct shard→compute streaming (future work)

The coordinator currently buffers every compute-bound query's full
result set in memory before forwarding it to the compute pool. For
a query like `cat prod-nginx.log | grep 500 | sort | uniq -c` over a
busy week, that's tens of GB of `pb.ResultRow` bytes sitting in the
coordinator's heap between shard return and compute send.

The fix is the `QueryToCompute` RPC that's already defined in the
proto (`proto/weftpb/weft.proto:18`) but stubbed on the shard side
(`shard/service/server.go:221-223`, returns empty). Instead of
shards sending rows back to the coordinator as a relay, the
coordinator tells each shard where the compute worker is and the
shards stream rows there directly. Coordinator holds nothing in
memory during the scan.

## Why this matters

- **Coordinator RAM.** Today every compute-bound query double-buffers:
  once per-shard `s.Query(sql)` result slice, once as the merged
  `allRows` slice after sorting. A 50M-row log scan needs ~5 GB of
  struct overhead before anything gets into the compute pipeline. A
  few of those concurrent puts the coordinator close to OOM.
- **Latency.** Today compute sees its first row only after every
  shard's full result is back and sorted. With streaming, compute
  starts producing output as soon as the k-way merge's first row is
  available — typically within tens of ms of query start.
- **Parallelism.** Multiple concurrent compute-bound queries today
  serialize on the coordinator's attention (it's the merge point).
  Direct streaming lets shards fan out per-query with the
  coordinator only involved at query start and result fetch.

## Current state

```
Coordinator creates job on compute worker (CreateJob RPC)
  → Shards execute SQL and return full row slices (Query RPC)
    → Coordinator buffers all rows, sorts by line_num, applies Limit
      → Coordinator streams sorted rows to compute (SendRow × N)
        → Coordinator fetches text result (GetResult RPC)
```

Code points:

- `planner/sql.go` — shard SQL builder. Already supports optional
  `ORDER BY <col>` and `LIMIT N` clauses; the latter is used for
  `head` / `tail` pushdown today.
- `coordinator/coordinator.go:416-500` — fan-out, buffer, sort,
  limit, then compute-pool send.
- `shard/service/server.go:221-223` — `QueryToCompute` stub.
- `proto/weftpb/weft.proto:15-18` — RPC signature: `SQL`,
  `ComputeAddr`, `JobID` in; `RowsSent` out.

Current `QueryToCompute` request carries `sql`, `compute_addr`, and
`job_id` — enough to route; no `order_by` field yet.

## What to build

### 1. Shard implementation of `QueryToCompute`

In `shard/service/server.go`, fill in the stub:

1. Dial the caller-supplied `compute_addr` (gRPC, reusing the
   existing `pb.Compute_SendRowClient` stream type).
2. Open a `SendRow` stream tagged with `JobID` from the request.
3. Execute the SQL against the local DuckDB — same path as
   `QueryToSink` uses today.
4. For each row returned, emit a `pb.SendRowRequest{JobId, Row}`
   into the stream.
5. When the DuckDB cursor closes (or the context is cancelled),
   emit the **per-stream sentinel** — the same end-of-stream marker
   the compute pool already uses today (`job.SendSentinel("merged")`
   in coordinator/coordinator.go:492). In the direct path each
   shard sends its own sentinel tagged with its stream id;
   compute's k-way merge drops that stream from the heap when the
   sentinel arrives. The overall job closes once every stream has
   sentinelled. Close the stream after the sentinel.
6. Return `QueryToComputeResponse{RowsSent: n}` so the
   coordinator has a sanity check.

Errors are returned as RPC errors — coordinator retries or bubbles
up to the caller. Partial streams are the compute worker's problem
to reconcile (see the ordering section).

### 2. Coordinator flow for compute-bound queries

`executeQuery`'s compute-stage branch changes from buffer-sort-send
to fan-out-and-wait:

```go
if len(plan.ComputeStages) > 0 && c.Pool != nil {
    job := c.Pool.NewJob(plan.ComputeStages, len(matching))
    errs := make(chan error, len(matching))
    for _, s := range matching {
        go func(s shard.Shard) {
            _, err := s.QueryToCompute(sql, job.ComputeAddr(), job.ID())
            errs <- err
        }(s)
    }
    for range matching {
        if err := <-errs; err != nil {
            job.Cancel()
            return nil, err
        }
    }
    text, err := job.Result() // blocks until k-way merge completes
    return &QueryResult{Text: text}, err
}
```

Notice the second arg to `NewJob` changes from `1` to
`len(matching)` — the compute pool's input frame becomes
multi-stream so it knows how many sentinels to expect before
closing the merge.

### 3. Ordering: planner picks a merge key from the pipeline's terminal sort

Unconditional `ORDER BY line_num` is wrong. When the pipeline ends
with `| sort` the user wants output ordered by raw text; shards
pre-sorting by line_num is wasted work that compute re-does. When
the pipeline uses `| sort -k 3` or `| sort -n`, shards can't
pre-sort at all (they don't parse the extraction key). The merge
strategy has to match the pipeline's terminal ordering.

The planner picks one of three shard-output modes:

**Mode A — line_num ordered.** Default. Covers filter-only
pipelines (`cat … | grep X`), `head`, `tail`, and anything with no
`| sort` stage.

Shard SQL depends on whether the plan has a head/tail:

| case               | shard SQL                                  | compute heap | output                       |
|--------------------|--------------------------------------------|--------------|------------------------------|
| no head/tail       | `ORDER BY line_num`                        | min-heap     | rows ASC by line_num         |
| `\| head -N`       | `ORDER BY line_num ASC LIMIT N`            | min-heap + LIMIT N | first N globally       |
| `\| tail -N`       | `ORDER BY line_num DESC LIMIT N`           | max-heap + LIMIT N | last N globally, then reversed at output so the user sees oldest→newest |

The "reverse at output" for tail is a cheap O(N) final step — N
is small by definition (N rows after the merge heap clamps).

Memory: O(shardCount) rows in the heap. Constant w.r.t. total rows.

**Mode B — raw-text ordered.** Pipeline ends with a plain `| sort`
or `| sort -r` (no flags, no `-k`, no `-n`), **and** no compute
stage between the shard output and that sort transforms row bytes.

The second clause matters: `ORDER BY raw` on the shard only
produces a sensible global order when the bytes `raw` holds on the
wire are the bytes `sort` compares. Every intermediate compute
stage between the shard and the sort must either (a) be absent, or
(b) be a pure filter that drops rows without rewriting them (today
that's limited to `grep`, `head`, `tail`). Anything that rewrites
row text — `awk '{print $N}'`, `sed` substitution, `jq '.key'` —
breaks the invariant because `sort` will compare rewritten bytes,
not the bytes the shard ordered by.

Planner rule: Mode B applies only when all stages from shard
output through the sort are in the "order-preserving, non-
transforming" set, AND the sort has no flags. A safe first cut is
"Mode B only when `sort` is the first compute stage" — all other
cases fall to Mode C.

- Shard SQL: `ORDER BY raw [DESC]` (DuckDB lex ≈ POSIX sort's
  default byte-wise compare). With `| head -N` or `| tail -N`
  appended, add `LIMIT N` and handle direction the same way as
  Mode A's head/tail.
- Compute: k-way merge on raw. The `| sort` stage in
  `ComputeStages` sees pre-sorted input; planner may strip it (a
  well-behaved sort detects sorted input and passes through
  linearly if left in).
- Memory: O(shardCount).

**Mode C — compute-sorts.** Everything else: `| sort -n`,
`| sort -k <field>`, `| sort -f`, a sort that follows an awk/sed/jq
transformation, or any combination the planner can't prove
preserves raw bytes.

- Shard SQL: **no** `ORDER BY`. And critically, **no** `LIMIT`
  either when there's a terminal `| head` or `| tail` — the
  reordering compute does means shard-local "first N" is not
  global "first N." Both orderings and limits move entirely to
  the compute side.
- Compute: drains shards in arrival order, runs the `sort` stage
  as a real sort, then any subsequent stages including head/tail.
- Memory: bounded by compute's own sort buffer, not by shardCount.
  Loses the merge modes' constant-memory property. Still strictly
  better than today — the *coordinator* doesn't buffer, compute
  is where the data plane lives, and compute can spill to disk
  under pressure (the warp `sort` implementation handles this).

**How the planner decides.** Walk `ComputeStages` looking for the
last `sort` invocation:

1. No sort → **Mode A**. Shard SQL ORDER BY is line_num, direction
   and LIMIT depend on terminal head/tail.
2. Sort has flags (`-n`, `-k`, `-f`, etc.) → **Mode C**. Shard SQL
   has no ORDER BY and no LIMIT; compute does everything.
3. Plain sort / `sort -r` AND all pre-sort stages preserve raw
   bytes → **Mode B**. Shard SQL orders and limits by raw.
4. Otherwise (plain sort but a pre-sort transform rewrites raw) →
   **Mode C**. Same reasoning as (2).

Encoded on the plan as two fields:

- `QueryPlan.ShardOrderBy` — `""`, `"line_num"`, `"line_num DESC"`,
  `"raw"`, or `"raw DESC"`. Appended to the shard SQL by the builder.
- `QueryPlan.ShardLimit` — per-shard LIMIT. Zero when LIMIT
  pushdown is unsafe (Mode C, or Mode A/B with no terminal head/
  tail).

Both values flow into the `QueryToCompute` request via the new
`merge_key` proto field (see Open Question) so the compute worker
can select its heap/concat behaviour without duplicating the
planner's logic.

### 4. Compute worker's input frame

The pool's `NewJob` call already takes a `streams` count
(`c.Pool.NewJob(plan.ComputeStages, 1)` today). Extend the job's
input handler to either (a) k-way-merge on the declared merge key
when `ShardOrderBy` is non-empty or (b) plain concat when it's
empty. Both cases feed a single logical input stream into the
compute pipeline — compute stages don't change.

K-way merge internals:

```
heap := min-heap of (merge_key, row, shard_stream)
for each shard_stream: pull first row → push into heap
while heap not empty:
    (_, row, stream) := heap.pop()
    feed row into compute pipeline
    next := stream.Recv()
    if next exists: heap.push((next.merge_key, next, stream))
    else: stream is done, drop it
```

Comparator flips (min vs max) for descending mode. `merge_key` is
`line_num` for Mode A, `raw` for Mode B.

First output appears as soon as every shard has emitted at least
one row (or signalled EOF). Same time-to-first-byte as today's
path, which also has to wait for every shard to respond.

### 5. Limit: mode-dependent pushdown

The per-shard LIMIT pushdown that `head`/`tail` uses today is safe
only when the shard-local "first N by shard order" is also the
global "first N by plan output order." That means LIMIT pushdown
is **mode-specific**:

- **Mode A with head/tail:** shard ORDER BY line_num [DESC] LIMIT
  N. Compute k-way merges and stops at N globally. Worst case N×
  shardCount rows on the wire, N emitted. This is the existing
  pushdown rule, unchanged from today.
- **Mode B with head/tail:** shard ORDER BY raw [DESC] LIMIT N.
  Same mechanic as Mode A — per-shard "top N by raw" rolls up to
  "global top N by raw" via the merge heap.
- **Mode C with head/tail:** **no shard-side LIMIT.** The compute
  sort reorders rows; a shard's first N by line_num / arrival
  order is not the global first N by the custom sort key. Shards
  must emit every matching row; compute sorts them all; compute
  then applies head/tail. The pushdown win is lost — this is the
  price Mode C pays for keyed sort.
- **Any mode without head/tail:** no LIMIT at all, on shard or on
  compute. Full result flows through.

Compute still caps globally when head/tail is set — in Mode A/B
the k-way merge stops once N rows have been emitted and cancels
any still-open streams; in Mode C the compute sort takes the full
input and the head/tail stage trims at the end. The `plan.Limit`
field already lives on the QueryPlan; add a `plan.ShardLimit` that
the planner sets to `Limit` in Modes A/B and `0` in Mode C.

## Beyond this plan: should the compute pool dispatch to shards?

The current coordinator is dual-role — control plane (shard
registry, bucket lifecycle, cron scheduling, alert dispatch) AND
data plane (query fan-out, row merge, compute hand-off). This plan
strips the row-merge half of the data plane. The fan-out half is
still in the coordinator.

A natural next step after this: move the fan-out into compute too.
Coordinator becomes planner + control plane; compute becomes
executor. A query flow would be client → coordinator.Plan(pipeline)
→ coordinator hands the plan (with the list of matching shards and
the injected customer predicate) to a compute worker → compute
dials every shard's QueryToCompute → compute merges and runs the
pipeline → compute returns text to coordinator which returns it to
the client.

**Wins:**
- Coordinator heap stays control-plane-shaped. No query-data
  buffering at all.
- Compute pool scales independently for query load; coordinator
  scales for shard-count / bucket-count / cron-tick-rate.
- One fewer hop for the rows themselves (they never touch the
  coordinator even as a routing target — shards send directly to
  compute, compute returns text).

**Breaks / requires:**
- `MatchingShards` access needs to reach compute. Either (a)
  coordinator includes the list of shard addresses in the plan it
  hands to compute, or (b) compute calls back to the coordinator
  for shard lookup. (a) is simpler and doesn't add round-trips.
- Customer injection and plan-tier retention clamp move out of
  `coordinator.query()` — either into the planner output itself or
  into a thin wrapper on the coordinator before the plan hand-off.
  Already close to there; `InjectCustomer` is a one-liner at the
  call site.
- Alert-bearing plans need rows back in the coordinator for
  incident dedup + notification. Options: compute returns rows
  alongside text for alert plans only; or alert evaluation itself
  moves to compute. Both are work.
- `QueryForWatch` (cron's query path) today funnels into
  `coordinator.query()`. Would instead call compute, with the
  watch-bound AlertContext threaded through. Mechanical change.
- Ingest is untouched (ingest writes directly to shards; never
  flowed through the coordinator's query plane).

The answer to "what would that refactor break?" is: alert dispatch
has to be rethought, and two or three call sites need rewiring.
Not huge but not trivial. Worth its own plan doc; tracked here as
a future direction, not scoped into this one.

Note that the direct-to-compute path described in this plan is the
natural first step toward that split — it eliminates the
coordinator's data-plane buffer, leaving only the fan-out
responsibility still there to move. If this ships and runs clean,
the second half is mostly wiring.

## Edge cases and concerns

- **Shard mid-stream failure.** The compute worker sees a stream
  error; it signals the job as failed. Coordinator's
  `for range errs` loop picks up the RPC error from the
  `QueryToCompute` call too (the shard's outbound stream wraps the
  RPC). Coordinator cancels the compute job and returns the error
  to the caller. No partial results.
- **Compute worker crash.** Coordinator's `CreateJob` returned a
  specific worker; if that worker dies, the job RPC fails when
  coordinator calls `job.Result()`. Shards discover their stream is
  broken and give up. Existing coordinator error path applies. No
  rescheduling to a new worker in v1 — the query fails and the
  caller retries.
- **Authentication shard → compute.** Today shards only talk to the
  coordinator. In the direct-to-compute path they'd dial compute
  workers. The coordinator passes the compute addr in the
  `QueryToCompute` request; the compute worker should validate the
  incoming `JobID` matches one it actually created (i.e. the
  coordinator set it up first). This prevents unauthenticated
  shards from pushing rows into a job they weren't routed to. The
  `JobID` should be a cryptographically random handle (existing
  job IDs are uuid-ish — verify).
- **Back-pressure.** gRPC streaming gives us flow control for free:
  if compute is slow, the shard's `SendRow` blocks. That's
  desirable (prevents coordinator OOM of the old path by simply
  slowing the producer). Risk: a slow compute stage across many
  concurrent queries could pin all N shards. Same operational risk
  as today; no worse.
- **Queries without compute stages.** Unchanged. `executeQuery`
  keeps its current buffered path for plans whose `ComputeStages`
  is empty, because there's no compute worker to stream to.
  Equally, the `| alert` post-stage (`QueryResult.Rows` inspected
  by the alert evaluator) needs rows back at the coordinator — so
  alert-bearing plans take the existing path too. The optimization
  helps compute-stage-heavy pipelines (`grep`, `awk`, `sort`,
  `uniq`, `wc`, `jq`) which are exactly where the row counts are
  highest anyway.

## Order of operations

1. **Shard-side impl of `QueryToCompute`.** Fill the stub; returns
   `RowsSent` count. Unit-testable against a fake compute client.
2. **Compute pool input frame.** Extend the job's input handler to
   dispatch between k-way merge (when `ShardOrderBy` is set) and
   concat (when empty). Covered by unit tests that push out-of-
   order rows from 3 fake streams, one test per mode, asserting
   output shape.
3. **Planner: `ShardOrderBy` + mode selection.** Walk
   `ComputeStages` for the last `sort`; set `QueryPlan.ShardOrderBy`
   accordingly (`line_num`, `line_num DESC`, `raw`, `raw DESC`, or
   `""`). Pipelines with a final `| alert` or no compute stages
   stay on the old path regardless.
4. **Planner: per-shard LIMIT pushdown.** When the plan has a
   limit, emit `LIMIT N` on every shard's SQL.
5. **Coordinator: replace the buffered compute-stage branch.**
   `executeQuery` fans out `QueryToCompute` in parallel, waits on
   an error-channel, calls `job.Result()`. No fallback path; the
   old buffered branch is deleted.
6. **Shard SQL builder.** Append `ShardOrderBy` to the emitted
   SQL; omit `ORDER BY` when it's empty.
7. **End-to-end tests.** Seed multiple shards with overlapping
   line_num ranges, exercise each mode (filter-only, `| sort`,
   `| sort -k 3`), assert correctness of the output and coordinator
   RAM stays flat during the scan.

Steps 1 and 2 can land in parallel. 3–6 go together as a single
commit so the coordinator, planner, and shard all speak the same
`ShardOrderBy` dialect at the moment the direct path goes live.
7 is the gate to merging.

## Proto extension

`QueryToComputeRequest` grows a `merge_key` field alongside the
existing `sql`, `compute_addr`, and `job_id`:

```proto
message QueryToComputeRequest {
    string sql          = 1;
    string compute_addr = 2;
    string job_id       = 3;
    // Ordering key for compute's k-way merge. One of:
    //   "line_num", "line_num DESC", "raw", "raw DESC" → merge.
    //   "" (empty)                                     → concat.
    // Matches plan.ShardOrderBy. Shards do not read this directly
    // (the ORDER BY is already baked into `sql`); compute reads it
    // to choose between heap-merge and concat input frames.
    string merge_key    = 4;
}
```

Single source of truth: the planner decides the mode, encodes it
in `merge_key`, and both shards (via the SQL itself) and compute
(via this field) see the same decision.
