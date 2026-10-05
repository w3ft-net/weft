# Known issues

Real, verified gaps that are deliberately not fixed yet — either out of
scope for the work that surfaced them, or needing a design decision before
a fix. Not a bug tracker; keep entries here only once root-caused, with
enough detail that fixing them later doesn't require re-discovery. Move an
entry to `docs/completed/` (or delete it) once fixed.

## `GracefulStop()` doesn't drain within systemd's 90s `TimeoutStopSec`

**Where:** `cmd/coordinator/main.go`'s SIGTERM handler (calls
`srv.GracefulStop()` then relies on deferred `drainCancel()`/
`provCancel()` on return) and `deploy/weft-coordinator.service` (no
explicit `TimeoutStopSec`, so systemd's 90s default applies).

**Symptom:** found running Test 1 of
`docs/plans/coordinator-chaos-tests.md` on 2026-09-19, and **reproduced
independently in Test 3 the same day** — not a one-off. Attempt 1
(`i-0d4b0c33ab6b07d29`, Test 1): `Stopping weft-coordinator.service...`
at `15:49:59`, forced `SIGKILL` at `15:51:29` — exactly 90s later.
Attempt 2 (`i-0408530e395c754a8`, Test 3): stop requested `16:09:24`,
forced `SIGKILL` at `16:10:55` — 91s later, same pattern, different
instance, unrelated cron/traffic load. Both ended in `failed`, not the
clean `inactive` a graceful stop should produce. Also observed in
attempt 2: background goroutines (the cron ticker, specifically) kept
running and completed a scheduled watch execution 13s before the forced
kill, so whatever `GracefulStop()` is blocked on, it isn't blocking the
whole process — just its own return, and by extension the deferred
lease-release path (see Root cause).

**Root cause:** not yet diagnosed — the SSM agent on the affected
instance was gone by the time this was noticed (the ASG had already
replaced it, see "Why it matters"), so its logs couldn't be inspected
post-mortem. Leading hypothesis, not yet confirmed: every lease-holding
loop's clean-release path (`ctx.Done()` → `ReleaseLease` +
`lease_event ... event=released`) is wired to a context that's only
canceled by `main()`'s deferred `drainCancel()`/`provCancel()` calls —
which run on `main()`'s *return*, i.e. only *after* `GracefulStop()`
itself has returned. If `GracefulStop()` blocks (on a slow-draining
ingest stream, a `tail -f` subscriber that never half-closes, etc.), the
lease-release path can never fire either — the process is then entirely
dependent on systemd's forced `SIGKILL` after `TimeoutStopSec`, with no
clean release logged at all. If confirmed, this ties the two problems
together: not just "graceful stop is slow" but "a stuck `GracefulStop`
also silently disables the clean lease-release path that's supposed to
make shutdown fast for everyone else." Worth checking whether the
underlying stall is the same class of issue as the previously-fixed
ingest-stream-leak (abandoned streams pinning `Recv()` handlers, fixed
via a weftd journal-tailer churn fix plus a server-side idle reaper) or a
distinct gap in the tail/gRPC server's own shutdown path.

**Why it matters:** every coordinator redeploy that stops the old
process before replacing it inherits this ~90s-worst-case stall per
instance (times however many instances roll), and the unit ending in
`failed` rather than `inactive` could interact badly with automation
that checks exit cleanliness. It also means "systemctl stop" is not the
fast, low-drama shutdown Gotcha A (in
`docs/plans/coordinator-chaos-tests.md`) assumed — it can take just as
long as a hard kill in practice, just via a different path (forced
`SIGKILL` after `TimeoutStopSec` instead of an immediate one).

**Status:** open, not yet root-caused. Needs a repro with active ingest/
tail traffic and a look at what `GracefulStop()` is actually waiting on
when it stalls — the coordinator's pprof endpoint + SIGUSR1 goroutine
dump over SSM (`docs/operations.md`, already used for prior leak hunts)
is the natural tool if it happens again.

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

**Status:** partially fixed. `IngestStream` now flushes immediately (ahead
of the 100-row/2s batch) when an incoming row matches an active **local**
`tail -f` subscriber — `TailBroker.HasSubscriber` /
`TailFanout.HasSubscriber`, wired into the per-row flush check in
`coordinator/service/server.go`'s `IngestStream`. This closes the gap for
the common case: dev/single-coordinator, and prod when the ingest stream
and the tail session happen to land on the same coordinator.

**Remaining gap:** the fast path is local-only. `TailFanout.Publish`
still has no way to know that a *different* coordinator has a matching
subscriber — Redis pub/sub doesn't tell a publisher who's subscribed, and
there's no reverse interest-announcement protocol today (`TailFanout.refs`
tracks *this* coordinator's own subscribers, used only to `PSUBSCRIBE` on
their behalf, not to learn about peers'). So when the ingest stream lands
on one N=2 coordinator and the tail session on the other, the row still
waits for the 2s ticker/100-row threshold before `Publish` (and therefore
the Redis hop to the peer) fires. Building a cross-coordinator interest
signal (e.g. peers announce topic interest to each other, or `PUBSUB
NUMSUB`-style querying) is the follow-up, not attempted here.

**Related:** `docs/plans/coordinator-horizontal-scaling.md` item 4,
`integration/tailfanout_live_bench_test.go`,
`coordinator/service/ingest_tailflush_test.go`.

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

## `evaluateStateMachines` has no per-shard/per-bucket timeout on `Evaluate`

**Where:** `logdb/provisioner.go`'s `evaluateStateMachines`.

**Fixed (2026-09-20):** the sequential `for` loop that made one slow
shard's `Evaluate` latency additive to the whole tick — confirmed live
under ordinary query jitter via Test 6 of
`docs/plans/coordinator-chaos-tests.md` on 2026-09-19 — is gone. Bucket
and shard `Evaluate` calls now run through `evaluateConcurrently`, a
bounded worker pool (`maxConcurrentEvaluate` = 8 slots by default,
`ProvisionerConfig.MaxConcurrentEvaluate` to override). Pinned by
`TestEvaluateStateMachines_ShardsConvergeConcurrently`
(`logdb/provisioner_evaluate_concurrency_test.go`), which fails against
the old sequential code (~N × per-shard delay) and passes bounded by the
concurrency limit instead.

**What's left:** no individual `Evaluate()` call has its own timeout, so
a genuinely *stuck* (not just slow) shard still occupies one of the 8
concurrent slots indefinitely rather than erroring out and freeing it for
the next tick.

**Why it matters less now:** before the fix, one slow/stuck shard blocked
100% of that tick's fleet-wide convergence. Now it costs at most 1/8 of
concurrent capacity — real degradation only shows up if 8+ shards are
stuck at once, which has never been observed (no real `ARCHIVING` stall
has happened yet either).

**Status:** open, low priority. Candidate fix: wrap each shard/bucket
`Evaluate` call in a per-call `context.WithTimeout`.

## Cron gap detection is not built (handoffs no longer skip or double-cover a window)

**Status (2026-10):** the scheduling half is fixed and live; gap *detection*
and recording a run's window are not built. Everything under **Symptom**
describes the old per-cron lease and sliding window, which are gone (see
**Built and rolled out**). Kept for the design reasoning behind what is left.

**Where:** `coordinator/cron_claims.go` (`runClaims`, `claimTick`),
`coordinator/cron_slot.go`, `executeWatch` in `coordinator/cron.go`,
`store.ParseScheduleInterval`, and `cron_results` (which records `ran_at` but
not which window a run covered).

**Symptom:** when a cron's owner stops, loses its lease, or hands it to
another coordinator, a stretch of time is either never queried or queried
twice, and nothing records which. Specifically:

- Every run queries `[now-interval, now]` (`watchQueryRange`). That tiles
  run to run only if runs are exactly one interval apart.
- `runWatch` runs once immediately on acquiring the lease and then starts
  a fresh ticker. So every acquisition (restart, roll, handoff) fires
  early with a window that overlaps the previous run's, double-counting
  rows, and resets the cron's phase. Rough estimate, not measured: a roll
  of the two coordinators moves each cron's lease once or twice, so up to
  ~35-70 early extra runs for today's 35 crons.
- A stretch with no owner leaves a hole nothing marks. Cron leases have a
  30s TTL (`store.LeaseTTL`) and are not released on shutdown (only
  `nat-holder` and `shard-fsm-leader` have a release path, and the first
  entry above suggests even that may not fire when `GracefulStop()` stalls).
  After a longer outage the first run covers only the last interval.
- `cron_results` has `ran_at` only, so a gap can't be told from a schedule
  edit, and an overlap can't be seen at all, without inferring from spacing.
- The planner's time bounds are closed (`ts >= start AND ts <= end`,
  `planner/sql.go` `addTimeBounds`). Any scheme that gives adjacent windows
  a shared endpoint double-counts rows stamped exactly on it, which matters
  once boundaries are round and logs carry second-resolution timestamps.

**Root cause:** execution is driven by a per-process ticker whose phase is
"when this process got the lease", the window comes from the wall clock at
run time, and results are keyed only by when they ran. Nothing ties a
result to a scheduled slot, so there is nothing to compare against.

**Built and rolled out (2026-10-05):** `coordinator/cron_slot.go` is the pure
slot/window logic. Slot `k` of interval `I` covers the epoch-aligned window
closing at `k*I`, identical for every cron of that interval; `Window()` ends
1µs before the boundary to suit the closed bounds; a cron fires at boundary +
settle (30s) + a stable per-watch jitter in `[0, min(I/2, 30s))`;
`LatestDueSlot` returns only the latest due slot, so missed slots are skipped,
never replayed. A run happens only if the coordinator wins an insert of the
claim key `cron:<watch>:<interval>:<k>` into `cron_claims` (a Postgres unique
key), so a slot runs at most once across coordinators; a failed claim write is
a skipped slot, never a duplicate. This is now the only execution path: the
per-cron lease, the sliding `[now-interval, now]` window and the immediate run
on acquire are removed. Holes are visible in `cron_claims` (a gap in the slot
index sequence for a cron), and the overdue guard (`cron_overdue.go`) reports a
cron that has stopped. See `docs/plans/cron-claims-rollout.md`.

A new cron's first run is its next slot (a slot whose window closed before the
cron was created is skipped), not an immediate run as before.

**What's left** (design agreed 2026-10-04; items 1-3 not built):

1. Gap detection as a pure function: given the previous run's window end
   and size and the current slot, return how many slots were missed, and
   whether to report at all (not when the interval changed since the last
   run, or the cron was disabled in between).
2. Record the window. Add nullable `window_end timestamptz` and
   `window_secs int` to `cron_results`, and persist them in
   `persistResults`. Add `last_window_end` / `last_window_secs` to
   `cron_watches`, written with the existing `last_run_at` update. Gaps are
   then derived by SQL (`window_end - lag(window_end) - window_secs`); a
   multi-row cron (alert_key set) writes several rows per run, so dedupe on
   `(cron_watch_id, window_end)`. Failed runs still write a row
   (`exit_code = 1`), so they never look like gaps. The partman template
   table `partman.template_public_cron_results` needs the same columns,
   guarded as migration 078 did for the other prod-only partman objects.
3. Log `cron[<name>]: skipped N slot(s) (<from>..<to>)` at detection. The log
   line ships into weft, so a cron can alert on it. No extra query: the new
   owner reads the previous window with the watch it already loads.
4. ~~Wire to slots and an exactly-once claim.~~ Done, with a Postgres unique
   key as the claim store. The immediate run on acquire is gone; an explicit
   run on create/edit was not built.

**Policy decided:** skip and record; do not replay or widen. Widening a
window across N missed intervals inflates count-style values N-fold and can
page falsely; replay bursts every cron's queries on recovery and sends stale
alerts. A per-cron `catchup` option can come later if someone needs it.

**Does not fix:** rows that land after a run has already queried their
window. Windows are over each row's `ts`, so a late row (for instance a
`weftd` spool delivered after an outage) is never counted by the cron for
its window. Contiguous means no overlap and no hole as of run time, not
complete.

**Why it matters:** alert and metric crons see inflated or missing windows
around every coordinator roll, and there is no record that it happened.
Nothing is on fire: results are only wrong near handoffs, and the crons
that exist today mostly read gauges, not counts.

**Related:** the schedule parser collapses 5-field cron strings into plain
intervals and falls back to 5 minutes for shapes it doesn't recognise (by
reading `ParseScheduleInterval`, e.g. `0 9 * * *` would run every 5
minutes), while `docs/manual/crons.md` shows cron syntax as the format.
Slots keyed by nominal time would make real cron expressions possible; that
is a separate decision and not covered here.

**Status:** open. Design agreed, pure slot logic written and unwired,
nothing recorded yet.

