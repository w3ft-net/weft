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
| 4 | In-memory per-process state (tail/ingest/usage) | **Usage stats done** (append-only flush, see below). `RingBuffer` scoped out — no fix needed (see below). `TailBroker`: designed 2026-09-17, two candidates pending a benchmark — not yet implemented |
| 5 | Shard FSM/registry convergence race | **Done** 2026-09-17 — `shard-fsm-leader` lease, see below |
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
pure in-memory, per-process state with no cross-instance sync. Unlike usage
stats (done, below) this isn't a leader/lease problem — nothing needs
mutual exclusion, it's fragmented read-side state.

### Scoping correction 2026-09-17: `RingBuffer` doesn't actually belong in this item

Original framing lumped `RingBuffer(Ingest)` in with `TailBroker` as
"fragmented read-side state." It isn't. `RingBuffer` is pure write-side
buffering — `ingestBatch` (`coordinator/service/server.go:770`) writes rows
into it and `StartDrain`'s background loop (`coordinator.go:352`) drains
and routes them to shards; "the write path always goes through it — there's
no scenario where a caller benefits from skipping it"
(`coordinator.go:307`). Nothing reads it cross-process — the only
consumers are that same process's own drain loop and a local `UsagePct()`
metrics gauge, which is legitimately per-instance-meaningful (you want to
know *that coordinator's* buffer pressure, not a merged fiction). Each row
only ever arrives at one coordinator to begin with, so there's no
duplication risk (unlike poller/cron) and no fragmented-*view* risk (unlike
tail) — every coordinator just independently drains what it received into
the right shard, which needs zero cross-instance work. The one real
property worth naming, not new and not multi-coordinator-specific: rows
sitting in `RingBuffer` when a coordinator crashes are lost from memory —
whether that matters depends on whether the sender (weftd, the CloudWatch
poller) redelivers on a dropped connection, untraced. Out of scope for this
item; `RingBuffer` needs no design work.

So this item is really just **`TailBroker`.**

### The actual problem

`ingestBatch` calls `Tail.Broadcast(rows)` (`server.go:782`) purely
locally, same call site as the `RingBuffer.Write` above. A live `tail -f`
subscriber only sees rows ingested by the exact same coordinator process
they're gRPC-connected to — not stale, just silently empty if their data
arrives elsewhere.

**Sticky routing (route a customer's tail connection to whichever
coordinator is ingesting their data) doesn't actually resolve cleanly.** A
single customer's ingest isn't reliably single-homed to one coordinator
today: CloudWatch-sourced ingest is pinned to one coordinator, but only as
a side effect of #2's per-log-group lease, not by design for this purpose.
A customer's weftd agents (one per monitored instance, potentially several)
connect independently via the public NLB with no cross-agent affinity, so
they can land on different coordinators from each other and from wherever
the CloudWatch lease landed. There's often no single "the coordinator
receiving this customer's data" to stick a tail connection to.

### Direction: centralize via fan-out, not sticky routing — two candidates, benchmark before choosing

Two candidates, Postgres `LISTEN`/`NOTIFY` ruled out (payload-size cap and
throughput-on-a-shared-instance concerns weren't worth it once the other
two were on the table):

1. **Coordinator-to-coordinator gRPC mesh.** Zero new infra dependency —
   but there is currently **zero existing coordinator-to-coordinator
   communication anywhere in this codebase** (checked; coordinators only
   ever talk to shards/compute), so this is new plumbing, not a small
   extension of something already there. Naive version (broadcast every
   ingested row to every peer) has the memory-pressure problem below;
   avoiding it means building our own subscription-interest-propagation
   protocol between peers (announce what a coordinator's local subscribers
   currently care about, maintain a peer interest registry, route only
   matching rows to only interested peers) — real, nontrivial engineering
   this candidate doesn't get for free.
2. **Redis Pub/Sub relay.** New operational dependency — confirmed zero
   existing Redis/ElastiCache footprint anywhere in this repo or
   `infrastructure-catalog`'s weft stack, in code or Terraform, so this
   would be the first place the "stay in Postgres, no new infra"
   posture established by NAT/the leases breaks. Also reintroduces a
   fleet-wide single point of failure for tail specifically (a Redis
   outage breaks tail everywhere, vs. today where a coordinator's own
   local subscribers work regardless of what else fails) — probably an
   acceptable trade given tail is already explicitly best-effort
   (`Broadcast`'s non-blocking-drop semantics, `tail_broker.go:60`,
   mirror Redis Pub/Sub's own fire-and-forget contract exactly), but a
   real one to name. In exchange: topic-scoped delivery (below) comes
   essentially free via native `SUBSCRIBE`/`PSUBSCRIBE`, no
   interest-propagation protocol to build ourselves.

### Memory pressure / broadcast amplification — the reason naive fan-out fails at the one goal that matters

Flagged 2026-09-17: a naive version of *either* candidate (broadcast every
row to every peer / every coordinator subscribes to one global channel)
loses exactly the property this whole plan exists to gain. Per-coordinator
memory/CPU for tail fan-out would scale with **total cluster ingest
volume**, not with how much that coordinator's own clients actually care
about — adding a coordinator to handle more ingest throughput wouldn't
shrink the tail-fanout burden on any existing one, and total fan-out
network traffic would grow linearly with coordinator count even though
tail *demand* (active sessions) hasn't grown at all.

**Fix: topic-scoped delivery, not broadcast-to-everyone.** `RowMatcher`
(`row_matcher.go:15`) already splits cleanly into a structural filter
(`Namespace`/`Service`/`Kind`, set once per subscription) and content
patterns (`Patterns`, grep-style matching on raw row text, can't be mapped
to a routing key). The structural half is exactly a topic key, at the same
granularity `IngestingShard` already routes writes by
(`namespaceID, service`) — nothing new to invent. A coordinator subscribes
only to the (namespace, service[, kind]) topics its own currently-active
local subscribers care about, adding/dropping as clients connect/
disconnect; content-pattern matching stays exactly where it is today,
applied locally by `Match()` after a row lands on an already-interested
coordinator.

This is also what tips the mesh-vs-Redis balance further toward Redis:
`SUBSCRIBE`/`PSUBSCRIBE` gives topic-scoped delivery for free, whereas the
mesh needs the interest-propagation protocol mentioned above specifically
to get this property — that protocol *is* the mesh's price of admission
for not losing the scaling win, not an optional refinement.

**Degenerate case, resolved 2026-09-17: ordinary tail is already naturally
scoped, `warp.log` is the one accepted exception.** `ExecuteFollow`
(`coordinator.go:616`, what powers `tail -f`) only runs for a pipeline
`planner.PlanInDir` can resolve to a real source (`ErrNotPlannable`
otherwise); the `RowMatcher` it builds (`coordinator.go:646`) always
carries `plan.SourceNamespace`/`SourceService`/`SourceKind` — so ordinary
`tail -f /log/<namespace>/<service>` is already topic-scoped today, no
enforcement work needed. The one exception is `warp.log`, a real designed
feature ("the all-sources wildcard," `planner/parse.go:17`), which
deliberately resolves to an empty `Namespace` — "requires a source
argument" is already true and doesn't close this gap, since `warp.log` *is*
a source argument, just the intentionally unscoped one. A coordinator
hosting a `warp.log` tail necessarily subscribes to everything for its
duration; accepted as the cost of an explicit "show me everything" request
(presumably rare, operator-driven), not a gap to design around further.

### Benchmark plan before choosing 1 vs. 2

Build two minimal harnesses (not full integrations) — N simulated
coordinators, each with a real `TailBroker`, wired either via direct
gRPC/channel fan-out or a real `redis-server` (not `miniredis` — the point
is measuring actual hop cost). Both must implement topic-scoped delivery
per above, not naive broadcast, since that's the property actually being
compared.

- **Latency** — stamp synthetic rows with an origin timestamp on the
  publishing coordinator, measure arrival in a subscriber's channel on a
  *different* coordinator. Report p50/p95/p99, not just mean — felt
  "live-ness" of a tail session is dominated by the occasional slow
  delivery. Isolate from same-process `Broadcast` cost (unchanged by either
  design either way).
- **Throughput ceiling** — sustained rows/sec before queueing grows
  unbounded or drop rate crosses a threshold, at a few coordinator counts
  (N=2, 3, maybe 5) — mesh cost is O(N) sends per row from the publisher,
  Redis publish cost is O(1) with fan-out absorbed inside Redis; worth
  seeing where that difference starts to matter in practice, if it does at
  the coordinator counts actually expected here.
- **Backpressure behavior** — weighted heaviest: what each does when it
  can't keep up. If either design's backpressure propagates onto the
  *ingest* hot path (not just tail lagging), that's disqualifying
  regardless of latency/throughput numbers — it'd turn a best-effort
  feature into a risk to the durable write path.

Not yet built. Not yet decided.

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

**Design 2026-09-16.** A single global `"shard-fsm-leader"` lease, same
primitive as NAT/poller/cron, guarding only the *write*/convergence side.
Needs NAT's continuous-retry election loop, not poller/cron's
try-once-at-boot `AcquireAndHold` — a non-leader has to keep retrying
every tick in case the leader dies, same reason NAT couldn't use the
one-shot pattern.

### What gets gated vs. what stays unconditional

`Provisioner.check()` (`provisioner.go:632`) today runs
`RefreshFromStore()` → `syncShardServices()` → `evaluateStateMachines()`
every `PollInterval` (1 min prod, `provisioner.go:121`); `Start()`'s loop
also calls `adoptFromHeartbeats()` before each `check()`. Going through
each:

- **`RefreshFromStore()`** — pure read, writes only local memory. Stays
  unconditional on every coordinator, every tick. This is the whole
  mechanism that makes a separate "leader bounces followers" push
  unnecessary: a follower already re-syncs to whatever the leader last
  wrote within one tick, by polling, not by being pushed to.
- **`syncShardServices()`** (`provisioner.go:703`) — looks like a write
  (stamps `shard_services`) but the code comment says `AddServiceToShard`
  is idempotent, "an already-present membership is a cheap no-op."
  Unconditional — no gating needed.
- **`adoptFromHeartbeats()` / `ProcessHeartbeat`** (`provisioner.go:482`)
  — mixed. For a shard already in steady state it calls `ObserveHeartbeat`,
  which dials-if-missing (an in-process gRPC handle, inherently
  per-coordinator — can't be shared across processes anyway), bumps
  `LastHeartbeatAt`, and persists via `TouchHeartbeat`. Not exclusive,
  stays unconditional — every coordinator needs its own live connections
  and its own accurate local staleness view for query/ingest routing
  regardless of leadership. But for `LAUNCHING`/`SUSPENDED` shards (or an
  unclaimed instance heartbeat) it calls `promoteOnHeartbeat`, which dials
  the shard and **fires an FSM transition** — exclusive, gate this behind
  `isLeader()`.
- **`evaluateStateMachines()`** (bucket + shard `Evaluate`) — launches,
  terminates, EBS attach/detach, persisted state transitions. Fully
  exclusive, leader-only.

So the gating surface is narrow: `evaluateStateMachines()` wholesale, plus
just the `promoteOnHeartbeat` branch inside `ProcessHeartbeat` — not the
whole heartbeat path.

### Why that split avoids a trap: `LastHeartbeatAt` doesn't need a fix

First instinct was that `RefreshFromStore` would need to start pulling
`LastHeartbeatAt` from the DB, since it currently skips that field
deliberately (`registry.go:329`, "the in-memory entry owns more freshly
than the row" — true only when this process is the one processing
heartbeats). Gating *all* of `adoptFromHeartbeats` behind the leader lease
would make that assumption false and freeze every follower's
`LastHeartbeatAt` at boot forever — and `isStale()` (which both
`MatchingEntries` and `IngestingShard`, i.e. read *and* write query
routing, depend on — `registry.go:148`) would eventually reject every
shard on that follower. Scoping the gate to just `promoteOnHeartbeat`
avoids this: every coordinator keeps bumping its own `LastHeartbeatAt`
independently off the same `shard_heartbeats` rows, every tick, leader or
not. `RefreshFromStore` doesn't need to change for this field.

### Real gap #1: `AZ` / `LaunchAttempts` / `StripeIndex` need to leave the skip-list

These three *are* in `RefreshFromStore`'s current skip-list ("FSM-launch
bookkeeping... deliberately NOT refreshed," `registry.go:331`) — but
unlike `LastHeartbeatAt`, nothing keeps them fresh on a process that never
personally wrote them, and the DB is already authoritative for two of the
three:

- `LaunchAttempts` — `IncrementShardLaunchAttempts` (`shard.go:162`) is a
  real atomic DB increment, called *before* the local `sh.LaunchAttempts++`
  (`shardsm.go:481-485`). A new leader that never personally launched this
  shard has a stale/zero local count. Impact is bounded: the retry budget
  (`LaunchRetryBudget`, default 5) becomes "per leader generation" instead
  of lifetime — a flapping leader means a doomed shard takes longer to
  reach `GONE`, nothing worse.
- `AZ` — `SetShardLaunchResult` persists it to DB in the same call that
  sets `sh.AZ = res.AZ` locally (`shardsm.go:496`). Higher severity if
  left unfixed: `AZ` pins a *recovery* relaunch to the surviving EBS
  volume's AZ (EBS volumes are AZ-locked). A new leader driving a
  `SUSPENDED` shard's recovery relaunch with a stale/blank `AZ` risks
  picking the wrong AZ and failing to reattach the existing volume.
- `StripeIndex` — same category, lower-traced risk (bucket-assigned
  stripe position, used for launch-target selection); include it in the
  same fix for consistency since the mechanism is identical.

Fix: pull all three into `RefreshFromStore`. No tradeoff the way there
might be for `LastHeartbeatAt` — DB is never behind memory for any of
these.

### Real gap #2: `IngestFailures` — the ingest-side circuit breaker can go inert

`RecordIngestFailure`/`ResetIngestFailures` (`registry.go:509,522`) are
pure in-memory bumps — no dedicated persist call, unlike the other two
(the DB column only gets written incidentally, whenever some *other*
transition happens to trigger a full-row `Save`). And it's incremented by
*whichever coordinator happens to be routing that shard's ingest writes*
(`shard/shard.go:394`, the write path) — today that's the same process as
the one evaluating `evaluateIngesting`'s circuit breaker
(`sh.IngestFailures >= SuspendFailureThreshold`, `shardsm.go:527`) purely
because there's only one coordinator. Once `evaluateIngesting` is
leader-only, the leader is very likely reading a counter some *other*
coordinator is the one actually incrementing — the `DEGRADED` circuit
breaker could go effectively inert, a silent loss of a safety mechanism
rather than a crash.

This isn't really a #5 bug — it's a #4-shaped gap (per-process ingest-side
state, same category as the still-open tail/ingest buffers) that #5's
leader-gating is what makes consequential. Fix mirrors `TouchHeartbeat`:
persist synchronously on increment/reset, then let the leader's
`RefreshFromStore` pull it. Scope as its own small piece of work alongside
#5, not folded into the same commit. `reconnectFailures` (the other half
of the same `unhealthy` check) is fine to leave local/unsynced — it's
inherently "can *this* process reach the shard," and it's the leader's own
reachability that should matter for the leader's own decision.

### Launch-race and leadership-handoff safety already built in

- **Double-launch is already defused, independent of the lease.**
  `EC2Provisioner.Launch` (`provisioner_ec2.go:207`) checks
  `findLiveByShard` (`DescribeInstances` filtered on `tag:shard_id` +
  `pending|running`) before calling `RunInstances`, adopting an in-flight
  instance instead of minting a twin — "No ClientToken: idempotency comes
  from findLiveByShard." Even a stalled leader (GC pause / frozen VM) that
  resumes after its lease expired and a new leader has started launching
  can't produce two real EC2 instances for the same shard. Narrow TOCTOU
  sliver between the `DescribeInstances` check and the `RunInstances`
  call remains, but that's the same residual risk any lease system has —
  not specific to #5.
- **A freshly-elected leader's SM bootstraps correctly.** `ensureSM`
  builds each `stateless.StateMachine` with
  `NewStateMachineWithExternalStorage(sh.readState, sh.writeState, ...)`
  — it never caches "current state" at construction, it re-reads on every
  `Fire`/`Evaluate` call. Combined with `check()` always running
  `RefreshFromStore()` first, a new leader has already pulled the latest
  DB state in the same tick before touching `evaluateStateMachines()`. Per
  the `Evaluate` doc comment: "a restart... re-drives any half-finished
  work — there is no boot-vs-running mode." A leadership handoff is
  structurally the same event as a restart from the SM's point of view.
- **Archiving's checkpoint fields are already synced.**
  `evaluateArchiving` (`shardsm.go:619`) gates each of its three steps
  (export → S3, retire volume, terminate instance) on a persisted field —
  `ParquetPath`, `EBSVolumeID`, `InstanceID` — and all three (unlike
  `AZ`/`LaunchAttempts`) are already in `RefreshFromStore`'s synced list.
  A leader handoff mid-archive is exactly equivalent to a process restart
  mid-archive, which this code already survives by design. No fix needed.
  Per-step behavior if a zombie leader resumes mid-step: export's S3 key
  is deterministic (`s3Prefix/shard-id`, `shard.go:573`, no timestamp), so
  a redundant concurrent export overwrites rather than orphans an object
  — though whether the shard's own gRPC `Archive` handler tolerates two
  concurrent export calls against the same DuckDB file is untraced, worth
  a quick check before implementing; volume delete and instance terminate
  are both already idempotent/error-tolerant.
- **Buckets have no equivalent gap.** `BucketManager` deliberately caches
  nothing between calls (`app_buckets.go:26`) — every method does a fresh
  DB read/write. No skip-list-style staleness risk on the bucket side.

### Real gap #3 — the one that matters most: liveness ≠ progress

The lease's renewal loop runs on its own independent goroutine/ticker,
decoupled from `check()`'s tick loop — deliberate, and correct for
NAT/poller/cron. But it means a coordinator that's alive enough to make a
cheap `AcquireLease` call, yet genuinely stuck or deadlocked inside one
`Evaluate()` call, keeps renewing `"shard-fsm-leader"` forever — no other
coordinator can ever take over, even though zero convergence work is
happening. The lease only proves "this process can still reach Postgres,"
not "this process is making progress." Known failure mode of lease-based
single-leader systems generally, not a bug specific to this design — but
worth naming rather than discovering in an incident.

Mitigation isn't in the lease — it's making sure nothing reachable from
`evaluateStateMachines()` can block unboundedly. `Archive`'s export and
`retireVolume` are already bounded (`archiveTimeout = 10 * time.Minute`,
`shardsm.go:24`, via `context.WithTimeout`). `Terminate` (archiving's step
3) is called with the bare `ctx` passed down from the tick, tracing back
to the provisioner's long-lived top-level context — no per-call deadline
found, meaning it currently relies entirely on the AWS SDK's own default
HTTP timeout, if any. **Audit every blocking call reachable from
`evaluateStateMachines()` for a bounded context as a named prerequisite
for implementing #5** — without it, the lease can't actually deliver the
failover-time bound it's supposed to.

**Concrete mitigation: progress-gated renewal (a deadman's switch), not
just a bounded-context audit.** Lease-based leader-election systems
generally solve liveness-≠-progress one of three ways:

1. **Tie renewal to proof of progress, not just DB reachability** — the
   pattern to adopt here. Kubernetes' `client-go` leader-election package
   is the widely-used reference: it requires a controller to actually stop
   its reconcile loops the instant it observes the context passed to
   `OnStartedLeading` get canceled, and the standard convention layered on
   top is to also call `os.Exit(1)` inside `OnStoppedLeading` — a
   deliberate "if you're still running after being told to stop, just
   die," precisely because a goroutine that ignores cancellation and keeps
   running is this exact failure mode. For us: the tick loop stamps a
   `lastProgressAt` (an `atomic.Value`/`atomic.Int64` unix-nanos) after
   *each* shard's `Evaluate()` call, not just once per full tick — so one
   stuck shard doesn't mask the others still completing — and the
   elector's renewal goroutine refuses to renew (or actively calls
   `ReleaseLease` and stops) once `time.Since(lastProgressAt)` exceeds
   something like 2× `PollInterval`. This is the recommended fix, layered
   on top of (not instead of) the bounded-context audit above — the audit
   shrinks how often this would ever need to trigger; the deadman's switch
   is the backstop for whatever the audit misses (a genuine deadlock has
   no context to time out).
2. **Fencing tokens** — Chubby, ZooKeeper (`zxid`), etcd (revision
   numbers), and Consul (`ModifyIndex`) attach a monotonically increasing
   token to each lease acquisition and have the guarded resource reject
   any write carrying an older token than the newest it's seen, so a
   resumed zombie leader's writes are no-ops by construction rather than
   by the operation happening to be idempotent. We're already getting an
   informal version of this for free: `findLiveByShard`'s AWS-tag lookup
   and `Terminate`'s "already gone is success" both make the *specific*
   AWS operation safe against a duplicate actor, with AWS's own state as
   the source of truth. Not worth building a formal token layer unless
   implementation turns up a write path that isn't naturally idempotent
   the way the launch/terminate paths are (a plain `PersistState` full-row
   `Save` doesn't need one — two writers racing it is just last-write-wins
   against Postgres, which is already the single source of truth being
   overwritten, not a separate uncoordinated system).
3. **Externalize the health signal** — Consul's session-plus-health-check
   model invalidates a session (and anything locked under it) when an
   attached health check fails, independent of whether the process itself
   ever calls renew again — useful specifically because a truly deadlocked
   process can't run its own watchdog code either. Heavier than (1) for
   what's likely a rare failure mode; treat as the fallback if (1) proves
   insufficient in practice, not a day-one requirement. (Our coordinator
   already exposes `--health-port 9021`, so the plumbing would be "an
   external prober empowered to `DELETE FROM leases`," not a new health
   endpoint.)

Separately, but related: `evaluateStateMachines` runs shard `Evaluate`
calls in a single sequential loop (`provisioner.go:664`), not
per-shard goroutines, so one shard's slow archive already blocks every
other shard's convergence for that tick today, with one coordinator.
#5 doesn't worsen this (a design where multiple coordinators raced to
cover for a "stuck" one would just reintroduce the double-launch/
double-transition hazard #5 exists to prevent) — but it does mean the
bounded-context audit above isn't optional polish, it's what keeps a
single slow shard from becoming a cluster-wide convergence stall.

### Implemented 2026-09-17

Built as designed above, in this order:

- **Real gap #1** (`logdb/registry.go` `RefreshFromStore`) — `AZ`,
  `StripeIndex`, `LaunchAttempts` moved out of the permanent skip-list; the
  DB is never behind memory for these (both writers persist before
  mutating locally), so there's no `LastHeartbeatAt`-style tradeoff.
  `LastHeartbeatAt` itself stays excluded, unchanged, per the "why that
  split avoids a trap" reasoning above. Covered by
  `TestRefreshFromStore_SyncsLaunchBookkeeping`
  (`logdb/leader_gaps_test.go`), which simulates a leadership handoff:
  one write path persists launch bookkeeping the way `runLaunch` does,
  a second independent registry (never touched the shard before) picks
  it up via `RefreshFromStore`.
- **Real gap #2** (`logdb/registry.go` `RecordIngestFailure`/
  `ResetIngestFailures`, `logdb/shard.go` `SetShardIngestFailures`) — both
  now persist synchronously, mirroring `TouchHeartbeat`, and
  `IngestFailures` was pulled into `RefreshFromStore` alongside gap #1's
  fields. `ResetIngestFailures` only writes through when the counter
  actually had something to clear (it runs on every successful ingest
  batch — the hot path — so persisting an already-zero counter on every
  call would be pure write amplification). Covered by
  `TestRecordIngestFailure_PersistsAcrossCoordinators` and
  `TestResetIngestFailures_SkipsWriteWhenAlreadyZero`
  (`logdb/leader_gaps_test.go`).
- **Real gap #3, bounded-context half** (`logdb/shardsm.go`
  `evaluateArchiving`, `evaluateRecovery`) — both `Launcher.Terminate`
  calls (archive-time and recovery-reap) now run under
  `context.WithTimeout(ctx, archiveTimeout)`, matching `retireVolume`'s
  existing pattern, instead of the bare tick `ctx` that had no deadline of
  its own. Scoped to the two calls the design doc actually traced as
  unbounded (`Store`/gorm calls elsewhere in the sync path weren't
  audited — a DB-level `statement_timeout` is a separate, broader gap not
  raised by this design).
- **Real gap #3, deadman's switch half** (`logdb/leader_election.go`
  `leaderTick`/`progressStale`) — `Provisioner.lastProgress` is stamped
  once per `evaluateStateMachines` tick (via `defer`, so an idle tick with
  nothing to converge still counts) and again after each individual
  bucket/shard `Evaluate` call. The election loop refuses to renew — and
  proactively releases — the lease once `time.Since(lastProgress) >
  progressStaleFactor(2) * PollInterval`. Covered by
  `TestLeaderTick_ProgressStaleReleasesLease`
  (`logdb/leader_election_test.go`), which also confirms a challenger can
  win the lease immediately after (the DB row is actually released, not
  just the local flag).
- **The lease + gating itself** (`logdb/leader_election.go`,
  `logdb/provisioner.go` `check`/`promoteOnHeartbeat`) —
  `shard-fsm-leader`, continuous-retry election loop matching
  `coordinator/nat.go`'s `NATElector` shape (not poller/cron's
  try-once-at-boot `AcquireAndHold`). Gates exactly the surface identified
  above: `evaluateStateMachines()` wholesale (from `check`) and the
  `promoteOnHeartbeat` branch inside `ProcessHeartbeat` — `RefreshFromStore`,
  `syncShardServices`, and `ObserveHeartbeat` stay unconditional on every
  coordinator, every tick, as designed. `Provisioner.leading` defaults to
  `true` in `NewProvisioner` and `startLeaderElection` no-ops when
  `Holder`/`Store` aren't configured, so every existing test that drives
  `check()`/`evaluateStateMachines()` directly (never through `Start`)
  keeps behaving exactly as before — single-process, always leader.
  `cmd/coordinator/main.go` threads the same `coordinatorHolder()` id
  already used for the NAT/poller/cron leases into `logdb.Config.Holder`.
  Covered by `logdb/leader_election_test.go` (lease acquire/renew/failover,
  mirroring `coordinator/nat_test.go`'s pattern) and
  `TestCheck_EvaluateStateMachinesGatedBehindLeader`/
  `TestProcessHeartbeat_PromoteGatedBehindLeader`
  (`logdb/leader_election_test.go`).

Not covered by this pass: a full audit of every `Store`/gorm call
reachable from `evaluateStateMachines` for a bounded context (only the
`Launcher.Terminate` calls the design doc actually traced were fixed), and
option 3 (externalized health-check-based lease invalidation) from the
liveness-≠-progress discussion above — the deadman's switch (option 1) is
what shipped; fall back to (3) only if (1) proves insufficient in
practice.

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
