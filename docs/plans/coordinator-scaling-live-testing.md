# Live-testing the coordinator horizontal-scaling fixes

Follow-up to `docs/plans/coordinator-horizontal-scaling.md`: all 5 hazards
there (NAT self-election, CloudWatch-poller double-ingest, cron/watch
double-fire, `TailBroker` fragmentation, shard-FSM convergence race) were
designed and coded, and `coordinator_desired` moved from 1 to 2 in prod on
2026-09-19. This doc tracks proving those fixes under real N=2 concurrency
— observability first, then a chaos harness — rather than trusting them on
paper.

## Status

**Phase 1 — instance identity + lease-event logging: done, deployed, verified.**

- `weft-go` (the SDK coordinator uses to emit its own telemetry) gained
  `HeartbeatWithFields`/`RuntimeStatsWithFields` (v0.2.0) so a caller can
  attach extra fields to every record — used to tag every heartbeat and
  runtime-stats row with the emitting coordinator's instance id.
- `store.LogLeaseEvent` emits one structured line per lease-state
  transition (`acquired`/`denied`/`lost`/`released`/`deadman_released`)
  from all 4 call sites that use the lease primitive (NAT, shard-fsm-leader,
  per-poller, per-cron).
- Confirmed live after the coordinator AMI rollout that shipped this: the
  new `coordinator_id` field is populated correctly, `lease_event` lines
  are flowing, and — as a bonus — the rollout itself exercised a real
  cold-start leader election (both `nat-holder` and `shard-fsm-leader`
  cleanly re-elected; lease ownership was cross-checked against the
  `leases` table directly and matched the log output). Notably, cron/poller
  lease ownership came out genuinely split across both new instances after
  this restart, rather than one incumbent holding everything as it had
  before the redeploy — encouraging evidence the mutual-exclusion guard
  behaves correctly under concurrent cold-boot racing.

**Phase 2 — dashboard: done.** The dogfood account's `ops` dashboard has a
new "Multi-Coordinator" section (per-instance memory/disk tables, a live
coordinator-instance-count tile, a per-instance runtime breakdown, a
lease-holder table, and lease-flap/contention sparklines). Three existing
alert crons (`coord-fds-high`, `coord-goroutine-high`, `coordinator-restart`)
were also fixed — they previously checked only the single latest log line,
which could silently miss a problem on whichever coordinator's row didn't
happen to land last; fixed to check the worst of the last 5 readings,
matching the pattern other crons in that dashboard already used.

Dashboard/cron definitions themselves are account configuration, not code —
they live only in the dogfood account's own database (edited via the weft
CLI or the `weft` MCP `*_propose`/`proposals_apply` tools), never as files
in this repo. See `docs/manual/dashboards.md` and `docs/manual/crons.md` for
the mechanism; ask the weft MCP server (`weft.crons_list`) for the current
cron set rather than looking for it here.

**Phase 3 — additional machine-checkable logging: done, not yet deployed.**

- `store.LogEvaluateEvent(kind, id, dur, err)` (`store/telemetry_events.go`)
  emits one `evaluate_event` line per bucket/shard `Evaluate` call inside
  `evaluateStateMachines()` (`logdb/provisioner.go`), timed individually.
  `store.LogEvaluateTick(buckets, shards, dur)` emits one `evaluate_tick`
  line per tick with the total duration — the line that will confirm or
  disprove the known sequential-evaluation gap (one slow shard blocking
  convergence fleet-wide) once read under real N=2 load (chaos scenario 6
  below).
- `store.LogTailFanoutEvent(op, err)` (same file) is called from all 4
  existing error sites in `coordinator/tail_fanout_redis.go` (unmarshal,
  psubscribe, punsubscribe, marshal/publish) plus a new `OnConnect` hook on
  the redis.Options — the only signal go-redis exposes for its own
  auto-reconnect, so every post-initial `connect` event there is a
  reconnect after a drop. Needed for the Redis-disruption chaos scenario
  below.
- The shard-fsm-leader deadman's-switch trigger already gets a
  `lease_event` line (`deadman_released`) as of Phase 1; no further work
  needed there.
- Not yet shipped to prod — needs a coordinator AMI rollout (like Phase 1)
  before Phase 4's chaos scenarios can rely on these log lines.

**Phase 4 — chaos harness: doc written 2026-09-19, not yet executed.** Full
8-scenario runnable test matrix in `docs/plans/coordinator-chaos-tests.md`
(exact `kevcons`/SQL commands, PASS/FAIL criteria per scenario), modeled on
`docs/plans/fsm-chaos-tests.md`'s format. Summary:

1. NAT re-election — hard-kill the current `nat-holder`.
2. Poller double-ingest — hard-kill a `poller:<id>` holder, confirm no
   duplicate ingested rows.
3. Cron/watch double-fire — hard-kill a `cron:<id>` holder, confirm a
   watch runs once, not twice.
4. `TailFanout` under Redis disruption — restart/partition Valkey with
   active `tail -f` subscribers pinned to each coordinator via SSM
   port-forward + `WEFT_ENDPOINT`.
5. shard-fsm-leader flap under load — hard-kill the current leader;
   confirm re-election and no shard double-launch.
6. Slow-shard convergence stall — purely observational, reads Phase 3's
   `evaluate_event`/`evaluate_tick` lines to confirm or disprove the
   known sequential-evaluation gap from
   `docs/plans/coordinator-horizontal-scaling.md`.
7. Scale event — `coordinator_desired` 2→3→2 (needs a `max_capacity` bump
   too, currently hardcoded to 2), check for orphaned lease rows.
8. Steady-state load check — dashboard-only, no fault injection.

Two things the write-up surfaced that change how Phase 5 should read the
results, not just how to run the tests:
- **`systemctl stop`/SIGTERM is a clean shutdown**, not a crash — every
  lease loop releases on `ctx.Done()`. Tests 1 and 5 need
  `pkill -9 -f weft-coordinator` to actually exercise the 30s-TTL
  deadman's-switch path instead of the clean-handoff path.
- **Poller leases have no reconcile loop; cron leases do.**
  `Poller.Start()` enumerates `log_groups` once at boot and never retries
  a lost lease; `CronExecutor.reconcile()` retries forever on a 30s
  ticker. Tests 2 and 7 are expected to actually surface a stuck
  `poller:<id>` lease after its holder dies — flagged in advance as a
  likely real gap (missing `Poller` reconcile loop), not a test-harness
  artifact. Test 4 additionally needs `weft-redis` (ElastiCache) confirmed
  actually applied first — it may still be Terraform-only per
  `coordinator-horizontal-scaling.md`'s last update.

Run against prod directly — no isolated test environment, per an explicit
call made when starting this work (full blast radius accepted; there is no
real external customer on this account to protect, only the dogfood
tenant).

## Findings along the way (worth keeping in mind for anything else touching crons/dashboards)

Two query-planner gaps were found and written up in `docs/known-issues.md`
while building the Phase 2 crons:

- jq's reserved meta-variables (`._host`, `._service`, etc.) resolve to
  `null` when read as plain output — they only work inside a pushable
  `select()` predicate. The awk-side equivalent (`$HOST`) only works as
  the sole statement in a trivial `{print $HOST}` program. Prefer a
  literal payload field over either meta-variable mechanism for anything
  beyond simple filtering.
- A pipeline whose first stage isn't `cat`/`head`/`tail` (e.g. a bare
  `grep pattern /log/...`) silently returns zero rows instead of erroring.
  Always lead cron/script pipelines with an explicit read verb.
