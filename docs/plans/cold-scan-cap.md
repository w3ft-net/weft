# Cold scan cap: metering and enforcing the cold line scan budget

> **Status (2026-11-08): plan, nothing built.** The pricing model
> (`scripts/costmodel/`, results in `cost-and-pricing.md` under "Phase 3
> results") defines the budget. The cold-shard Lambda already measures rows
> scanned (`ab397030`) but only logs it. This plan carries that number to a
> per-org meter, then enforces it.

## Goal

Each tier may read a fixed number of lines a month from cold storage. A query
that needs cold data once the month's budget is spent fails with a message that
says what happened and when it resets. Queries that stay in the hot window are
never affected.

The cap bounds worst-case Lambda spend. It is not a margin lever: a full pass
costs at most $1.54 a month on Yard (cost-and-pricing.md). So the design favors
being simple and never wrong in the customer's disfavor over being tight.

## What exists

- The cold-shard Lambda writes a trailer after the end sentinel:
  `shard.ScanStats{RowsScanned, BytesRead}` (`shard/proto_stream.go`). Rows
  scanned is DuckDB's `cumulative_rows_scanned` divided by the thread count,
  rounded up (`cmd/lambda-shard/handler.go`, `Stats`). A bare `count(*)` or a
  `LIMIT` scans nothing and reports 0, which is correct: they cost nothing.
- `ColdShard.queryToSinkRemote` reads the trailer and **only logs it**
  (`shard/cold_shard.go`). Nothing else sees it.
- Cold shards are queried from the compute pool, not the coordinator:
  `compute/compute.go` calls `sh.QueryToSink(ctx, sql, args, job)` with a
  `*compute.Job` as the `shard.RowSink`. The coordinator only sees the
  compute stream (`ExecuteQueryStreamResponse`).
- The coordinator knows the org and the shard set before it dispatches:
  `executeQuery` has `custID` and `descs`, and a descriptor with `lambda_url`
  set is cold (`proto/weftpb/weft.proto`, `ShardDescriptor`).
- Ingest metering is the pattern to copy: `CustomerUsageStats` accumulates in
  memory, `flushUsage` appends delta rows to `usage_hourly` (append-only, no
  unique key, so two coordinators never race), and `store.QueryUsage` sums.
- Tier limits live as switch functions in `store/user.go` and
  `store/retention.go` (`VolumeAllowance`, `LogRetention`, `WatchLimit`), each
  pinned by a test (`store/retention_test.go`).

## Decisions

1. **Unit: lines scanned, per org, per month.** The same period as the volume
   allowance, read through the same period helper that `QueryUsage` callers
   use. Not bytes, not queries, not wall-clock: lines are what the model costs
   and what the Lambda already reports.
2. **Allowance is a constant per tier, derived from the model.** Budget =
   `scan_passes` x cold lines stored = ingest GB x 1e9 / 155 bytes per row x
   (retention - 14) / 30. With 1 pass:

   | Tier | Lines a month |
   |---|---|
   | trial | 0 |
   | swatch | 34 M |
   | yard | 1.63 B |
   | bolt | 17.8 B |

   `ColdScanAllowance(tier) int64` in `store/retention.go`, pinned by a test with
   a comment pointing at `tiers.yaml`. The numbers are provisional until step 3.
3. **Meter after, check before.** Rows scanned is known only when a query ends,
   so enforcement is "is this month's meter already at or over the allowance?"
   before dispatch, and the query's own scan is charged after. A single query
   can overshoot by up to one query's scan, bounded by the server deadline. That
   is accepted; it is cheaper than a mid-query abort and never penalizes the
   query that was allowed to start.
4. **Exhausted means error, never a partial answer.** Per the standing rule that
   partial results are failures: if the budget is spent and the shard set
   includes any cold shard, the query fails. It does not drop the cold shards
   and answer from hot ones. The message names the remedy: a range inside the
   last 14 days (or the tier's hot window), or the reset date.
5. **Hot-only queries and zero-scan queries are free.** Nothing about the cap
   touches a query with no cold descriptor, and a cold query that scans 0 lines
   (a count, a limit) charges 0.
6. **Fail open on metering gaps.** A Lambda from before the trailer, a malformed
   trailer, or a compute from before the new field all report "no stats". That
   charges 0. Under-charging costs us pennies; over-charging breaks a customer.
7. **Crons and alerts: metered, warned at save, refused and disabled
   (decided 2026-11-08).** A cron's window is its schedule interval, and the
   schedule parser will cap that at 2 weeks. A cron normally stays in the hot
   window, but one with a window near 14 days can graze a just-archived shard.
   Such a fire is metered like any query. A save-time warning says so through
   the existing `store.Warning` channel (`lintSavedWatch`, `store/shell.go`),
   only for schedules whose window is close enough to the hot window to graze
   it; a 5-minute cron gets nothing. Once the budget is spent, a fire that needs
   a cold shard is refused like an interactive query, and the cron is **disabled
   at once** with a page to the org's `system` alias. Not three strikes: a
   refusal repeats until the month resets, so further fires would only burn
   runs. This reuses the executor's existing disable path
   (`runOutcome.typeError`, `Store.DisableCron`, `pageSystem` in
   `coordinator/cron.go`); see step 4.

## Open questions for Paul

- **Warning threshold.** `lintSavedWatch` needs the schedule (SaveCronWatch has
  it) and a "close to the hot window" cutoff. The hot window is a coordinator
  setting (14 d in prod, 1 h in dev), so the cutoff should come from the same
  place, not a literal in `store`. Pin the slack from shard rotation and archive
  lag before choosing it; I have not measured it.
- **How many passes.** 2 passes costs Yard 0.1 to 3.6 points of margin
  depending on the Lambda rate (cost-and-pricing.md, "Phase 3 results"). Decide
  after step 3 gives real numbers; the allowance is one constant per tier.
- **Visibility.** Where does a customer see remaining budget: the usage page,
  `explain`, or only in the error? Recommend all three, in that order of
  priority (step 6).

## Steps

Each step is independently shippable. The deploy order is compute, then
coordinator, then shard, with additive wire fields only, so an old receiver
stays correct (`feedback_deploy_order_wire_changes`).

### 1. Confirm the trailer is live and calibrated

- Check the deployed cold-shard Lambda is the build with `ab397030`. A
  Terraform apply reverts the Lambda to a placeholder, so redeploy after any
  apply (`project_coldshard_deploy_traps`).
- Read a day of `cold shard ...: query completed ... rows_scanned=` log lines.
- Replay the two cost samples (weft coordinator logs, loom api) and compare the
  trailer's rows scanned to the rows-in-window approximation used in
  `prices.yaml`. If the trailer disagrees by a lot, fix the model inputs before
  any code depends on it. Also confirm the thread division with
  `cmd/lambda-shard/rows_read_test.go` against a large real scan.

Done when: `prices.yaml` has Lambda $/billion lines from at least ten real
(rows_scanned, billed ms) pairs per org class, not two.

### 2. Carry rows scanned from the shard to the coordinator

- `shard`: add an optional interface, in the style of `FieldQuerier`:

  ```go
  // ScanReporter is a RowSink that wants a cold shard's scan stats.
  type ScanReporter interface{ ReportScan(shardID string, s ScanStats) }
  ```

  `queryToSinkRemote` calls it when the trailer parses, in addition to the log
  line. A sink without it is unaffected.
- `compute.Job` implements it with an atomic sum. When the stream ends, the
  final `ExecuteQueryStreamResponse` carries `int64 rows_scanned = 5` (additive;
  field 4 is reserved). Compute deploys first; an old coordinator ignores the
  field.
- The coordinator reads it where it drains the compute stream (`executeQuery`
  returns `&ExecuteResult{Stream: stream}`; the total arrives at stream end, so
  the stream wrapper needs a completion hook) and calls a new
  `Coordinator.ColdScans.Record(custID, rows)`.
- Tests: a `ColdShard` against a fake Lambda that writes a trailer reports it to
  a recording sink; `Job` sums two shards; the response field round-trips; an
  old-Lambda stream (no trailer) records nothing.

Done when: a cold query on a dev stack logs one `cold scan: org=... rows=...`
line at the coordinator, matching the Lambda's own line.

### 3. Meter per org (observe only)

- New `CustomerScanStats` beside `CustomerUsageStats` (same drain-and-reset
  design; keyed by org and day).
- New table `cold_scan_daily (org_id, day, rows_scanned)`, append-only with no
  unique key, same reasoning as `usage_hourly` (several coordinators each flush
  their own delta; `coordinator_desired = 2`). A migration at a version above
  `store/schema.sql`'s max, per the migration-collapse rule. Daily granularity is
  enough because enforcement is monthly; no rollup job, and a small purge of
  rows older than the longest billing period we read (two months) can ride the
  existing usage rollup procedure.
- `store.QueryColdScan(orgID, periodStart, periodEnd) (int64, error)`, a plain
  `SUM`.
- No enforcement. Run for a week or two in prod.

Done when: `cold_scan_daily` fills, the per-org totals look plausible against
the Lambda logs, and `prices.yaml` and the allowance table are re-derived from
the real numbers. This is the step that settles "how many passes".

### 4. Enforce

- `store.ColdScanAllowance(tier)` and its pinning test (decision 2).
- In `executeQuery`, after `descs` is known and before compute is called: if any
  descriptor has `LambdaUrl` set, read the org's period total and compare it to
  the allowance. At or over: return a typed error
  `ErrColdScanBudgetExhausted{Used, Allowance, ResetsAt}`.
- The check is one helper, called from every place that has a shard set. That
  is `executeQuery`, `executeTraceList`, **and `dispatchLoweredWithTier`**: a
  cron's hot path is the lowered one, which builds its own descriptors and never
  goes through `executeQuery`, so a check placed only there would miss every
  lowered cron.
- Crons: the helper's error is `ErrColdScanBudgetExhausted`. The cron executor
  maps it (`errors.As` on the `ExecuteForWatch` error) to the same outcome as a
  type error: status `error`, `DisableCron` (which returns true for exactly one
  coordinator, so the page goes out once), then `pageSystem` with the cause
  ("this month's cold history budget is used"), how much it used, the reset
  date, and how to re-enable. A cron the budget disables stays disabled after
  the reset until someone re-saves it, as with any disabled cron.
- Keep the check cheap. An in-process cache of the org's period total, refreshed
  from Postgres every 30 s and bumped locally by the coordinator's own unflushed
  delta. With two coordinators each sees the other's scans up to one refresh
  late; the overshoot is bounded by that window and is acceptable (decision 3).
- Map the typed error to a user message that follows the `explain` voice: user
  terms only, no shard or Lambda vocabulary. For example:
  "This month's cold history budget is used (1.63 B of 1.63 B lines). Queries
  over the last 14 days still work. It resets on 1 December." Check `shell` and
  `api` error mapping so the web shell and CLI both show it unwrapped.
- There is no feature flag. The coordinator's `-cold-scan-enforce` setting was
  removed (2026-11-08, Paul): the budget is always enforced, and backing it out
  means a deploy.
- Tests, each failing first: exhausted budget plus a cold descriptor errors;
  exhausted budget plus hot-only descriptors does not; a cold query that
  reports 0 rows charges 0; trial (allowance 0) errors on any cold range;
  a missing-trailer query charges 0 (decision 6); a lowered cron fire with an
  exhausted budget and a cold descriptor is refused, disabled once and paged
  once across two executors; a hot-only cron is untouched; the two-coordinator case with
  a stale cache stays within the bound. DB tests need `-tags test` and `-p 1`.

Done when: on a dev stack, a Yard org with a lowered allowance hits the error on
its second cold query, and a hot query in the same minute succeeds.

### 5. Visibility

- Usage API and page: used, allowance, and reset date next to the ingest meter
  (extend wherever `QueryUsage` is surfaced). Frontend elements need
  `data-testid`s per the repo rule, and the React app must compile.
- `explain`: for a pipeline whose range reaches cold data, say that it will
  read cold history and how much of the budget the range could use, using the
  existing retrieval line counts (`explain-counts`), phrased as a count, not a
  guess. This depends on the query-limits stack landing first.
- Manual and pricing copy: a "cold history budget" line in
  `frontend/src/components/PricingTable.tsx` and the `docs/manual` limits page,
  then `make docs`. No mention of the interpreter or the Lambda.

### 6. Follow-ups, not in scope

- Aborting a query mid-scan when it crosses the allowance. Needs the Lambda to
  watch its own count; the overshoot bound makes it unnecessary for now.
- Per-query scan caps (a different control from the monthly budget).
- Billing overage for extra passes through Stripe. Decide only if customers
  ask; a budget at 1 pass is cheap enough to simply raise by plan.

## Risks

- **The trailer undercounts or overcounts.** Rows scanned is a thread-divided
  profile figure, rounded up. Step 1 validates it before money or errors depend
  on it.
- **Two samples set the allowance.** Mitigated by step 3 and by the fact that
  the allowance, not the price, is what we tune; the constants are one place.
- **Cache staleness lets one org overshoot** by roughly (refresh interval x scan
  rate). Bounded and tiny against the $1.54 pass cost.
- **Lambda redeploy trap.** If a Terraform apply reverts the Lambda to the
  placeholder, metering silently drops to 0 (decision 6 makes that safe but
  invisible). Alert when cold queries run and the day's recorded scan is 0.
- **Orgs with no `system` alias get no page** when a cron is disabled
  (`pageSystem` is silent then, by design). For this feature that means an alert
  can go quiet unnoticed. Consider a visible marker on the cron (the cron list
  already shows disabled) and a line in the save-time warning telling the user
  to define the alias.
- **Interactive use can disable a cron.** A user's large searches exhaust the
  budget, and the cron that grazes cold data is the one disabled. The save-time
  warning has to say this plainly.
