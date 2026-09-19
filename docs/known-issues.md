# Known issues

Real, verified gaps that are deliberately not fixed yet — either out of
scope for the work that surfaced them, or needing a design decision before
a fix. Not a bug tracker; keep entries here only once root-caused, with
enough detail that fixing them later doesn't require re-discovery. Move an
entry to `docs/completed/` (or delete it) once fixed.

## jq's reserved meta-variables (`._host`, `._service`, etc.) resolve to `null` when read as output

**Where:** `planner/jq.go` — `jqMetaVars` is only consulted by
`jqComparisonToSQL` (used from `jqCondToSQL`/`jqExprToSQL`, reached via
`tryJqPushdown`/`tryJqStagePushdown`), which handles turning a
`select(._host == "x")`-shaped predicate into a `WHERE instance = 'x'`
SQL fragment. There is no equivalent binding in the actual jq compute-
stage evaluator (`github.com/graybeardcorp/warp/pkg/jq`) that runs for a
non-pushable expression.

**Symptom:** `._host` (and every other `._`-prefixed reserved
meta-variable — `._service`, `._namespace`, `._kind`, `._instance`, etc.)
returns `null` whenever it's read for *output* rather than used inside a
pushable `select()` filter — e.g. `jq -r '._host'` or
`jq -r '"\(._host) \(.pct)"'`. Filtering (`select(._host == "...")`)
works correctly because that path goes through SQL pushdown, which does
have real column access.

**Root cause:** the meta-variable is a synthetic column attached to the
row inside the query planner/shard layer, not a key in the row's JSON
payload. The predicate-pushdown path (`jqComparisonToSQL`) knows to map
`._host` → the real `instance` column and compiles a `WHERE` clause
against it — but a bare output read like `._host` isn't a comparison, so
it never goes through that mapping. It falls through to the jq
compute-stage evaluator, which only sees the row's raw JSON body — and
since `_host` isn't a literal key in that body, the read returns `null`.

**Why it matters:** `docs/manual/metavars.md`'s own "Per-host average
over a window" recipe relies on exactly this (reading `._host` for
output inside a `jq -r` pipeline) and does not actually work as
documented. The awk-side equivalent (`$HOST`) *does* work, but only as
the sole statement in a trivial `{print $HOST}`-shaped program — see the
next entry.

**Workaround (used in practice):** rely on a literal payload field
instead of the meta-variable, either because the producer already writes
one (e.g. weftd's local OS-metrics collector writes a plain `"host"` key
into its own JSON body, so `.host` — no underscore — works fine via
ordinary JSON-extract), or by adding one deliberately (e.g. an
application-level `Send()` call including its own identity field in the
record).

**Status:** open, no fix scheduled. Fix direction: give the jq
compute-stage evaluator the same synthetic-column bindings the SQL
pushdown path has, so a bare `._host` read resolves for any row that
carries that dimension, matching the doc's documented behavior.

## `pkill -9`-ing a coordinator process doesn't test cross-instance lease failover

**Where:** `store/leases.go`'s `AcquireLease` — `WHERE leases.holder = ?
OR leases.expires_at < ?` — plus `deploy/weft-coordinator.service`'s
`Restart=always` / `RestartSec=5`, and every lease loop's holder value
being the EC2 instance-id itself (`cmd/coordinator/main.go`'s
`coordinatorHolder()`), stable across process restarts.

**Symptom:** found running Test 1 (NAT re-election) of
`docs/plans/coordinator-chaos-tests.md` on 2026-09-19. Hard-killing the
`nat-holder`/`shard-fsm-leader` coordinator process
(`pkill -9 -f weft-coordinator`) was expected to force failover to the
surviving coordinator. Instead systemd restarted the process on the
*same* instance ~6s later, and that restarted process immediately
reclaimed essentially all of its own previously-held leases (`nat-holder`,
`shard-fsm-leader`, ~33 of ~36 `cron:*` leases) — confirmed via
`lease_event ... event=acquired` lines all timestamped at the restart
moment. The surviving coordinator's own reconcile attempts for those same
leases, ticking ~9-40s later, were denied because the "victim" had
already won them back. No failover to the other coordinator was ever
exercised.

**Root cause:** `AcquireLease`'s SQL lets a caller reclaim a lease by
matching on `holder = self` alone, with no TTL check in that branch —
correct and intentional for routine same-holder renewal, but it also
means a *freshly restarted* process on the same instance can reclaim a
lease regardless of how long it was actually down, as long as it asks
before any other coordinator's slower retry/election tick fires.
Combined with `Restart=always`/`RestartSec=5` (process back in ~6s) being
far faster than a challenger's own next attempt (NAT/shard-fsm-leader:
next 10s tick; cron: next 30s `reconcile()` tick), the original holder
almost always wins the race back to itself.

**Why it matters:** a `pkill -9` on a healthy instance is not a valid
stand-in for "this coordinator died" in a chaos test — it only exercises
"this coordinator's process crashed and came back," which is a much
weaker and less interesting case. `docs/plans/coordinator-chaos-tests.md`
Tests 1, 3, and 5 all instruct a bare process kill; as written, none of
them can actually observe real cross-instance failover unless the killed
instance is prevented from restarting (e.g. `systemctl stop` +
`systemctl mask` on the unit before the kill, or a real EC2 stop/
terminate) or is otherwise unable to win the self-reclaim race (e.g. the
instance itself is unreachable).

**Status:** open, doc fix scheduled — `coordinator-chaos-tests.md` should
be updated to mask/stop the unit (or stop/terminate the instance) rather
than bare-`pkill`, for any scenario that means to test true failover. Not
a product bug: the lease self-reclaim behavior is correct and desirable
for the ordinary crash-and-restart case, it's just not the failure mode
these chaos tests intended to simulate.

## `GracefulStop()` doesn't drain within systemd's 90s `TimeoutStopSec`

**Where:** `cmd/coordinator/main.go`'s SIGTERM handler (calls
`srv.GracefulStop()` then relies on deferred `drainCancel()`/
`provCancel()` on return) and `deploy/weft-coordinator.service` (no
explicit `TimeoutStopSec`, so systemd's 90s default applies).

**Symptom:** found running Test 1 of
`docs/plans/coordinator-chaos-tests.md` on 2026-09-19. `sudo systemctl
stop weft-coordinator` on a live, otherwise-healthy coordinator
(`i-0d4b0c33ab6b07d29`) never returned on its own — `journalctl` shows
`Stopping weft-coordinator.service...` at `15:49:59`, then `State
'stop-sigterm' timed out. Killing.` and a forced `SIGKILL` at `15:51:29`,
exactly 90s later. The unit ended in `failed`, not the clean `inactive`
a graceful stop should produce.

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

## A pipeline whose first stage isn't `cat`/`head`/`tail` silently returns nothing

**Where:** `planner/planner.go` — `QueryPlan.SourceCmd` (comment at
`planner.go:46`: "source command: `cat`, `head`, or `tail`") is the only
mechanism that establishes which pseudofile path to actually read.
`grep`, `awk`, `jq`, `sed`, etc. are pushdown/compute stages that operate
on an already-established source stream; none of them can themselves
supply the source path.

**Symptom:** a pipeline like `grep <pattern> /log/weft/coordinator/main.log`
(grep given the path directly, as real Unix grep would accept, instead
of being piped a stream) parses without error and returns zero rows —
even immediately after confirming matching rows exist via
`tail -n N /log/weft/coordinator/main.log | grep <pattern>` against the
same explicit time range. This reproduces for any pattern, not a
specific one, and independent of the query's time range.

**Root cause (not fully traced, but narrowed):** without a leading
`cat`/`head`/`tail`, there's no `SourceCmd` to anchor `QueryPlan`'s
source path/kind resolution. The pipeline doesn't error out as "missing
a source stage" — it silently produces an empty result instead. This
was found empirically (several controlled `weft.run` calls isolating
pattern vs. presence of a preceding `tail`) while live-testing coordinator
crons; the exact point where the empty result gets produced (rather than
a parse/validation error) wasn't traced further.

**Why it matters:** the MCP `weft.run` tool's own description lists
`grep` alongside `cat`/`head`/`tail` as an "allowed verb" without noting
this ordering constraint, and real Unix habit (`grep pattern file`) makes
this an easy mistake — the failure mode (silent empty result) is much
worse than a clear "expected a source stage" error would be.

**Status:** open, no fix scheduled. Fix direction: either make
`grep`/`awk`/`jq`/etc. valid as a leading stage against an explicit path
(treating it as implicit `cat` + the stage), or reject a sourceless
pipeline at parse time with a clear error instead of silently returning
nothing. Workaround: always lead with an explicit `cat`/`head`/`tail`.

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
