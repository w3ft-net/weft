# Plan: Postgres optimization pass

**Status (2026-10-08):** Items 0, 2, 3 and 4 are implemented in the working tree, uncommitted and undeployed (migrations 081 and 082). Item 0 (shard_heartbeats) is the largest finding from the prod baseline. Everything else is analysis only. The code-side audit of `store/` and its callers came first; items 1 and 2 were re-verified against source. A partial prod baseline (database stats, table sizes, partman and pg_cron state) was captured later the same day; see Step 0 and item 0. The full `pg_stat_user_indexes` pull is still outstanding, so confirm item 7's unused-index claims before dropping anything.

weft's Postgres is `weft-postgres`, a 1 GB t4g.micro shared with vimgym, reached through PgBouncer (transaction mode, `pool_size=10`) since 2026-10-07. Memory and write volume are the binding constraints, so this pass favors cutting writes, dead tuples, and index footprint over read latency.

## Loop frequencies (for sizing)

| Loop | Interval | Where |
|---|---|---|
| Shard heartbeat | 10s per shard | `cmd/shard/main.go:38` |
| Lease renewals (×3) | 10s per coordinator | `store/leases.go:15-16` |
| Usage flush | 30s per coordinator | `coordinator/coordinator.go:463` |
| Cron reconcile | 30s per coordinator | `coordinator/cron.go:53` |
| AgentSeen flush | 30s per coordinator | — |
| Provisioner tick | 60s per coordinator | `logdb/provisioner.go:147` |
| Overdue-cron guard | 60s per coordinator | `coordinator/cron_overdue.go` |
| `rollup_cron_hourly` (pg_cron) | 5 min | `store/schema.sql:3435+` |
| `rollup_cron_4h`, `rollup_cron_daily`, `rollup_usage_daily` | hourly | same |
| partman maintenance | 30 min | same |
| Web sidebar poll | 30s per open tab | `frontend/src/components/SidebarNav.tsx:35` |

Prod runs `coordinator_desired=2`, so every per-coordinator loop happens twice.

## Prod baseline (2026-10-08, partial)

- `weft` database: 1105 MB, **35% cache hit rate** (every other database is above 99.5%), 953M blocks read from disk, 78 deadlocks. Host is a db.t4g.micro: `shared_buffers` ~46 MB, `effective_cache_size` ~361 MB, `random_page_cost` 4.
- **`shard_heartbeats_default` is 1003 MB (604 MB heap, 399 MB indexes), about 91% of the database**, for ~75k live rows. `usage_hourly_default` (item 1) is only 17 MB (170k rows back to 2026-04-16), so item 1 is low priority.
- One query accounts for essentially all the disk reads: `SELECT DISTINCT ON (shard_id) … FROM shard_heartbeats WHERE beat_at > NOW() - $1::interval`. 16,150 calls, mean **19.6 s**, 937M blocks read.
- `partman.run_maintenance_proc()` (pg_cron job 1, every 30 min) has failed on every run since its last success at 2026-10-06 21:00. Two alternating errors: `ATTACH PARTITION … updated partition constraint for default partition would be violated by some row`, and `deadlock detected` on `DROP TABLE shard_heartbeats_p20260525_020000`.

## Item 0: shard_heartbeats redesign (implemented, not deployed)

**Root cause.** Heartbeats were an hourly-partitioned log with 5-minute partman retention. Once maintenance failed, rows landed in the default partition, which partman never trims. The adopt query then seq-scanned it for ~20 s every minute on both coordinators, holding locks long enough to deadlock the next maintenance `DROP`, which guaranteed the next run would fail too. The ATTACH failure is the same loop from the other side: rows for the new hour were already in the default partition.

**Who reads the table** (grepped across weft, kevcons, infrastructure, vimgym, gravitty):
- `adoptFromHeartbeats`, every 60 s on every coordinator, looking back 1 minute.
- `LatestShardDiskBytes`, once per export attempt, with no time bound.
- Tests, and `docs/runbooks/export-drill.md`.

Nothing reads history. The durable liveness copy is `shard_records.last_heartbeat_at`, written by `TouchHeartbeat`.

**Durability.** UNLOGGED was considered and rejected: the table is small enough that the WAL savings are negligible, UNLOGGED tables are emptied on crash or failover, and it would have taken away the one reader that needs data to outlive a restart (export sizing). Keeping the table LOGGED but tiny makes the question moot.

**Change (migration 081):**
- `shard_heartbeats` is now a plain table with one row per `(shard_id, instance_id)`, upserted by `Store.RecordHeartbeat` (renamed from `InsertHeartbeat`). Unclaimed instances beat with an empty `shard_id` and are told apart by `instance_id`.
- It is removed from partman (`part_config` row and template table dropped). `TestPartmanBackfill` no longer expects it.
- `shard_records.disk_bytes` is new, written by `TouchHeartbeat` only when positive, so a beat that reports 0 never erases the last known footprint. `LatestShardDiskBytes` now reads it, so export sizing no longer depends on transient rows. The migration backfills it from the old table before dropping that table.
- `Store.PruneShardHeartbeats` deletes rows older than `heartbeatRetention` (1 hour). `adoptFromHeartbeats` calls it each tick on every coordinator. It is idempotent, so no leader gating is needed.
- `LiveShardHeartbeats` returns the newest beat per claimed shard (so a replaced instance's old row can't resurface its dead address) plus every unclaimed instance's own beat. The old `DISTINCT ON (shard_id)` collapsed all unclaimed instances into one, so a multi-shard launch could starve all but the newest.

**Tests** (`store/shard_heartbeat_test.go`, plus edits in `logdb`):
- Five beats leave one row.
- A replaced instance returns only its newest beat.
- Two unclaimed instances are both returned.
- Beats outside the window are excluded.
- Prune drops only expired rows.
- The table is a plain relation and not registered with partman.
- `TouchHeartbeat` persists `disk_bytes`, and `LatestShardDiskBytes` reads it and keeps the last non-zero value.

`go test -tags test ./store ./logdb ./coordinator/...` passes. I did not capture a recorded red run against a real database before implementing: the local test pool DBs were stale (see below), so the pre-fix state was only confirmed as compile failures.

**Rollout (not done yet):**
1. Migration 082 (trigger split) is trivial and runs right after 081. Migration 081 runs `DROP TABLE` on the partitioned parent. It takes an ACCESS EXCLUSIVE lock and can deadlock if `run_maintenance_proc` or an adopt scan is mid-flight, and the backfill `UPDATE` seq-scans the 1 GB default partition once (expect on the order of 20 s). Deploy away from :00 and :30, when pg_cron fires.
2. Run `make collapse` after the deploy. Do not hand-edit `store/schema.sql`: the partman reconstruction block is regenerated from `part_config`, which the migration cleans.
3. The `DROP` of the old table is what returns the ~1 GB to the OS; no `VACUUM FULL` is needed.
4. Local pool DBs `weft_test_0`..`4` recorded version 81 from an earlier branch, so they skipped this migration. I dropped them (the documented remedy) and they rebuilt. Other worktrees will need the same.

**Stopgap if the deploy is delayed:** a single-transaction cleanup (`LOCK` the parent, `TRUNCATE` the default partition, drop the stale partition, `SELECT partman.run_maintenance('public.shard_heartbeats')`) would unwedge maintenance, but it only buys time and the same trap can recur.

**Adjacent finding, not fixed here:** the adopt tick is 60 s and `heartbeatStaleThreshold` (`logdb/registry.go:155`) is also 60 s. A follower coordinator learns about a shard's liveness only by polling this table, so its view sits on a knife-edge. That plausibly contributes to the follower staleness symptom behind the flaky `weft sh` reads. Item 4 below covers the related replay-stamp bug.

## Step 0: capture prod baseline

Before any index drop or retention change, capture and save:

- `pg_stat_user_tables`: size, `n_live_tup`/`n_dead_tup`, `seq_scan`/`seq_tup_read`, `idx_scan`, `n_tup_upd` vs `n_tup_hot_upd`
- `pg_stat_user_indexes` with `idx_scan < 50`, ordered by size (this confirms item 7)
- `pg_statio_user_tables` heap/index hit ratios
- `pg_stat_activity` grouped by application/state
- `partman.part_config` and `cron.job` (this confirms item 1: `usage_hourly` should be absent)
- `pg_relation_size('usage_hourly_default')` and its row count

Re-run the same queries after each tier to get before/after numbers. `scripts/auth-query-rate.sql` already gives the rate-diff pattern for item 2.

## Tier 1: small, clear-cut

### 1. `usage_hourly`: unbounded, append-only, full scan every hour

- `flushUsage` inserts one row per (org, hour) delta every 30s on each coordinator (`coordinator/coordinator.go:484-493`). That's about 240 rows per active org-hour, by design (migration 074 dropped the uniqueness so coordinators don't race).
- `usage_hourly` and `usage_daily` exist only as `_default` partitions (`store/schema.sql:1839-1855`). Neither is in the partman reconstruction (`schema.sql:3417-3431`), so nothing ever drops rows.
- `partman.template_public_usage_{hourly,daily}` (`schema.sql:587`, `598`) are orphaned.
- `QueryUsage`'s comment (`store/usage.go:37-41`) assumes partman drops hourly partitions. That's no longer true.
- `rollup_usage_daily` (`schema.sql:339-350`) filters on `hour >= …` with no org predicate. The only index is `(org_id, hour)` (`schema.sql:2916`), so each hourly run sequentially scans the whole growing table.

**Change:**
- Register `usage_hourly` with partman (daily partitions, retention a few days past the rollup window). Decide whether `usage_daily` needs retention at all; billing may want it forever.
- Index `hour`, or add BRIN on it, for the rollup.
- Optional, bigger win: accumulate per (org, hour) in memory and upsert on `(org_id, hour, coordinator_id)`. That keeps the no-race property and cuts ~240 rows to ~2.
- Fix the `QueryUsage` comment.

### 2. Unconditional `UPDATE users` on every authenticated request

- `persistLastActive` (`api/middleware.go:64`, `77-87`) calls `UpdateUserLastActiveOrg` on every request. The comment says it runs in a goroutine and only when the value differs; it does neither.
- `UpdateUserLastActiveOrg` (`store/user.go:289-293`) claims to be a no-op when nothing changed. It always sets `updated_at = now()`, so every request is a real row write: a dead tuple plus WAL.

**Change:** skip the write when `session`'s user already has `LastActiveOrgID == ActiveOrgID` (already loaded by `BuildSession`). Correct both comments. A test should assert that two requests in the same org produce zero `users` updates.

Related, same middleware:
- `BuildSession` (`auth/session.go:86-143`) runs 5 SELECTs per request (user, `ListUserOrgs`, `TierForOrg`, `IsOwner`, `GetOrgPermission`). A short-TTL session cache is a later option.
- The 30s sidebar poll hits `handlePendingSources` (`api/sources.go:155-168`). That handler loads `log_groups.*`, including the `sample`/`detection` blobs, just to count rows, and re-resolves the org by name although the session carries `ActiveOrgID`. Replace it with a `COUNT(*)` keyed on the session org.

**Implemented.**
- `Session` now carries `LastActiveOrgID` (what `users.last_active_org_id` held when the session was built), and `persistLastActive` skips the write when it already equals the active org.
- `UpdateUserLastActiveOrg` now has `AND last_active_org_id IS DISTINCT FROM ?` in its WHERE clause, so the six other callers (login, invites, admin) are real no-ops too, as its comment always claimed. Both comments are corrected.
- New `GET /api/sources/pending/count` backed by `Store.CountPendingLogGroups`. The sidebar badge uses it instead of fetching every pending source with its sample and detection blobs just to take the length. The list handler also uses `session.ActiveOrgID` instead of re-resolving the org by name.
- Tests: `api/last_active_write_test.go` (repeat requests leave the `users` row's `ctid` unchanged; same-org `UpdateUserLastActiveOrg` is a no-op), `api/sources_pending_count_test.go`. Both failed before the fix. The frontend build passes.
- Not done: the per-request `BuildSession` cache (5 SELECTs per request) stays a later option.

### 3. Turn off GORM's default transaction wrap

`gorm.Config` sets only `Logger` (`store/store.go:120-122`), so every single-statement `Create`/`Updates`/`Delete` runs as BEGIN, statement, COMMIT: three round trips through PgBouncer. That includes the heartbeat touch, cron status updates, incident updates, and the `users` write above.

**Change:** `SkipDefaultTransaction: true`. Before flipping it, audit the callers that rely on hooks or associations writing atomically. Explicit `Transaction(...)` blocks are unaffected.

**Implemented** in `store.Connect` (the only `gorm.Open` site). Audit result:
- No model hooks (`BeforeCreate`/`AfterSave` etc.).
- No association writes: `Organization`/`Namespace` fields on `AgentToken` and `LogGroup` are only populated by `Preload` on reads.
- The two `CreateInBatches` sites (`store/tmp.go`, `mounts/tmp.go`) already run inside explicit `Transaction` blocks. Outside one, GORM would stop wrapping batches after this change.
- `Save` (`SaveShard`, `SaveBucket`) can issue UPDATE then INSERT … ON CONFLICT as two statements; both are idempotent.
- Tests run inside a transaction, where GORM never opened its own, so the suite cannot detect an atomicity regression either way; the audit above is the evidence. `store/connect_config_test.go` pins the setting.

## Tier 2: write amplification

### 4. Heartbeat path

- Each 10s beat does `INSERT shard_heartbeats` (`coordinator/service/server.go:1061`) plus `TouchHeartbeat`, an UPDATE of `shard_records` (`logdb/shardsm.go:980`, `logdb/shard.go:124-131`). The beat lands on whichever coordinator receives it; it isn't leader-gated.
- `adoptFromHeartbeats` replays every live heartbeat on both coordinators every minute (`logdb/provisioner.go:483-490`). That's another UPDATE per shard per coordinator.
- The `shard_records_audit` trigger (`schema.sql:3105`) fires on every UPDATE, including heartbeat stamps.

**Correctness bug, fix test-first:** the replay stamps `LastHeartbeatAt = Clock()` (`logdb/shardsm.go:971`) instead of the beat's own timestamp. A beat up to 59s old therefore makes a shard look fresh. This lines up with the follower heartbeat-staleness symptom behind the flaky `weft sh` reads. Write a failing test (replay an old beat, then assert `LastHeartbeatAt` equals the beat time) before changing it.

**Changes:**
- Restrict the audit trigger to `AFTER UPDATE OF state` or add a `WHEN (OLD.state IS DISTINCT FROM NEW.state)` clause.
- Skip the replay UPDATE when the stored stamp is already at least as new as the beat.

**Implemented.**
- `ShardHeartbeat.BeatAt` (an unused RFC3339 string) is replaced by `Age`, how long ago the database recorded the beat, computed in SQL as `NOW() - beat_at`. A beat just received over RPC has `Age` 0. Using an age rather than a timestamp keeps the coordinator's clock and the database's clock out of each other's way.
- `ObserveHeartbeat` stamps `Clock() - Age` and never moves `LastHeartbeatAt` backwards. It calls `TouchHeartbeat` only when the stamp advanced or `row_count`, `disk_bytes` or `binary_sha` changed. On the coordinator that received the RPC, the 60 s replay of the same beat is now a no-op; followers still write once per shard per minute.
- Migration 082 splits the audit trigger: INSERT/DELETE as before, UPDATE only `WHEN (OLD.state IS DISTINCT FROM NEW.state)`.
- Tests: `logdb/observe_heartbeat_test.go` (stamps the beat's time, never goes backwards, skips redundant persists, persists `disk_bytes` on the steady-state path), an `Age` test in `store/shard_heartbeat_test.go`, and a trigger-shape pin in `store/shard_operations_test.go`.
- Found while writing these: my first item 0 patch put the `DiskBytes` assignment in `configureOnHeartbeat` (the promotion path) instead of `ObserveHeartbeat`, so steady-state beats were not updating `disk_bytes`. Fixed here and covered by a test.
- `configureOnHeartbeat` (promotion) still stamps `Clock()`. A promotion requires a successful dial, which proves liveness, so a replayed stale beat cannot promote a dead shard.
- Not done: the 60 s adopt tick versus the 60 s `heartbeatStaleThreshold` knife-edge noted under item 0. A follower's view of a shard is at most one tick old, which is already as long as the threshold. Fixing it means either a faster adopt tick or a larger threshold; both change failure-detection timing, so it deserves its own decision.

### 5. Alert evaluation N+1

The per-row loop at `coordinator/alert.go:85-89` runs, for every tag on every tick:
- `TierForOrg` (`alert.go:150`, `273`)
- an aggregate over `cron_results_all` (`alert.go:173`)
- `ListCronResults`, a `SELECT *` including `stdout` (`alert.go:183`, `store/shell.go:647-653`)
- `checkIncident`: a SELECT, plus an UPDATE of `last_seen_at` on every tick while the incident is open (`alert.go:451-475`)

**Change:** resolve the tier once per run. Fetch previous values with a single `DISTINCT ON (tag)` that selects only the needed columns. Batch incident reads. Stamp `last_seen_at` only when it would advance meaningfully (or drop it if nothing reads it at sub-minute precision).

### 6. Cron rollups rewrite unchanged rows

- `rollup_cron_hourly` runs every 5 min and re-upserts the last 3 hours, so each completed hourly row is rewritten about 36 times. `ON CONFLICT DO UPDATE` writes even when the values are identical (`schema.sql:326-331`).
- The daily rollup re-upserts 3 days every hour.
- The `ran_at` range filter has no index leading on `ran_at`; the only one is `(cron_watch_id, tag, ran_at DESC)` (`schema.sql:2545`). Each run scans the daily partitions.

**Change:**
- Add `WHERE (t.col1, t.col2, …) IS DISTINCT FROM (EXCLUDED.col1, …)` to each rollup upsert.
- Shrink the re-roll window to what late-arriving rows actually need.
- Add BRIN on `cron_results(ran_at)` through the partman template so new partitions inherit it.

### 7. Redundant indexes

Each one costs write I/O and shared-buffer space. Confirm with Step 0's `idx_scan` before dropping.

| Index | Why it's redundant |
|---|---|
| `idx_cron_{hourly,4h,daily}_watch_tag_bucket` (`schema.sql:2517`, `2531`, `2573`, plus default-partition copies) | Same columns as the PKs (`schema.sql:2152`, `2168`, `2200`). DESC adds nothing for a btree |
| `idx_tmp_file_lines_file_line` (`schema.sql:2902`, `2951`) | Prefix of PK `(file_id, line_num, updated_at)` (`schema.sql:2440`) |
| `idx_cron_results_deleted_at` (`schema.sql:2559`, `2566`) | Busiest insert table; nothing soft-deletes `cron_results` |
| ~11 single-column `deleted_at` indexes (`schema.sql:2587`, `2615`, `2622`, `2657`, `2706`, `2755`, `2762`, `2769`, `2804`, `2818`, `2881`) | Can't serve GORM's `deleted_at IS NULL` filter. If one is needed, make it a partial index on the real predicate |
| `idx_shard_operations_ts` (`schema.sql:2860`) | No query filters on `ts` alone (`logdb/shard.go:348`, `388`) |
| `idx_memberships_org` (`schema.sql:2727`) | Mostly covered by the partial unique indexes |
| `idx_agent_tokens_version_seen` (`schema.sql:2601`) | Includes `last_seen_at`, which blocks HOT updates for the AgentSeen flush |

Partitioned-table indexes must also come out of the partman templates, or `make collapse` will bring them back on new partitions.

### 8. Provisioner reads whole tables every tick

- `RefreshFromStore` runs `ListShards` (`SELECT *` over all `shard_records`, every ARCHIVED row included) and `ListShardServices` over all rows, every 60s on both coordinators (`logdb/shard.go:96-99`, `logdb/registry.go:340`, `store/shard_service.go:46-49`). The cost grows linearly with shard history.
- `ServicesForBucket` filters on `COALESCE(bucket_id, subquery)` (`store/shard_service.go:62-66`), which no index can serve.

**Change:** load ARCHIVED rows once at boot. On each tick, fetch only non-terminal rows (or rows with `updated_at` past the last refresh). Rewrite `ServicesForBucket` as an index-friendly `OR`/`UNION`.

## Tier 3: retention and housekeeping

### 9. Tables with no retention

| Table | Growth source | Proposed retention |
|---|---|---|
| `ingestion_errors` | One insert per failed batch (`coordinator/ingestion_errors.go:68`); nothing resolves or deletes rows | pg_cron delete older than N days |
| `magic_links`, `device_codes` | Every login / device auth | Delete expired rows daily |
| `alert_incidents` (closed) | Every incident | Keep N days after close |
| `shard_operations` | Every FSM op | Keep N days, or tie to shard archival |
| `shard_services`, ARCHIVED `shard_records` | Shard history | Keep rows that retention janitor / billing still need; prune the rest |

Separately, `cron_results.stdout` is stored uncapped (`coordinator/cron.go:245-253`). The 7-day partman retention bounds the row count, not the row size. Cap the stored stdout and add a "truncated" marker.

### 10. `SELECT *` of wide rows

- Cron reconcile (30s, `coordinator/cron.go:75`) and the overdue guard (60s, `coordinator/cron_overdue.go:113`) load `pipeline` plus the `lowered_plan` jsonb just to diff IDs. Select `id, updated_at` and fetch full rows only for changes.
- `handleListSources` (`api/sources.go:372`) loads every org's `log_groups.*` (`schema_yaml`, `sample`, `detection`) and filters by customer in Go. Push the org filter into SQL. This also tightens the tenancy boundary.

### 11. Missing indexes (small tables today; cheap insurance)

- `log_groups(log_group_name)`: hit on every Firehose request; only a per-request memo protects it (`store/source.go:276-290`, `api/adapter_cloudwatch.go:99-117`)
- `agent_tokens(org_id)`, `agent_tokens(namespace_id)`: CASCADE foreign keys; `ListAgentTokens` filters on org (`store/token.go:214`)
- `users(last_active_org_id)`: unindexed foreign key, so an org delete scans `users`
- `shard_records(state)`: `StuckArchiving` (`logdb/shard.go:345`), or make it partial on non-terminal states
- The tag-less Absent query (`coordinator/alert.go:276`) can only use the `cron_watch_id` prefix of its index. Fine until it isn't.

### 12. Syslog token validation

- `ValidateAgentTokenForProducer` (`store/token.go:193-197`, called from `api/syslog_server.go:163`) skips the auth cache that `ValidateAgentToken` now uses. Every syslog connection costs 3 queries (the lookup plus two `Preload`s). Route it through the same cache.
- The `Authenticate` RPC writes `UpdateAgentMetricsConfig` unconditionally (`coordinator/service/server.go:458-462`). Write only when the value changes.

## Connection pool

No change needed now:
- `MaxOpenConns` 20 (`DB_MAX_OPEN_CONNS`), `MaxIdle` 5, lifetime 30m (`store/store.go:129-131`).
- Only `cmd/coordinator` and `cmd/api` connect, 2 instances each. PgBouncer collapses that to 10 server connections.

Follow-ups:
- The 4×20 sizing comment at `store/store.go:18-29` predates PgBouncer; rewrite it.
- pgx's per-connection statement cache (512) is larger than PgBouncer's `max_prepared_statements=100`. Either lower pgx's cache or raise PgBouncer's limit, to avoid eviction churn.
- vimgym and gravitty still connect to RDS directly. They are the remaining connection-count risk on the shared instance.

## Transactions and locks

Nothing to fix:
- No transaction spans a network call.
- `BeginExportAttempt` takes a short `pg_advisory_xact_lock` (`logdb/shard.go:272-273`).
- The migration session lock uses the direct DSN (`store/migrate.go:85`).
- Lease upserts total about 0.6/s fleet-wide.

## Order of work

0. Item 0: deploy the shard_heartbeats redesign (see its rollout notes), then re-run the baseline queries and record the before/after numbers here.
1. Step 0 baseline (finish the remaining pulls).
2. Items 2 and 3: minutes each, immediate drop in write and round-trip volume.
3. Item 1: migration plus partman registration; then `make collapse` after deploy.
4. Item 4 (failing test for the replay-stamp bug first), then items 6 and 7.
5. Items 5 and 8.
6. Tier 3, as time allows.

Re-run the Step 0 queries after each step and record the deltas here.
