# Coordinator horizontal scaling hazards

The coordinator AMI/design assumes exactly one coordinator instance is ever
running. `coordinator_desired` is pinned to `1` today
(`infrastructure-live/prod/weft/terraform.tfvars:13`). This doc catalogs what
breaks if that's bumped, so we can work through each one independently. None
of the items below are behind a leader election or work-partitioning scheme —
they were each written as if `len(coordinators) == 1` were an invariant.

## Status

| # | Issue | Status |
|---|-------|--------|
| 1 | NAT gateway self-election | **Done** — option (a), lease-based failover, see below |
| 1a | Quick win: S3 traffic off the NAT path entirely | **Done** — `aws_vpc_endpoint.s3` applied, confirmed `available` and associated with `rtb-08515c241db4539ec` |
| 1b | Found: live prod config drift in `app_config_blob` (shard volume sizes, Stripe price IDs) | **Done** — applied, `app_config_blob` no longer drifts |
| 2 | CloudWatch poller double-ingest | **Done** — per-log-group lease, see below |
| 3 | Cron/watch executor double-fire | **Done** — per-watch lease, see below |
| 4 | In-memory per-process state (tail/ingest/usage) | **Usage stats done** (append-only flush, see below); tail/ingest buffers still open |
| 5 | Shard FSM/registry convergence race | Not started — needs the same lease primitive, as a single global "shard-fsm-leader" lease |
| — | Generic lease primitive (`store.AcquireLease`/`AcquireAndHold`) | **Done** — see below; also fixed a latent concurrent-migration bug this uncovered |

---

## 1. NAT gateway self-election (infra, not app code) — outage risk

**Where:** `infrastructure-catalog/modules/asg-application/user-data.sh.tpl:140-193`

Every instance with `Role=coordinator` runs, at boot:
- `aws ec2 modify-instance-attribute --no-source-dest-check`
- `aws ec2 modify-network-interface-attribute --source-dest-check Value=false` on its own ENI
- `aws ec2 replace-route` (falling back to `create-route`) pointing the shared
  private-subnet route table's `0.0.0.0/0` at its own ENI
  (`aws_route_table.nat_private` in `infrastructure-catalog/stacks/asg-weft-application/main.tf:236-296`,
  route table ID handed down via the `/${app_name}/config/nat-route-table-id`
  SSM param)

With N coordinators this is a boot-order race — whichever instance boots (or
reboots) last silently "wins" the route. There is no ongoing reconciliation,
only a one-shot claim at boot. If the ASG later terminates (scale-in, health
check, AZ rebalance) the coordinator instance that currently owns the route,
every shard/compute instance in the private subnet loses internet/AWS-API
egress until some *other* coordinator happens to reboot and re-claim it.

Fix direction likely means: NAT should not live on the coordinator role at
all (there's already a separate `proxy` role in the same user-data script
that does the identical NAT setup — worth understanding why coordinator also
claims it), or the route-claiming needs to become health-check-driven instead
of boot-only.

### Why the coordinator is pinned to a single AZ today (context, not part of the bug)

Dug through `infrastructure-catalog` history. Originally (commit `55ecb53`,
2026-03-30) the coordinator had a persistent 1GB EBS "config store" volume,
and EBS volumes are AZ-local — the ASG got pinned to that AZ so the volume
could always attach. That volume was later **deleted entirely** (commit
`3cda6cf`) once config moved to Postgres, but the AZ pinning was **kept
deliberately** — the comment was rewritten to `# AZ pinning (cost
optimization: fewer public IPs)`. So single-AZ is a live, intentional cost
decision, not leftover cruft — any fix that reintroduces multiple AZs is
reopening that tradeoff on purpose and should be called out as such.

### Options discussed for fixing NAT self-election

**(a) Dynamic lease-based failover.** Build a generic Postgres lease
primitive (`leases` table: `name, holder, expires_at`; atomic
`INSERT ... ON CONFLICT ... WHERE holder = $self OR expires_at < now()`
upsert). Whoever holds the `nat-holder` lease calls `ec2:ReplaceRoute`
(idempotent) to point the route at itself; a dead holder's lease expires
within one TTL window (~15-30s) and another instance claims it — bounded
failover instead of "whenever some other coordinator happens to reboot."
This same primitive is the fix for #2 and #3 below, so it's worth building
once and reusing three times rather than three bespoke solutions. Open
design question: needs a bias toward the incumbent holder, or every routine
deploy (fleet cycling) will flap the NAT target.

**(b) Split API out of coordinator, NAT through API instead.** `cmd/api/main.go`
is meaningfully lighter than the coordinator — no CloudWatch poller, no cron
executor, no shard FSM, no in-memory tail/ingest buffers. Today it's
co-located (`deploy/startup.sh` starts `weft-api` on every coordinator host;
`--coordinator` defaults to `localhost:9020`), but the address-substitution
pattern to point it at a remote coordinator already exists elsewhere in the
same script (`COORDINATOR_NLB`, used for the shard role). Splitting API onto
its own single-instance role and moving NAT duty there unblocks scaling the
*coordinator* ASG (the role we actually want to scale, for ingest/shard-mgmt
capacity) without solving the general N-instance problem — it just moves the
singleton onto a role with no near-term scaling pressure. Cheapest, most
targeted option; defers rather than solves.

**(c) Real multi-AZ NAT (two AWS-managed NAT Gateways, one per existing
`nat_private` AZ).** Fixed cost ballpark ~$0.045/hr each ≈ **~$66/month
combined** (verify current AWS pricing before budgeting), plus ~$0.045/GB
processed on top of normal data-transfer-out — this is the part to watch,
since (before 1a below) all shard-archiver S3 traffic rides this path with
no separate metering today. All-in estimate **~$70-90/month**, vs. ~$0
incremental today (NAT rides the `t4g.nano` coordinator you're already
paying ~$3/month for regardless). Cheaper alternative: two self-managed NAT
instances (~$6/month total) instead of managed Gateways, but then you own
the failover logic per-AZ yourself (effectively (a), twice).

**Recommendation given "run lean":** do 1a (below) regardless of everything
else — it's free. Do (b) next to unblock coordinator scaling now. Treat (c)
as deliberately deferred, consistent with `docs/plans/az-resilience.md`
already deferring AZ resilience for the shard fleet ("not a day-1 concern").

**Implemented 2026-09-15: option (a), lease-based dynamic failover.**
Chosen over (b)/(c) per explicit direction — (a) solves the general
N-coordinator case standalone rather than deferring it onto a second
singleton role.

- **`coordinator/nat.go`** (new): `NATElector` runs a background loop —
  `store.AcquireLease("nat-holder", holder, LeaseTTL)` every
  `LeaseRenewInterval` (the same primitive #2/#3 use, reused a third
  time as anticipated). Only the transition from not-holding to
  holding calls `ec2:ReplaceRoute` (falling back to `CreateRoute`) at
  the instance's own ENI — a renewal tick that's still held does not
  re-touch the route. On `ctx` cancellation, a held lease is released
  (best-effort) so the next holder doesn't have to wait out the full
  30s TTL.
- **Resolved the doc's open "incumbent bias" question without new
  code**: `AcquireLease`'s existing `WHERE holder = $self OR
  expires_at < now()` already means a healthy incumbent's renewal
  never loses to a challenger's concurrent acquire attempt — a
  challenger only wins once the incumbent has missed a full `LeaseTTL`
  of renewals. A routine fleet-cycling deploy (new instance boots
  alongside a still-healthy old one) can't trigger a flap under this
  primitive. Verified directly in
  `coordinator/nat_test.go`'s `TestNATElector_DoesNotChallengeHealthyIncumbent`
  / `TestNATElector_ChallengerTakesOverAfterIncumbentExpiry` pair
  against a real Postgres, with a fake EC2 client recording
  ReplaceRoute/CreateRoute calls.
- **`cmd/coordinator/main.go`**: wires `NATElector` in alongside the
  poller/cron holder setup, reusing the same `holder` and `ctx`. Reads
  `NAT_ROUTE_TABLE_ID` from the process env (already populated —
  see below) and resolves the instance's own ENI once at boot via a
  new `coordinator.ResolveOwnENI` (an `ec2:DescribeInstances` call,
  same lookup the old bash claim used). No-ops off-EC2 or when
  `NAT_ROUTE_TABLE_ID`/IMDS instance-id aren't available — the
  expected shape in dev/docker.
- **`infrastructure-catalog/modules/asg-application/user-data.sh.tpl`**:
  split the shared `proxy`/`coordinator` boot block. Both roles still
  unconditionally set up IP forwarding/masquerade and disable
  source/dest-check (harmless without also owning the route), but only
  `proxy` still does the boot-time `replace-route`/`create-route`
  claim — weft's coordinator role no longer does, since the Go elector
  above now owns that. Confirmed via `grep` that weft is the only
  consumer of `instance_role = "coordinator"` in this shared module
  (django-application/vimgym/shared-haproxy use `proxy`/`web`/`worker`/`app`),
  so this split doesn't touch any other app's boot behavior.
- **`infrastructure-catalog/.../asg-weft-application/main.tf`**: removed
  `aws_ssm_parameter.nat_route_table`, the standalone
  `/${app_name}/config/nat-route-table-id` param the old bash claim
  read via a live `aws ssm get-parameter` call. Turned out to already
  be redundant before this change — `NAT_ROUTE_TABLE_ID` has been
  present in `app_config_blob` (the kevcons-managed config, flowed
  into `/opt/app/.env` for every role including coordinator) the whole
  time, and a comment on the standalone param already flagged it as
  "removable once user-data reads NAT_ROUTE_TABLE_ID from the kevcons
  blob instead" — which is effectively what happened, just via the Go
  process's env rather than bash. So the Go side needed zero new
  Terraform/SSM plumbing.
  **`terraform apply` not run** — this deletes a live SSM parameter;
  per the terraform-safety rule, run a scoped `terraform plan` on this
  stack first to confirm it's exactly the one param being destroyed
  before applying.
- No IAM changes needed: `aws_iam_role_policy.coordinator_nat`
  already grants `ec2:ReplaceRoute`/`CreateRoute` (Resource `*`), and
  `ec2:DescribeInstances` was already granted on the main instance
  role for other purposes (shard discovery/launch).
- **Not yet done / left for a deploy**: none of this is live until the
  next coordinator AMI build + rollout picks up the new binary and the
  updated user-data. The coordinator ASG is still pinned to
  `coordinator_desired = 1` — this change makes running N>1 safe for
  NAT specifically, it doesn't itself bump the desired count (that's a
  separate decision once #4 and #5 are also addressed, since those are
  still open for N>1).

## 1a. Quick win: take S3 traffic off the NAT path (S3 Gateway VPC Endpoint) — IMPLEMENTED, PLAN READY

Today, shard/compute instances uploading parquet to S3 for archival go
`instance → nat_private route table → 0.0.0.0/0 → coordinator ENI → S3`,
even though S3 never actually needs internet routing.

**Correction during investigation:** this account has *two* VPCs in
us-east-2 with the same CIDR (`172.31.0.0/16`) — the real default VPC
(`vpc-0597bcb3fcfbe4641`, where weft's `nat_private` subnets/route table
actually live) and a separate, non-default VPC (`vpc-01de623b626c8d4e5`,
tagged `aws-controltower-VPC`, created by the Control Tower account-factory
CloudFormation baseline). An S3 gateway endpoint already exists — but in
the *Control Tower VPC*, not the one weft uses. VPC endpoints are
VPC-scoped, so it can't be reused. Confirmed via
`kevcons aws ec2 describe-vpc-endpoints --filters Name=vpc-id,Values=vpc-0597bcb3fcfbe4641`
that weft's actual VPC has **zero** gateway endpoints today — so a new one
is required, and AWS's one-gateway-endpoint-per-service-per-VPC quota
isn't in the way (that quota is per-VPC; this VPC has none).

`aws_s3_bucket.cold_storage` (the archive target,
`infrastructure-catalog/stacks/asg-weft-application/main.tf:1181`) is
same-region/same-account, so this is purely a routing change with zero
application-code involvement.

**Implemented** in `infrastructure-catalog/stacks/asg-weft-application/main.tf`
(after the `aws_ssm_parameter.nat_route_table` resource):

```hcl
resource "aws_vpc_endpoint" "s3" {
  vpc_id            = data.aws_vpc.default.id
  service_name      = "com.amazonaws.${var.region}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = [aws_route_table.nat_private.id]

  tags = merge(var.default_tags, {
    Name = "${var.app_name}-s3-gateway"
  })
}
```

Verified via a scoped `terraform plan` (targeting just this resource plus
two required no-op "moved" state-address relabelings elsewhere in
`instance_role`) that this is exactly `1 to add, 0 to change, 0 to destroy`
— it does not touch anything else in the stack. Saved plan file:
`/tmp/weft-s3-endpoint.tfplan`. `terraform apply` on this stack is gated
by the auto-mode classifier (protected IaC apply) — needs to be run
manually:

```bash
cd ~/dev/infrastructure-live/prod/weft
terraform apply "/tmp/weft-s3-endpoint.tfplan"
```

Caveats:
- Doesn't help AWS API calls that aren't S3/DynamoDB (SSM, CloudWatch Logs,
  Secrets Manager still need NAT or a paid interface endpoint) — but S3 is
  almost certainly the largest volume.
- Gateway endpoints are strictly regional — only helps while `cold_storage`
  stays in the VPC's region (true today).

Net effect once applied: archiver traffic no longer depends on the NAT path
being up at all, and is free of any future NAT Gateway per-GB metering
regardless of which option from #1 gets picked.

### Unrelated but important: found live prod config drift while verifying this

Running a full (untargeted) `terraform plan` on this stack surfaced that
`aws_ssm_parameter.app_config_blob` (`main.tf:1118`, explicitly documented
as "no ignore_changes: apply is authoritative") has drifted from live
reality in two ways that would silently regress prod if anyone ever runs a
plain `terraform apply` here:

- `SHARD_SMALL_VOLUME_GB`, `SHARD_MEDIUM_VOLUME_GB`, `SHARD_LARGE_VOLUME_GB`
  are declared in Terraform vars as `2` / `50` / `100` GB but live prod is
  actually running `100` / `500` / `2000` GB. Doesn't resize existing
  volumes, but would size every *newly launched* shard far too small —
  a disk-full incident waiting for the next shard rotation/scale-out.
- All six `STRIPE_PRICE_*` vars in
  `infrastructure-live/prod/weft/main.tf:51-56` don't match the live price
  IDs currently in the SSM blob — an apply would silently revert live
  billing to old/possibly-deactivated Stripe prices.

Also present (lower risk, not yet acted on): a ~2-week backlog of
already-committed, not-yet-applied infra-catalog changes (loom-producer
instance role scoping, `weft-api-alb` HTTP:80 listener) and an
`aws_db_instance.postgres` / `aws_db_parameter_group.postgres` in-place
update (RDS param `checkpoint_completion_target` apply-method
normalization, and a date-templated `final_snapshot_identifier` that will
show as changed on every future plan — a known Terraform footgun, only
consumed if the DB is ever deleted).

**Fixed 2026-09-15.** For shard volumes: ground-truthed against a real
running shard (`i-00424bea779347555`, launched 2026-09-13, small tier,
100 GB volume attached) rather than guessing — confirmed live values are
the intended ones, Terraform was stale. Updated
`infrastructure-live/prod/weft/variables.tf`:
`shard_small_volume_gb` 2 → 100 (the 2026-06-19 DuckDB spill-ceiling
experiment, commit `1f154a5`, was superseded live but never backfilled),
and wired through `shard_medium_volume_gb`/`shard_large_volume_gb`
(500/2000, previously not passed to the module at all — silently used the
catalog defaults of 50/100 instead). For Stripe: confirmed with the user
that the live SSM values are correct (an operator rotated prices via
`kevcons config set` after a Stripe-side change and never backfilled
Terraform); updated all six `stripe_price_*` values in
`infrastructure-live/prod/weft/main.tf:51-63` to match. Re-planned and
confirmed via the same before/after key diff used to first find this:
`app_config_blob`'s computed value is now byte-identical to live —
it no longer appears in the plan's changed-resource list at all (dropped
from `7 to change` to `6 to change`). Remaining plan items are just the
pre-existing loom-producer/api-alb backlog and RDS param-group
normalization noted above, still unrelated to this task and still not
applied.

## 2. CloudWatch poller double-ingest

**Where:** `coordinator/poller/poller.go:35-71` (`Start`), `:99-155` (`poll`)

`Poller.Start` spawns one goroutine per enabled `log_groups` row, per
process, with no lease/lock. Each tick reads `LastPollAt`/`LastToken` off the
Postgres row, fetches from CloudWatch, parses, and pushes rows to the
`IngestingShard`. With two coordinators both running this loop against the
same DB, both read the same starting token and both ingest the same
CloudWatch events — every CloudWatch-sourced customer gets duplicate log
rows the moment a second coordinator comes up. This is a correctness bug,
not just wasted work.

**Fixed 2026-09-15.** `pollLoop` now calls `store.AcquireAndHold(ctx, p.store,
fmt.Sprintf("poller:%d", lg.ID), p.holder)` before doing anything — if the
lease can't be acquired (another coordinator already owns this log group),
it logs and returns without polling at all. On success, the rest of the
loop runs against `leaseCtx` (derived from the lease, not the raw parent
ctx), so an in-flight CloudWatch fetch gets canceled promptly if the lease
is ever lost mid-poll. `holder` is a new `Poller.holder` field, threaded in
from `New(store, provider, holder)`; `cmd/coordinator/main.go` computes it
once via `coordinatorHolder()` (EC2 instance-id via IMDS, falling back to
`hostname-<8 hex chars>` off-EC2).

## 3. Cron/watch executor double-fires every scheduled job

**Where:** `coordinator/cron.go:30-83` (`Start`/`reconcile`)

`CronExecutor.reconcile` loads every `enabled` `CronWatch` row and spawns a
goroutine per watch, independently in each process — the `sync.Mutex` there
(`ce.mu`) only guards the local in-process map, not cross-instance state.
With N coordinators, every scheduled alert / materialized-view refresh runs
N times: duplicate Slack/email notifications, duplicate side effects.

**Fixed 2026-09-15.** Same primitive as #2, applied to `runWatch`: the
lease (`fmt.Sprintf("cron:%d", watch.ID)`) is acquired *after* the existing
tier/min-interval validation check, not before — a watch that fails
validation and returns immediately shouldn't squat on a lease it'll never
use. On lease failure, `runWatch` calls the new `ce.forgetWatch(watch.ID)`
before returning: `reconcile` stores the cancel func and spawns the
goroutine speculatively, so without this cleanup step it would believe the
watch was "running" forever and never retry, even after the real holder's
lease expires. `CronExecutor` gained a `Holder` field, set from the same
`coordinatorHolder()` value as the poller.

Both fixes share `store.AcquireLease`/`store.AcquireAndHold`
(`store/leases.go`, migration `075_leases_table.sql`) — see the writeup at
the bottom of this doc for the primitive itself and a bug it uncovered in
`store.Migrate`.

## 4. In-memory per-process state (tail / ingest / usage)

**Where:** `cmd/coordinator/main.go:127-134`

`TailBroker`, `RingBuffer(Ingest)`, and `CustomerUsageStats` are wired up as
pure in-memory, per-process state with no cross-instance sync. Behind an
ALB/NLB round-robining connections across N coordinators, live tail and
"recent ingest" views only reflect whatever slice of traffic happened to
land on the instance a given client is connected to. **Still open** — no
design work done yet. Unlike usage stats below, this isn't a leader/lease
problem (nothing needs mutual exclusion); it's fragmented read-side state,
which needs either centralizing the data (shared buffer/pub-sub) or sticky
routing so a customer's tail/query connections consistently land on the
instance actually ingesting their data. Separate design question from the
lease work in #1/#2/#3/#5.

### Usage stats — DONE, fixed 2026-09-15

Traced `CustomerUsageStats.CurrentHour` (`coordinator/usage_stats.go`) to
its only caller: `GetUsage` (`coordinator/service/server.go`), a read-only
customer-facing usage-display RPC. Checked `coordinator/service/ingest_limiter.go`
and `shell/tier_limits.go` — neither references usage stats at all. So the
earlier worry ("could a customer bypass quota by spreading traffic across
coordinators") doesn't apply — there's no quota enforcement wired to this
counter today, only a dashboard number.

The real (minor, cosmetic) gap: `StartUsageFlush`'s old `DrainCompleted`
skipped the current, still-accumulating hour, so a customer's "usage so far
this hour" was accurate only on whichever single instance most recently
handled their traffic.

Fixed by making `usage_hourly` genuinely append-only instead of
upsert-and-accumulate:

- **Migration `store/migrations/074_usage_hourly_append_only.sql`**: drops
  the `(customer_name, hour)` primary key (the constraint the old
  `ON CONFLICT DO UPDATE` upsert depended on) and replaces it with a plain
  non-unique index for range-scan performance. `usage_hourly` is
  `PARTITION BY RANGE (hour)` with a default partition — confirmed the
  constraint drop cascades through the partition correctly by running the
  migration against a real Postgres, not just reading it.
- **`coordinator/usage_stats.go`**: `DrainCompleted` → `Drain`. Completed
  hours are still returned-and-deleted; the *current* hour is now also
  drained every tick via `atomic.Int64.Swap(0)` (delta since last drain)
  but its map entry is kept, not deleted, so `Record` calls after a drain
  keep accumulating into the same bucket instead of silently starting a
  fresh one. Added `coordinator/usage_stats_test.go` covering exactly this
  (completed-hour removal, current-hour reset-not-deleted, no
  double-reporting on a second drain with nothing new recorded).
- **`coordinator/coordinator.go` `flushUsage`**: plain `INSERT`, no
  `ON CONFLICT` — every flush from every coordinator instance appends its
  own row. Flush interval dropped from 5 minutes to 30 seconds, since
  appending is cheap (no lock contention on a shared row to worry about
  the way an UPSERT-under-concurrency question would raise) and a shorter
  interval directly shrinks the multi-instance display-staleness window.
- **No read-side changes needed** — `store.QueryUsage` and the
  `rollup_usage_daily()` stored procedure already `SUM(bytes_ingested)
  GROUP BY` rather than assuming one row per (customer, hour); this was
  already the right shape, just not exploited on the write side. Verified
  against a real Postgres: `TestRollupUsageDaily_AggregatesHourlyCorrectly`
  and `TestQueryUsage_SplitsOnTodayBoundary` both still pass, migrated
  through version 74, with the test helper (`store/usage_test.go`) updated
  from upsert to plain insert to match.

Why append-only instead of keeping the additive `+=` upsert (which was
already technically safe under concurrent instances, just less obviously
so): no shared mutable row for concurrent flushes to contend over, a
durable per-flush audit trail instead of one collapsed number, and no need
to reason about upsert-under-concurrency correctness at all.

## 5. Shard FSM/registry convergence race

**Where:** `cmd/coordinator/main.go:150` (`logdb.Start`), `logdb/shardsm.go`,
`logdb/bucketsm.go`, `logdb/registry.go`

Each coordinator process hydrates the shard registry from Postgres
independently and its own FSM drives shard lifecycle convergence
(`ec2:RunInstances`, EBS attach/detach, state transitions written back to
Postgres) — see `docs/plans/asg-as-api-stateless.md` for the state machine.
There's already a known intra-process `ShardEntry` data race between the
registry lock (`r.mu`) and the state-machine lock (`s.mu`) — see the
`project_shardentry_data_race` memory / the FSM integration test that
surfaced it. Running two full coordinator processes against the same
DB-backed registry with no distributed lock is very likely a worse version
of the ghost-instance problem that was already fixed specifically for the
*shard* ASG (`docs/plans/shard-ec2-direct.md`) — duplicate `RunInstances`
calls and conflicting state writes racing on the same shard rows, just at
the coordinator layer instead of the ASG layer this time.

Needs its own investigation pass — this is the deepest of the five and
probably wants a design doc of its own once scoped. Now that the lease
primitive exists (below), the likely shape is a single global
`"shard-fsm-leader"` lease guarding only the *write*/convergence side
(`Evaluate` calls that launch/terminate/transition shards) — non-leader
coordinators would still hydrate and read the registry for query routing,
just not drive convergence. Not yet designed or implemented.

## Generic lease primitive — DONE, built 2026-09-15

Considered two options: Postgres advisory locks (`pg_advisory_lock`,
already used in this codebase for `store/migrate.go`'s schema-bootstrap
race — auto-released on connection death, no TTL to tune) vs. an explicit
`leases` table with TTL + renewal (more code, but self-heals on a *hung*
process too, not just a dead one, and is directly queryable for ops).
Chose the explicit table.

- **`store/migrations/075_leases_table.sql`**: `leases(name PK, holder,
  expires_at, acquired_at)`. `acquired_at` is preserved across
  same-holder renewals and only bumped on takeover, so `SELECT * FROM
  leases` shows how long the *current* holder's streak has run, not just
  the last renewal time.
- **`store/leases.go`**:
  - `AcquireLease(name, holder, ttl)` — one atomic
    `INSERT ... ON CONFLICT (name) DO UPDATE ... WHERE holder = $holder
    OR expires_at < now()`. `RowsAffected > 0` means the caller holds it
    (fresh/renewed/taken-over); `0` means someone else holds a
    non-expired lease. No read-then-write race window.
  - `ReleaseLease(name, holder)` — best-effort; an unreleased lease just
    expires on its own, so skipping it on a crash is never a correctness
    problem.
  - `AcquireAndHold(ctx, store, name, holder)` — tries once to acquire,
    then (on success) spawns a background goroutine renewing every
    `LeaseTTL/3` and returns a derived context that's canceled the
    instant renewal ever fails *for any reason*, including a plain DB
    error — fail closed, since the alternative is two instances both
    believing they hold the same lease. This is the primitive #2 and #3
    both use; #5 (shard FSM leadership) will reuse it too.
- **`store/leases_test.go`**: fresh acquire, same-holder renewal, blocked
  by another non-expired holder, takeover after expiry, release only by
  the current holder, and `AcquireAndHold`'s hold/no-hold behavior. All
  passing against a real Postgres.
- **`LeaseTTL = 30s`, `LeaseRenewInterval = 10s`** (`store/leases.go`) —
  bounds failover time to 30s and gives two missed renewal ticks of slack
  before losing a lease to a GC pause or transient DB blip.

### Bug uncovered while testing this: concurrent `store.Migrate` could double-apply a migration

Adding migrations 074/075 was the first real work landed in
`store/migrations/` since the last `make collapse` — previously the
directory held only `.gitkeep`, so `goose.Up` was a no-op for every
caller regardless of concurrency. `TestConcurrentMigrateBootstrapsCompleteSchema`
(4 goroutines calling `Migrate` concurrently against a virgin DB) caught
this immediately: `goose.Up` has no locking of its own in this codebase,
so two racing callers both saw migration 075 as pending and both tried
`CREATE TABLE public.leases`, one hitting
`duplicate key value violates unique constraint "pg_type_typname_nsp_index"`.

The existing `schemaBootstrapLockKey` advisory lock
(`store/migrate.go`) only ever wrapped the schema.sql restore, not
`goose.Up` — a latent gap that simply had nothing to expose it until now.
**Fixed**: widened the lock (renamed `migrateLockKey`) to span the whole
`Migrate` function — schema.sql restore and `goose.Up` now serialize as
one unit. `applyCollapsedSchema` no longer takes its own lock; it relies
on the caller (`Migrate`) already holding it. Verified: the race test
passes reliably now, and the full `store`/`coordinator`/`coordinator/service`
suites are green.
