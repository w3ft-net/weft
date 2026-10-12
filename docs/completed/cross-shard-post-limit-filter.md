# Plan: Cross-shard ordering for post-limit filters

## Status

**Option A landed** in the planner (post-limit filters run in compute on the
globally merged top-N stream). Option B — pushing post-limit predicates back
into shard SQL via a two-pass time-bound — is **deferred** until a workload
actually wants the wire savings.

## Amendment (2026-10-12): no carve-out

Commit `a1842de` added a "limit-safe" carve-out: selective filters (awk field/meta-var
equality, positive regex, plain `grep`, `sed -n '/p/p'`) were pushed into the shard SQL
even after a head/tail froze the LIMIT, on the reasoning that `tail | awk '$N == "foo"'`
means "recent rows where N matches". That reading silently changes the pipeline: `tail -n
500 | awk 'LEVEL == "ERROR"' | wc -l` returned the last 500 *matching* rows, always 500
once a window had that many errors, so an `alert above 50` on it always fired. The
carve-out is removed; the rule is Option A as written below with no exceptions, so a line
count means "these N rows, then the rest of the pipeline". Selective post-limit filters
that want the wire savings belong to Option B.

## Why it exists

Pipelines that put a `head -n N` or `tail -n N` *before* a filter
(`tail -n 2000 | grep -v ERROR | wc -l`) ask, by pipe-order, for "the most
recent N rows globally, then keep matches." A shard-side `WHERE ... LIMIT N`
gives "this shard's most recent N matches" — and across N shards the merged
result is N×num_shards matches, after which the global cap clamps to N. With
a selective filter you get the right answer by accident; with an inclusive
one (e.g. `grep -v ERROR` keeping ~95% of rows) the cap saturates at N
regardless of the dataset's actual shape.

The pre-limit shape (`grep ERROR | tail -n 2000`) is unaffected — the filter
runs first in pipeline order, so SQL `WHERE LIKE '%ERROR%' ORDER BY ts DESC
LIMIT 2000` is exactly the right reduction. Most real watches look like that.

## Option A — landed: post-limit filters run in compute

```
  pipeline:  cat foo.log | tail -n N | grep PAT | wc -l
  per shard: SELECT line_num, raw FROM logs WHERE app='foo' ORDER BY ts DESC LIMIT N
  compute:   k-way merge by ts DESC  →  global LIMIT N  →  grep PAT  →  wc -l
```

**What changes in the planner.** The `sqlBuilder.limitFrozen` flag flips the
moment a LIMIT pushes (source `head`/`tail`, mid-pipeline `head -n` / `tail
-n`, or an `awk` NR-window). Once frozen, the stage loop stops attempting
pushdown — every later stage is appended to `computeStages` verbatim.

**What stays the same.** Pre-limit predicates push as `WHERE` fragments,
exactly as before. The merged-stages test cases (`awk filter | head`,
`grep | head`, `jq select | head`, etc.) all keep one-trip flat SQL.

**Wire cost of Option A.** Bounded by `N × num_shards` rows. With the demo's
2000-line tail and ~12 shards, that's ≤24k rows on the wire per query —
single-digit MB worst case, on a network the coordinator already streams
shard rows over for every query. No measurable change at the scales the
current dev/prod fleets run at.

**Single-shard cost.** When the source data lives on one shard (the common
case for short-window watches like `error-rate`), Option A still routes the
post-limit filter to compute. The shard scans the LIMIT-N rows it didn't
strictly have to filter; the wire cost is the same as if the filter had
pushed (the LIMIT bounds it). Functionally indistinguishable from the
pushed-predicate version, just with the filter running one process over.

## Option B — deferred: two-pass time-bounded query

Push post-limit predicates back to shards by first establishing the global
time window, then re-querying with the time bound:

**Pass 1** (timestamps only):

```
  per shard: SELECT ts FROM logs WHERE app='foo' ORDER BY ts DESC LIMIT N
  coordinator: collects N×num_shards timestamps, picks the global Nth-largest ts*
```

**Pass 2** (full rows, bounded):

```
  per shard: SELECT line_num, raw FROM logs
             WHERE app='foo' AND ts >= ts* AND raw LIKE 'PAT'
             ORDER BY ts DESC LIMIT N
  compute: k-way merge  →  global LIMIT N  →  remaining stages
```

**Why it's deferred.** The data savings are real (selective post-limit
filters could cut the wire cost from `N × num_shards` to far less), but the
latency cost is an extra round-trip per query, and the coordinator gains a
two-phase shape that today it doesn't have for any other pipeline. Until a
production workload demonstrates that the wire savings matter — say,
post-limit filters with single-digit-percent selectivity over multi-million-
row windows — Option A is strictly simpler.

**What it'd take.**

- Coordinator: a "time-bound discovery" RPC that fans out the timestamp-only
  pass, collects results, and computes `ts*` (a k-th-largest selection over
  N×num_shards 64-bit timestamps; trivial).
- Planner: emit two SQL strings (`ShardSQLDiscover` + `ShardSQLFinal`) with
  a `ts >= $1` placeholder bound at runtime. The post-limit predicates
  attach to `ShardSQLFinal`'s WHERE.
- ComputeJob: unchanged — same merge + global cap + stages.

The planner's `limitFrozen` flag already carves out exactly the cases that
benefit, so the routing decision is local: if `len(postPredicates) > 0` and
the workload heuristic says "wide window, selective filter," emit the
two-pass plan. Otherwise emit Option A's flat SQL + compute filter.

## When to revisit

- A dashboard widget or watch wants `tail -n 100k | filter | aggregate` and
  Option A's wire cost (100k × num_shards rows) gets uncomfortable. Profile
  first: at the time of writing, the largest watch tail is 2000.
- We add a "gather → reduce" two-phase shape for some other reason
  (windowed aggregates, distributed sort), and the discovery pass becomes
  free infrastructure rather than a one-off.
- Telemetry shows a long tail of post-limit-filter queries with low
  selectivity (the cases Option A's wire cost actually penalizes).
