# Known issues

Real, verified gaps that are deliberately not fixed yet — either out of
scope for the work that surfaced them, or needing a design decision before
a fix. Not a bug tracker; keep entries here only once root-caused, with
enough detail that fixing them later doesn't require re-discovery. Move an
entry to `docs/completed/` (or delete it) once fixed.

## `tail -f` can lag up to ~2s under sparse ingest, unrelated to the Redis fan-out

**Where:** `coordinator/service/server.go`'s `IngestStream` (the flush
goroutine and the `len(rows) >= 100` check, both around
`IngestStream`'s body).

**Symptom:** a live `tail -f` row can take up to ~2 seconds to appear,
even on a healthy stack with no queueing anywhere else.

**Root cause:** `IngestStream` buffers received rows locally and only
calls `ingestBatch` (which calls `TailFanout.Publish`, the thing that
actually wakes up `tail -f` subscribers) when either 100 rows have
accumulated or a 2-second ticker fires — a batching design tuned for
efficient shard writes, not for `tail -f` latency. A customer whose ingest
rate is well below 100 rows/2s effectively waits on the ticker for every
row. This is **not** about Redis, coordinator count, or cross-coordinator
delivery at all — it would add the same lag to a single-coordinator,
purely local `tail -f` too.

**How this was found:** `integration/tailfanout_live_bench_test.go`
(added 2026-09-17 to validate the Redis cross-coordinator tail fan-out —
docs/plans/coordinator-horizontal-scaling.md item 4 — against the real
docker-compose stack before its `terraform apply`) ran two sub-benchmarks
through the real `IngestStream` → Redis → `Execute(follow=true)` path:

```
latency (100 rows, 15ms apart):  delivered=100/100  p50=817ms  p95=1.536s  p99=1.599s  max=1.615s
throughput (2000-row burst):     delivered=2000/2000 (100%)   p50=16ms   p95=26ms   p99=26ms   max=26ms
```

The burst case blows past the 100-row threshold constantly and flushes
promptly (16-26ms, consistent with the isolated `bench/tailfanout`
harness's own ~100-130µs Redis-hop measurement plus real gRPC/network
overhead). The spaced-send case never hits 100 rows, so it's paced
entirely by the 2-second ticker — the gap between the two runs isolates
the ticker as the cause. `bench/tailfanout_bench_test.go`'s in-process
harness never saw this because it calls `Publish` directly, bypassing
`IngestStream`'s batching layer entirely.

**Why it matters enough to write down:** the Redis fan-out work itself is
fine — this doc's numbers are what confirm that. But a post-launch report
of "tail -f feels laggy" would naturally get pinned on the new Redis
feature, when the real cause is upstream in `IngestStream` and predates it.

**Status:** open, no fix scheduled. Candidate directions, not evaluated
in depth: flush immediately once a topic has at least one active `tail -f`
subscriber (costs some batching efficiency only during active tail
sessions); shorten the ticker; a dedicated low-buffer path for low-volume
producers.

**Related:** `docs/plans/coordinator-horizontal-scaling.md` item 4,
`integration/tailfanout_live_bench_test.go`.

## Stale dev postgres volumes can permanently miss schema.sql-only columns

**Where:** the `store/schema.sql` collapse workflow (see
`docs/completed/migration-collapse.md`) — `make collapse` folds applied
migrations into `schema.sql` and the individual migration files stop
existing on disk.

**Symptom:** `CreateToken`/`CreateAgentToken` (or any other
collapsed-in change) fails against a long-running local dev postgres with
a `column "..." does not exist` error, even though `store/schema.sql` and
every fresh bootstrap already have that column.

**Root cause:** a dev postgres container that was provisioned *before* a
given migration was written, and is still running when that migration
later gets folded into `schema.sql` by a collapse, has no path forward —
`goose up` only knows about migration files that still exist on disk, and
the delta file is gone. A fresh bootstrap picks the column up for free
from `schema.sql`; an already-running, already-populated dev volume does
not. Hit in practice 2026-09-17: `agent_tokens.producer` (migration 073,
added 2026-09-02) was missing from a dev postgres that had been running
across that collapse.

**Fix applied (that occurrence only, not persisted anywhere):** manually
ran `ALTER TABLE agent_tokens ADD COLUMN producer text DEFAULT ''::text
NOT NULL;` directly against the dev DB to match `schema.sql` exactly — a
one-time, additive repair of that one running container, not a code
change (a fresh bootstrap never needs it).

**Status:** open as a recurring risk, not a one-time bug — any sufficiently
long-lived dev postgres volume can hit this again after the next collapse.
No general fix designed (candidate: a `make dev-reset` that recreates the
postgres volume from current `schema.sql`, or a doc note in the collapse
runbook to recreate dev volumes after every collapse).
