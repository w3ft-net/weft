# True cost of operating weft, and re-pricing the plans

## Why now

The last pricing exercise predates three large cost changes:

- **More load balancers.** `asg-weft-application` now defines four `aws_lb`s:
  the public coordinator NLB, the VPC-internal coordinator LB (heartbeat
  listener included), the internal compute NLB, and the API ALB. Each carries a
  fixed hourly charge before any traffic.
- **Redundancy.** `coordinator_desired = 2` (horizontal scaling went live
  2026-09-19), a seamless-upgrade warm spare pool for shards, and a NAT private
  subnet.
- **Cold shards.** Lambda cold shards work, so archived data no longer needs an
  instance. The marginal cost of old data is now S3 plus Lambda per query, not
  EBS plus EC2. That changes what a long retention window costs us.

The weft AWS account (`220664822695`) also hosts gravitty and vimgym, so
account-level spend is not weft's cost; every number below has to be
attributed to weft by tag or resource first (Phase 1).

No cost model exists in the repo. The only pricing artifacts are
`frontend/src/components/PricingTable.tsx`, the tier functions in
`store/user.go`, and `docs/completed/stripe.md`.

## What the plans promise (decided 2026-11-08)

There are four tiers in code (`store.Tier`): `trial`, `swatch`, `yard`,
`bolt`. Until the relaunch below, public self-serve signup was hidden (the
landing page sent visitors to a demo booking link) and every account was an
operator comp (`GrantCompSubscription`, `compTiers` = swatch/yard/bolt, comp is
behaviourally identical to paid). Separately, shards have *instance* tiers
(small/medium/large, a cost axis, not a plan); medium and large start at 0 in
prod.

The posted prices were $19 / $79 / $299. They are now **$29 / $99 / contact
us**, the numbers in `scripts/costmodel/tiers.yaml`. Bolt has no public price:
it is quoted per customer on dedicated shards, so the landing page, signup and
the account page send visitors to the booking link instead of checkout, and
`POST /api/auth/signup` refuses it. The frontend carries the plans in one
module (`frontend/src/lib/plans.ts`).

| | Swatch | Yard | Bolt |
|---|---|---|---|
| Price | $29/mo | $99/mo | quoted |
| Ingest included | 10 GB | 100 GB | 500 GB |
| Overage | $2.00/GB | $1.00/GB | quoted |
| Retention | 30 d | 90 d | 180 d |
| Seats | 5 | 10 | 50 |
| Crons / min interval | 20 / 5 m | 100 / 1 m | 500 / 1 m |
| Tags per cron run | 50 | 100 | 250 |
| /tmp storage | 10 MB | 100 MB | 1 GB |

Trial: a new signup no longer holds the `trial` tier (1 GB, 24 h retention,
1 seat, 5 m cron interval). It picks Swatch or Yard, enters a card, and gets
that plan in full for 14 days, with nothing charged until the trial ends (see
"Self-serve relaunch"). The `trial` tier remains for accounts created before
that change.

**Inconsistency to resolve first.** `docs/completed/stripe.md` says Bolt is
1 TB included at $0.50/GB, and records Stripe price IDs with a
1,000,000,000 KB free quota and a $0.0000005/KB rate. The code and pricing page
say 500 GB at $0.75/GB. Either the doc is stale or the live Stripe prices don't
match what we advertise. Check live Stripe before anything else.

**Retention is enforced on disk, not just at query time.** The retention
janitor is live in prod (`janitor_enabled = true`): the coordinator invokes the
compactor Lambda to delete or rewrite archived parquet at
`now - LogRetention(tier) - 7 days` (`logdb/janitor.go`). So stored data is
roughly retention + 7 days, and the janitor itself is a cost line (compactor
Lambda + S3 reads/writes). What can still hold data longer: customers with no
active subscription (deliberately never deleted), shards archived before stats
were captured (rewriter must scan), and Postgres-side tables (`cron_results`,
etc.), which I have not yet verified are swept. `retention-janitor.md` also
lists Bolt at 365 days while code and the pricing page say 180; stale doc.

## Phase 0: decisions (settled with Paul)

- **Target gross margin: 30 %** per tier, treated as the minimum at typical
  (not best-case) usage. The model reports the price that hits 30 %.
  *Superseded 2026-11-08:* the targets are now per tier in `tiers.yaml`
  (95 % / 87.5 % / 80 %), and Paul accepted the modeled margins at the launch
  prices ($29 and $99: about 92 % and 80 to 82 % at 100 % use, above this 30 %
  floor but below those targets). The 30 % arithmetic elsewhere in this
  document is kept as written.
- **COGS scope:** AWS spend attributable to weft, Stripe fees, Mailgun, any
  paid third party. Excludes engineering time and graybeardcorp overhead.
- **Shared infrastructure is amortized across all tenants of it** (weft,
  gravitty, vimgym for the shared RDS, VPC, NAT, etc.), not charged wholly to
  weft. Where a usage proxy exists (DB size, bytes through NAT, connections)
  use it; where it does not, split equally and flag the line.
- **Enterprise / SSO / BYOK / residency tier: out of scope** for this pass.
- **Dogfood representativeness:** undecided, so Phase 2 analyzes it and ends
  with a recommendation (use as-is, scale, or synthetic profile).

## Phase 1: attribute spend to weft, then split it by how it scales

### 1a. Attribution (the account is shared)

Pull the last 3 full months from Cost Explorer for account `220664822695`
via `kevcons aws` (bare `aws` lands in the kevlar account). Group by SERVICE,
USAGE_TYPE and the `Application` / `Stack` tags. Then classify every line:

- **weft-owned**: tagged `Application=weft`, or a resource the weft stack
  creates (`asg-weft-application`: its 4 LBs, ASGs, Lambdas, Firehose, S3
  buckets, CloudFront, shard EBS).
- **other-app-owned**: gravitty / vimgym stacks. Excluded.
- **shared**: VPC/NAT, `weft-postgres` RDS, public IPv4s, Route 53, anything
  untagged. Amortized per Phase 0, with the allocation key written next to
  each line.
- **unattributed**: reported as its own number so the model can show how
  sensitive it is to it. Waste (orphaned volumes, ghost instances) is listed
  separately from steady-state cost; see `docs/known-issues.md`.

### 1b. Three classes of cost, not "fixed vs variable"

Treat infrastructure as a step function. For each component record the
current size, the trigger that grows it, and the cost of the next step.

| Class | Behaviour | Components today |
|---|---|---|
| **Floor** | exists at zero customers | one NAT, DNS, CloudFront, ECR/CodeArtifact, S3 buckets, minimum RDS share |
| **Step** | grows in stages as load grows | coordinators (2 now), compute pool (1-10), shard fleet + spares, RDS size, LBs |
| **Variable** | proportional to usage | ingest bytes, EBS GB-months, S3 GB, Lambda GB-seconds, NAT/egress GB, Stripe |

The LBs are the important case. Today they are floor cost, but at scale the
coordinator and API would each get their own ASG and ALB/NLB, the compute pool
scales out, and RDS moves to a dedicated, larger instance. So the model
needs a **scale-out topology per customer-count milestone** (e.g. today, ~10,
~50, ~200 paying orgs): for each, the instance counts and types, LB count,
RDS class, compute pool size, and warm-spare depth, each priced. That topology
is an input I will draft from the stack variables and
`compute-scaling.md` / `coordinator horizontal scaling` notes and then ask
Paul to correct, since the future shape is a judgement call.

Outputs of Phase 1: (1) attributed monthly cost table with evidence
(`component | class | $/mo | scales with | allocation | CE line`); (2) today's
floor; (3) step thresholds and the cost of each step; (4) a break-even
customer count per tier at each milestone.

It also answers which of the recent additions (second coordinator, warm
spares, extra LBs) are earning their cost at current scale.

### 1c. Database cells (first priority, driven by blast radius; not built)

One `weft-postgres` (db.t4g.micro, 1 GB RAM, 20 GB gp3) is shared with vimgym.
Its first limit is memory and write throughput, not disk (disk grows at
$0.115/GB-mo; the notes on the instance already show swap climbing after
restarts). The scale-out step is **database cells**: more small Postgres
instances, with new orgs routed to the newest one. Nothing here is built, and
at today's usage (~1,000x below the worst-case cron load) load alone would not
justify it. The reason to do it first is blast radius (section 1d): it removes
a failure that is already known to take the whole product down. The model
prices it as a step and names its load trigger for later cells.

**There is no surviving "control database"** (Paul, 2026-10-29). The cut is a
seam: whatever can be partitioned by org lives in that org's Postgres cell, and
needs no further discussion of where it goes; only what is truly cross-org goes
to DynamoDB. The existing `weft-postgres` becomes cell 0 once the global tables
have been moved out of it. Existing orgs stay where they are; the global tables
(users, directory, shard fleet, leases) do have to be migrated into DynamoDB.

#### The seam, table by table (from `store/schema.sql`)

**Cell-resident (org-partitionable):** `organizations` (the row; name,
display name, stripe ids, seat count), `subscriptions`, `stripe_usage_reports`,
`memberships`, `org_permissions`, `namespaces`, `namespace_permissions`,
`agent_tokens`, `log_groups`, `log_group_agents`, `cron_watches`,
`cron_watch_history`, `cron_claims`, `cron_results` and its 3 rollup tables,
`alert_incidents`, `alert_incident_deliveries`, `notify_configs` (+history),
`pagerduty_integrations`, `webhook_secrets`, `dashboards` (+history),
`shell_functions` (+history), `shared_links`, `tmp_files`, `tmp_file_lines`,
`usage_hourly`, `usage_daily`, `shard_archive_stats` (follows the janitor, which
is per org), and `shard_services` (see below). Cron execution, alerting and
token handling are therefore cell-local, with cell-scoped scheduling.

**DynamoDB (cross-org):**
- *Org directory and indexes:* org name -> org id and cell (also enforces global
  org-name uniqueness with a conditional put), stripe customer id -> org and
  cell, and an org list with tier and status for admin views.
- *Identity:* `users`, `magic_links`, `device_codes`, `user_notices`, plus a
  derived user -> orgs index for login and the org picker.
- *Shard fleet and coordination:* `buckets`, `shard_records`,
  `shard_heartbeats`, `shard_operations`, `shard_spares`, `parquet_tombstones`,
  `ingestion_errors`, and `leases`. (Paul named buckets, records, heartbeats and
  spares; operations, tombstones, errors and leases are my inference; confirm.)

**`shard_services` is on the seam.** It is `(shard_id, namespace_id, service)`
with an index on `(namespace_id, service)`, and `namespace_id` leads to
`namespaces.org_id`, so the forward lookup the read router uses ("which shards
hold this org's service") is org-partitionable and goes in the cell. The
reverse direction does not: the registry loads every row grouped by shard at
boot and refresh (`ListShardServices`, `logdb/registry.go`), and
`reconstruct_shard.go` rebuilds a shard's membership from it. That needs either
a fan-out over cells or a DynamoDB copy keyed by shard. Open.

#### Decided

- *Membership:* the cell is authoritative; DynamoDB holds a derived user -> orgs
  index (used only at login and for the picker) with a repair job. A stale index
  shows a missing org until repaired; per-request authorization stays in the cell.
- *Cross-org reads* (Stripe webhooks, admin listings, janitor, usage reports):
  a few small DynamoDB indexes, with heavy jobs fanning out to cells.
- *Leases:* Paul's call was to move them to DynamoDB; revised on 2026-10-29
  after reading `logdb/fence.go`, **proposed, not confirmed:** keep a lease in
  the same store as the data it guards. The fleet lease (`shard-fsm-leader`)
  lives in DynamoDB with the fleet; cell work leases (cron scheduling, rollups,
  janitor) stay in the cell's Postgres next to `cron_claims`, where a guarded
  write can verify the lease inside one SQL transaction.
- *Session:* identity from DynamoDB, membership, permissions and tier from the
  cell, per request, with caching. User JWTs carry no cell (they carry only the
  user id); user requests route by `X-Active-Org` through the cached directory,
  and only agent/ingest tokens carry a cell hint.

#### Leases and hotspotting on DynamoDB

Question from Paul: does moving leases and the fleet registry to DynamoDB need a
rearchitecture to avoid hot partitions? Read from `store/leases.go` and
`logdb/start.go`:

- *The lease itself needs no transaction; the guarded write does.* A lease is a
  single-item compare-and-swap, atomic on DynamoDB. Today's fencing is a
  check-then-act (the `leading()` callback in `logdb/fence.go` before each
  external call) with a documented window where an in-flight call still lands,
  so it is not atomic on Postgres either. On DynamoDB a multi-row FSM
  transition can include a `ConditionCheck` on the lease item inside one
  `TransactWriteItems`, which makes fleet row writes properly fenced (an
  improvement). EC2 calls cannot be fenced by any store; they need idempotency
  tokens and EC2-side conditions.
- *Lease items are not a hotspot.* A lease is one conditional upsert today
  (`WHERE holder = me OR expires_at < now`), which maps directly to a DynamoDB
  condition expression. There are a handful of named leases, each its own key,
  renewed every few seconds: far below a partition's limit. Use one item per
  lease name, and scope names (`fleet`, `cell/<id>/cron`, `cell/<id>/janitor`,
  ...) so org work partitions by cell instead of one scheduler scanning every
  cell. (My reading of Paul's "leases in dynamo, avoid hotspotting"; confirm.)
- *Do not rely on DynamoDB TTL for expiry.* TTL deletes lazily (can lag by a day
  or more); keep `expires_at` in the condition and use TTL only for cleanup.
  Keep a fencing counter on each lease so a stalled holder cannot act
  (`docs/failure-modes.md` already requires lease-guarded EC2 and shard calls).
- *Heartbeats* arrive about every 10 seconds per shard
  (`HeartbeatInterval` in `logdb/start.go`): roughly 0.1 write/s per shard,
  trivially cheap. Keep them on their own item or table, keyed by shard id.
  The hazard is a GSI whose partition key has low cardinality (for example
  `state = INGESTING`) while heartbeat-updated attributes are indexed: every
  heartbeat then writes the same GSI partition. Index only attributes that
  change on transitions.
- *Registry reads:* coordinators must not `Scan` the fleet on a timer. Query by
  bucket, cache in memory, and refresh by polling changed items or a stream.
- *Consistency:* leases and state-machine reads must be strongly consistent;
  GSI reads are only eventually consistent, so never read FSM state through a GSI.
- *Multi-row FSM transitions* become `TransactWriteItems` (max 100 items and
  4 MB per transaction), conditional on a per-record version counter, with retry
  on contention aborts. This is the real redesign work in the registry rewrite.
- *Cost at current scale:* about 10 shards x 8,640 heartbeats a day is under
  100 k writes a day on on-demand capacity: cents to a few dollars a month.
  Check current DynamoDB pricing before putting a number in the model.

#### Security rules for the cell hint (design constraints, not built)

The hint is for routing only and never for authorization; membership and tenant
scope are re-checked in the cell the request lands in (the existing
`store/tenant.go` org_id scoping stays as the backstop), and a wrong-cell
request fails closed, with no fallback to another cell. Verify an agent token's
integrity (an HMAC over cell and public id) before touching a cell's database,
so forged tokens cannot aim unauthenticated lookups at a chosen cell. Cells must
not be able to mint credentials: no signing secret shared with cells, or a
compromised cell could forge access to every other cell; use asymmetric signing
with cells holding verify keys only (whether the current JWT secret is a shared
symmetric key is to be confirmed). Cell ids in tokens are opaque labels, never
hostnames, DSNs or instance ids. The directory is the truth and the hint a
cache: a moved org must be re-routable without waiting out token lifetimes (user
JWTs last 30 days), so keep org moves rare and have the old cell forward or
refuse cleanly.

#### Costs and mechanics

- *Why the cell side is cheap to build:* no app SQL joins `cron_results` to
  `cron_watches`, and the rollup procedures touch only the results tables.
- *What it touches:* `Store` holds one `*gorm.DB`; `store/tenant.go` is the
  natural routing hook. Each cell needs the goose migrations, `pg_cron`, partman
  and the rollup procedures (reconstruction currently pins `pg_cron` to the
  `weft` database). Connections multiply by coordinators x cells, so pgbouncer
  matters. A signup becomes a multi-store sequence (reserve the org name in the
  directory, create the org in a cell, write identity and the index), so each
  step must be idempotent and repairable.
- *Trigger for adding a cell after the first:* not "half full on disk"; use
  freeable memory, swap, and write IOPS / CPU credits on the busiest cell.
- *Cost:* about $15/mo per single-AZ micro cell (instance ~$11.70, 20 GB storage
  ~$2.30, backup ~$1), about $29 with Multi-AZ; a small is unpriced. Cells
  needed = ceil(orgs x per-org write load / cell capacity). Per-org load is small
  for ordinary use (graybeard ~12 k rows/day) and large only at plan limits
  (Bolt: 500 crons at 1-minute is ~720 k rows/day). Cell capacity needs a
  write-throughput benchmark on a scratch Postgres; not yet measured.

### 1d. Blast radius and SLA exposure

Expected SLA credit is roughly the sum over failure modes of
`P(failure) x outage duration x share of customers affected x credit rate`.
Three levers act on different factors, and they are not substitutes:

| Lever | Cuts | Cost |
|---|---|---|
| Multi-AZ RDS | outage **duration** of a host loss (standby failover, typically a minute or two, against restore or host replacement) | roughly doubles that instance (about +$12/mo for a micro, ~$24 total); today's instance is single-AZ |
| Cells (org-complete) | **share** of customers hit by one database's loss | about $15/mo per micro cell, plus engineering |
| Directory outside Postgres / cell claim in tokens | removes the **shared** routing dependency | DynamoDB on-demand, pennies |

Findings and gaps:

- `weft-postgres` is **single-AZ and shared with vimgym**. Losing its host takes
  every weft customer's auth, crons and usage with it, and today there is no
  documented behaviour for it in `docs/failure-modes.md` or
  `az-resilience.md` (those cover shards, not Postgres).
- *(Decided 2026-11-08: no SLA and no credits at self-serve launch; see
  `service-commitments.md`. The expected-credit formula above is therefore zero
  for now, and the paragraph below is the reasoning that led there.)*
  No SLA text exists on the pricing page or in the manual, so the credit rate
  and uptime target are not in this plan's inputs yet. Paul's position: undecided;
  it depends on what we can be reasonably certain of providing by end of year,
  so set it after the decided order below has landed, not before.
- What survives a control-database outage today is untested. Coordinators hold
  an in-memory shard registry loaded at boot, and JWTs are stateless, so some
  existing sessions and ingest may keep working; per-request lookups of
  users, memberships, tokens and tier may not. This needs a chaos test
  (`coordinator-chaos-tests.md` is the place): stop the database, record what
  still works and for how long.
- The shard fleet (`shard_records` etc.) is also global and on the data path.
  A cell split does not remove that dependency; the answer is the shard
  registry rewrite onto DynamoDB in the decided order below.

**Decided order (Paul, 2026-10-29).** Limit the blast radius of what is already
known to take the product down, then measure, with the cheap availability lever
held in reserve:

1. **The seam migration** (1c; rollout plan in
   [`cells-dynamo-rollout.md`](cells-dynamo-rollout.md)): org-complete Postgres cells, with the org
   directory, identity and indexes in DynamoDB; `weft-postgres` becomes cell 0
   and no global tables remain in it.
2. **Shard fleet registry and leases onto DynamoDB** (1c has the table list,
   hotspotting analysis and the open `shard_services` reverse-lookup question).
   Needs a design pass over the shard FSM's use of these tables
   (`logdb/states.go`), especially multi-row transitions.
3. **Chaos tests**, run after 1-2 (`coordinator-chaos-tests.md`): lose one cell's
   database and confirm only its orgs fail; throttle DynamoDB and see what
   degrades; learn what the user path does with a cold cache.
4. **Multi-AZ on `weft-postgres`: deferred, runnable at any time** (below).
5. **Moving vimgym off weft's instance: postponed indefinitely.**

**Open for the design pass:** the `shard_services` reverse lookup (fan-out or
DynamoDB copy); confirm the inferred fleet tables and scoped leases; behaviour
when cached identity expires and DynamoDB is unreachable (serve stale for a
bounded time, fail closed, or never deny); what must keep working during a
DynamoDB or single-cell outage; and whether coordinators stay shared across cells.

#### Deferred, ready to run: Multi-AZ on `weft-postgres`

Live state 2026-10-29: single-AZ db.t4g.micro, PostgreSQL 16.13, maintenance
window Sun 04:00-05:00 UTC, 7-day backups, no pending modifications. AWS
behaviour below is from memory and should be checked against current docs
before a prod change.

- **Conversion downtime: none expected.** RDS snapshots the primary, builds a
  standby in another AZ, then syncs; the primary stays up and the endpoint DNS
  is unchanged. Expect higher I/O and latency for tens of minutes (20 GB) and
  a possible brief I/O stall; the instance is already memory-starved, so run
  it at low traffic and watch swap and latency. Afterward each commit waits on
  the standby (a few ms).
- **Terraform gotcha.** The stack owns the instance
  (`asg-weft-application`, `modules/rds`, `multi_az` defaults false) and
  hard-codes `apply_immediately = var.environment != "prod"`, which is false in
  prod. Setting `multi_az = true` queues the change for the next Sunday window.
  To pick the time: flip `apply_immediately` for that one apply, or run
  `kevcons aws rds modify-db-instance --multi-az --apply-immediately` and then
  reconcile Terraform. Review the plan first (the module's
  `final_snapshot_identifier = timestamp()` shows as a perpetual diff; a dead
  IAM principal has forced `-target` before).
- **Buys:** automatic failover on host or AZ loss (typically 1-2 minutes) and
  shorter maintenance outages. **Does not buy:** relief from the 1 GB memory
  ceiling, protection from bad queries or corruption, or a readable standby.
- **Cost:** about +$14/mo (instance +$11.70, storage +$2.30).
- **Before relying on it:** force a failover once
  (`reboot-db-instance --force-failover`) in a window, to see whether the
  coordinators and pgbouncer reconnect. Not known today.
- **Interaction with cells:** each cell is a separate instance, so each would
  want its own Multi-AZ if its loss must be survivable.

## Phase 1 findings (2026-10-29, first pass)

Sources: Cost Explorer Jul-Sep 2026 for account `220664822695` via
`kevcons aws`, plus a live inventory (`describe-load-balancers`,
`describe-instances`, `describe-volumes`, `describe-db-instances`). Cost
allocation tags are **not activated**, so every CE line is untagged;
attribution below is by resource inventory, not by tag.

**Whole account: $184 (Jul), $184 (Aug), $231 (Sep).** Largest lines:
Load balancers $66-68, EBS gp3 $37-40, t4g.micro $26-27, public IPv4 $15 -> $27,
RDS $15, Secrets Manager $5 -> $10 (13 -> 25 secrets), snapshots $6-8 (waste).
Smaller: t4g.nano $9-12, a one-month NAT ($5), ElastiCache $4 (one month),
t4g.large $9.5 (one month, probably an export or test).

**What it says about the platform**

- **Almost nothing is variable yet.** S3 is $0.10-0.25/mo; Lambda, Firehose,
  Kinesis and CloudWatch do not appear. Cold shards and the janitor are
  effectively free at current volume, so per-GB variable costs cannot be
  measured from the bill. They must come from list prices and a benchmark of
  what a shard sustains.
- **The 4 LBs are the single biggest line** (~$66/mo, about 30 % of the
  account, ~$16.4 each). Adding the dedicated coordinator/API LBs planned for
  scale-out adds ~$16 each; a doubling of LB count is worth ~$66/mo.
- **Public IPv4 doubled in September** (4 -> ~7.5 addresses in use) when the
  second coordinator went live; October is the first full month at that level.
- **Secrets Manager doubled** (13 -> 25). Find out what creates them
  (per-shard? per-coordinator?) before it scales with fleet size.
- **EBS snapshots still bill ~$6/mo** despite the 2026-09-15 removal of the
  per-archival snapshot; leftover waste, not steady state.
- **September under-states the October run-rate**: 7 t4g.micro are running now
  (4 live shards, 2 warm spares, pgbouncer) against ~4.4 average in September.

**Weft-attributed steady-state run-rate** (list prices, 730 h; shared items
shown in full, to be amortized per Phase 0):

| Line | $/mo | Class |
|---|---|---|
| 4 load balancers | 65.7 | floor now, step at scale-out |
| Public IPv4 (~7.5) | 27.4 | step (shared w/ pgbouncer) |
| Coordinators 2x nano | 6.1 | step |
| Compute 1x nano | 3.1 | step |
| loom-producer nano | 3.1 | floor |
| Shards 4 live + 2 spares (t4g.micro) | 36.8 | step |
| Shard data volumes 4x100 GB gp3 | 32.0 | step (1 per shard) |
| Root volumes (~11x8 GB) | 7.0 | step |
| RDS micro + storage + backup (shared w/ vimgym) | 15.2 | shared step |
| pgbouncer t4g.micro (shared?) | 6.1 | shared |
| Secrets Manager (25) | 10.0 | step? |
| Snapshots (waste) | 6.0 | waste |
| S3 / Lambda / Firehose / CW | 0.5 | variable |
| **Total** | **~219** | |

Not weft, excluded: `vimgym-app` (nano), `gravitty-gravittyd` (small),
ElastiCache (unassigned; confirm owner).

Of ~$219, the 2 warm spares are ~$12 (6 %) and a second coordinator is
~$3 of instance plus ~$3.65 IPv4; the LBs and IPs are what the "more
architecture" actually cost.

**Implications for pricing**

- The platform is floor-dominated: ~$200/mo with no paying customers. Break-even
  at a 30 % margin is roughly 17 Swatch, 4 Yard or 2 Bolt customers at the
  old $19 / $79 / $299 prices, and about 11 Swatch or 4 Yard at $29 / $99
  ($219 / 0.7 = ~$313 revenue) at the floor alone (Phase 3 refines this with per-customer
  variable cost).
- The real per-GB cost is **shard capacity**, not AWS unit prices: one 100 GB
  gp3 volume + one t4g.micro (plus its spare) per shard. What a shard holds in
  raw GB per window, and how it compresses to parquet, sets cost per ingested
  GB. That needs a benchmark or prod stats (`shard_archive_stats`,
  `usage_hourly`) and is the next measurement.

**Blocked on Paul**

- *Stripe reconciliation.* Prod SSM `STRIPE_PRICE_*` IDs (`price_1UB1s...`)
  differ from the IDs recorded in `docs/completed/stripe.md`
  (`price_1TPS...`), so that doc is stale at least on IDs. Reading the amounts
  needs the Stripe connector: `mcp__plugin_stripe_stripe__authenticate` opens
  a login flow that must be completed interactively.
- *Prod database read* for `usage_hourly` / `shard_archive_stats` (per-org
  bytes, compression, shard fill). I have not touched prod Postgres.

## Phase 2 findings (2026-10-29, prod Postgres, read-only)

Via `kevcons db query`. Note `usage_hourly` only starts **2026-10-15**.

**Tenants and usage**

| Org | Tier | Comped | Ingest 2026-10-15..29 | ~Monthly |
|---|---|---|---|---|
| loom | yard | no | 12.8 GB | ~27 GB (27 % of 100 GB quota) |
| graybeard | bolt | yes | 4.6 GB | ~10 GB (2 % of 500 GB quota) |

Subscriptions: 3 comped Bolt, 1 paid-tier Yard, 5 trial. Only `loom` is a
non-comp paid tier; the other 7 orgs have recorded no usage. Total ingest is
~37 GB/mo against $219/mo of attributed cost.

**Shard fill**

- 4 INGESTING shards hold 88.0 M rows / 14.6 GB on disk (~166 B/row on disk),
  against 400 GB of provisioned gp3. Volumes are ~4 % utilized: storage is
  not the cost driver, instance-hours and the 100 GB-per-shard sizing are.
- 16 archived shards (2026-09-20..10-18) hold 112 M rows: loom 101 M,
  graybeard 11 M.

**Preliminary per-GB economics (list prices, to be validated)**

- Hot: gp3 $0.08/GB-mo over a ~1-2 week hot window is a few cents per ingested
  GB. Cold: S3 $0.023/GB-mo times months retained, divided by parquet
  compression, is similar or lower even at Bolt's 180 days. Together
  probably well under $0.15 per ingested GB.
- Advertised effective price per included GB is $1.90 (Swatch), $0.79 (Yard),
  $0.60 (Bolt), with overage at $2.00 / $1.00 / $0.75. Storage cost is
  therefore not what constrains pricing; **query compute, cron load, and the
  fixed floor** are, and none of those are measured yet.
- Not yet derived: raw bytes per row (to turn rows into billed bytes) and
  the parquet compression ratio. A per-window join of `usage_hourly` against
  `shard_archive_stats` row counts is needed, and `usage_hourly` history is too
  short to do it for archived windows other than Oct 15 onward.

**Operational note.** `kevcons db query` takes minutes even for small
statements here; 7 `session-manager-plugin` processes from Sep 24-Oct 7 are
still running on the dev machine (leaked tunnels). Not touched.

## Phase 2 findings, part 2: cron and query load (2026-10-29)

**Cron load (prod Postgres, last 7 days)**

| Org | Crons (enabled) | Runs/day | Avg / p95 run | CPU-seconds/day | Non-zero exits |
|---|---|---|---|---|---|
| graybeard | 37 (23, 15 alerts) | ~11,950 | 25 ms / 81 ms | ~300 | 5.8 % |
| loom | 2 (0 now) | ~2,300 | 76 ms / 160 ms | ~175 | 1.4 % |

Schedules in use are hourly, 5, 10 and 15 minutes; nobody uses the Yard/Bolt
1-minute floor. Cron compute is negligible today (~0.35 % of one core). Whole
prod database is **99 MB**, `cron_results` has 114 k rows.

**Cron storage at plan limits (corrected).** An earlier draft of this plan said a
full Bolt customer would need ~32 GB and could fill the 20 GB RDS volume. That
was wrong: it assumed 180 days of raw rows and a guessed row size. Measured
and from the rollup config (`store/schema.sql`, partman):

- Retention: raw `cron_results` **7 days**; hourly 14 d; 4h 35 d; daily 400 d.
- Raw row: **282 B** including indexes (31 MB / ~115 k rows). Rollup rows
  ~350 B upper bound (partition overhead inflates the measurement).
- Rows per tick: **1.20 average, 4 max** (22,753 ticks over 3 days), versus the
  250-tag cap.
- Worst case, 500 crons at 1-minute interval and T = 1.2 rows/tick: raw
  ~6 M rows x 282 B = **~1.7 GB**; rollups ~470 k x 1.2 x 350 B = ~0.2 GB.
  Total **~1.9 GB**, a small fraction of the volume. Cron cost is a rounding
  error; the 250-tag cap (not the cron count) is what would make it large
  (~1.9 GB x T/1.2 if a customer really printed 250 tags every minute).

**Rollup bug found while checking this.** `rollup_cron_4h()` reads hourly
buckets in `[now-12h, now-1h)`, a window aligned to the hour but not to 4-hour
boundaries, and writes with `ON CONFLICT DO UPDATE` (replace). As the window
slides, a bucket that straddles its start is recomputed from the hours still
inside it and overwrites its earlier full value. Prod confirms it (sum of
`count` per day, all tiers, 2026-10-24..27):

| Day | raw | hourly | 4h | daily |
|---|---|---|---|---|
| 10-24 | 10,585 | 10,585 | 3,113 | 3,113 |
| 10-25 | 11,045 | 11,045 | 3,194 | 3,194 |
| 10-26 | 10,875 | 10,875 | 2,983 | 2,983 |
| 10-27 | 8,292 | 8,292 | 2,639 | 2,639 |

Raw and hourly agree exactly; 4h and daily hold ~25-32 % of the true count, and
daily inherits the 4h error. `PickGranularity` uses 4h for ~14-35 day windows
and daily beyond, so count/sum/min/max (and avg, which loses most of its
samples) over long windows are wrong; that covers every window past
14 days, i.e. most of what Swatch (30 d), Yard (90 d) and Bolt (180 d) sell as
retention. The existing `TestRollup_4hCombinesHourly` seeds an aligned bucket
8 hours back, so it does not exercise the straddle. Fixed in migration
`109_rollup_4h_aligned_window.sql` (uncommitted, not yet deployed): the window now
starts on a bucket edge, and the migration rebuilds the 4h and daily rows from
the hourly tier for the ~14 days it still covers. Older 4h/daily rows cannot be
repaired (their raw and hourly sources are gone). Tests:
`TestRollup_4hKeepsBucketThatStraddlesWindowStart`, `TestMigration109_RebuildsDamagedRollups`.

**Query load (coordinator `query rpc: done` log lines, weft's own logs)**

- Last hour: **only `graybeard`** issued queries: 10,723, avg 1.58 s, max 10 s,
  ~17 k query-seconds (about 4-5 queries in flight continuously). No `loom`
  queries at all.
- In the last 10 minutes 1,798 of 1,815 queries were the identical pipeline
  `cat /log/weft/shard/heartbeats.log | wc -l`, about **3 per second**. That is
  an automated poller, not user or cron traffic (crons are ~12 k runs/day;
  this is ~260 k/day). Source not identified.
- Two longer-window runs of the same aggregation (24 h, and ~7 days from
  2026-10-22) failed with `context deadline exceeded (Client.Timeout exceeded
  while awaiting headers)`. **That was not a server limit.** It is the 10-second
  wall-clock timeout on `HTTPCoordinator.client` (`shell/http_remote.go`), a
  default meant for short management calls that `Query` inherited, and the
  server's SSE handler had not sent headers yet. No server-side query deadline
  exists (none found in `api/` or `coordinator/service/`). So these runs show
  only that the aggregation takes more than 10 s, not how long it takes, and
  not that 24 h is unworkable. Its true latency at 1 h, 6 h and 24 h is
  unmeasured; measure it with the client timeout lifted before drawing a
  conclusion about the product. The coordinator log it scans is also inflated
  by the heartbeat poller (~260 k queries/day, each with several log lines).

**Conclusions for the model**

1. The dogfood account is **not representative** for query load: its query
   profile is one runaway poller, and the only paying org issues no queries.
   Per-query compute cost has to come from a synthetic benchmark (typical
   interactive queries, dashboards, cold-shard scans), not from prod.
2. Per-customer cron load is small in CPU and potentially large in
   Postgres storage; model it as a storage and RDS-sizing cost.
3. Query duration is a missing product control, and so a possible plan lever:
   there is no server-owned deadline today, only the client's accidental 10 s.
   A server-side limit that cancels the work and returns a structured, safe
   message (optionally per tier) would bound query cost and could be priced. Design in
   [`query-limits.md`](query-limits.md).
4. The 3 qps heartbeat poller is likely costing real shard/compute capacity;
   worth finding and stopping independent of pricing.

## Phase 2: variable cost per unit

Derive a $/unit for each driver. Prefer measured values over list prices;
use list prices to cross-check.

| Driver | Unit | Where it comes from |
|---|---|---|
| CloudWatch path ingest | per GB | Firehose ingest, CW subscription data, Firehose→coordinator transfer, DLQ/S3 on failure |
| weftd / syslog / HTTP ingest | per GB | NLB LCU (bytes), NAT egress if applicable, coordinator CPU |
| Hot storage | per GB-month | EBS gp3 for shard volumes, instance-hours per shard, spare-pool carry |
| Cold storage | per GB-month | S3 Parquet (post-compression) plus request charges |
| Cold queries | per query | Lambda GB-seconds + S3 GETs; Function URL is free |
| Hot queries | per query-second | shard CPU headroom, compute pool scaling (min 1, max 10) |
| Archive export | per shard-window | ephemeral export fleet runtime (`ephemeral-archive-export.md` says cents) |
| Retention janitor | per GB rewritten | S3 reads/writes, Lambda/compute time |
| Crons | per run | compute, `cron_results` rows in Postgres |
| Egress to users | per GB | results, tail, dashboards |
| Billing | % + fixed | Stripe's fee on $29 is $1.14 (3.9 %), on $99 it is $3.17 (3.2 %); Mailgun |

Key measurements, taken from our own data:

- **Compression ratio** raw → DuckDB (hot) → Parquet (cold). The gap between
  billed GB (raw bytes in `usage_hourly`) and stored GB decides every margin below.
- **Hot window length.** Shards are time-partitioned with hot windows and
  archive on rotation; find how many days stay hot vs cold, per tier, since
  retention is 30/90/180 d but hot residence is a coordinator setting.
- **Query intensity per customer.** From coordinator logs / metrics: queries,
  scanned bytes, compute-seconds per customer per day. Cold-shard Lambda cost
  scales with scan size, not retention alone.
- **Cron load.** 500 crons at 1-minute interval is ~720 k runs/day for one Bolt
  customer; that may be our biggest per-customer cost, not ingest.

**Dogfood analysis (decides representativeness).** For each existing org
(graybeardcorp, plus vimgym / gravitty if they ingest into weft), compute
ingest GB/month, bytes/row, queries/day and scanned bytes, cron count and
interval, retention actually queried, and compare to its plan limits. Report
how far it sits below the "typical" and "heavy" profiles the model needs. If
it covers only a small part of the range, recommend a synthetic profile (a
replayable load against a dev stack) for the heavy end, and say which model
inputs are measured vs assumed.

Data sources: Postgres `usage_hourly`, `shard_archive_stats`, cron tables;
coordinator metrics; Cost Explorer. Use `docs/known-issues.md` and the
ghost-volume notes to subtract waste (orphaned volumes, snapshots) from
"steady state" so the model reflects intended cost, with waste tracked
separately.

## Phase 3: the model

A small script in `scripts/costmodel/` (Python, `pipenv run`), inputs in a
checked-in YAML of unit costs, outputs a CSV/markdown table. No account-specific
numbers in git (see the "no account config in source" rule): the YAML holds
unit prices and ratios; real spend stays out.

Per tier, compute COGS and margin at **10 / 50 / 100 / 200 % of included
ingest**, for a customer with *light / typical / heavy* query and cron load, at
the plan's full retention. Add:

- **Per-milestone platform cost ÷ customers** using the Phase 1 step topology
  (not a single floor), so small-N and scale-out economics are both visible.
- **Comp cost.** Signup is retired and all accounts are comps; report what the
  current comps cost us. Only model trial (1 GB, 24 h) if self-serve signup
  is coming back.
- **Overage margin.** Per-GB marginal cost vs $2 / $1 / $0.75. Overage should
  be well above marginal cost or it is a subsidy for heavy users.

Sensitivity: tornado of the five assumptions that move margin most (likely
compression ratio, hot-window length, cron load, NAT egress, floor).

## Phase 3 results: the cold scan budget (2026-11-08)

The model is built (`scripts/costmodel/`, commit `c9698167`; run with
`pipenv run python scripts/costmodel/model.py`). It resolves Phase 4's first
lever, "bound it with a scan allowance", as a **cold line scan budget**: the
lines a tier may read from cold storage per month, defined as `scan_passes` full
passes over the tier's cold archive (`tiers.yaml`). Because it is a multiple of
stored volume, it scales with the plan by construction. Swatch and Yard get
1 pass; trial gets 0. The implementation plan is `cold-scan-cap.md`.

Inputs (all at 100% of every limit; AWS-only gross margin):

- 155 raw bytes per row; cold Parquet is 0.77 of raw; hot DuckDB is 1.07 of raw.
- Fixed plan facts: a 14-day hot window, retention 30 / 90 / 180 d. Cold days =
  retention - 14, so Swatch has 16 cold days and Yard 76.
- Lambda cost per billion rows scanned: **$0.07 (low) to $0.94 (high)**, from two
  orgs with two no-match-grep samples each (weft coordinator logs: 3.8 s for
  1.3 M rows; loom api: 34.8 s for 1.5 M rows). Rows scanned there is
  approximated by rows in the window. This 13x spread is the weakest input.

| Tier | Price | Cold lines stored | 1-pass budget cost (lo to hi) | COGS (lo to hi) | Margin (lo / hi) | Target |
|---|---|---|---|---|---|---|
| Swatch | $29 | 0.03 B | $0.00 to $0.03 | $1.06 to $1.09 | 92.4% / 92.3% | 95% |
| Yard | $99 | 1.63 B | $0.11 to $1.54 | $14.42 to $15.85 | 82.2% / 80.8% | 87.5% |
| Bolt (dedicated ref.) | quote | 17.8 B | $1.25 to $16.78 | $119.88 to $135.41 (micro) | quote $703 to $794 | 80% |

What it says:

- **The cap is a safety valve, not a margin lever.** Even fully used, one pass
  costs at most $1.54 a month on Yard. Hot storage is the dominant cost
  (Yard: $9.07 of about $15).
- **Neither posted tier reaches its target at today's prices** (Swatch needs
  about $65, Yard about $153 to $168 for the targets in `tiers.yaml`). That gap
  comes from hot storage at 100% shard fill, not from cold scans: Yard's
  required price is already $152 with 0 passes, and the first pass adds $1
  (low rate) to $16 (high rate). Four passes would add $5 to $64.
- **Section 5 of the model reports "affords 0 passes"** for both tiers: with the
  margin target applied, the advertised price has no headroom left for any scan
  budget. Read that as a statement about the target, not about the budget.
- **Why bound it at all:** one tier-sized pass is cheap, but an unbounded
  customer is not. A script that runs a no-match grep over 180 days on every
  dashboard load costs real Lambda time, and the high rate is 13x the low one.

**Two passes instead of one** (model run 2026-11-08, margin at the advertised
price, low / high Lambda rate):

| Tier | 1 pass | 2 passes | Cost of the extra pass |
|---|---|---|---|
| Swatch ($29) | 92.42% / 92.31% | 92.41% / 92.20% | 0.01 to 0.11 points |
| Yard ($99) | 82.23% / 80.79% | 82.11% / 79.24% | 0.12 to 1.55 points |
| Bolt reference (scan cost a month) | $1.25 / $16.78 | $2.50 / $33.56 | quote rises about $7 to $98 |

Doubling the budget is cheap at the low rate and costs Yard about 1.5 points at
the high rate, which would take it from 80.8% to 79.2% against an 87.5% target
it already misses. The high rate rests on one org's two samples, so the real
answer waits for the metering in `cold-scan-cap.md`.

Unmeasured or assumed (flagged by the model): ingest compute $/GB (0), hot-path
query compute and S3 requests (not modeled), cell capacity (100 rows/s), shard
fill (100%, a floor on cost per GB since prod sits near 4%), and Yard's 87.5%
target (a midpoint). The model's prices ($29 / $99) and margin targets (95% /
87.5% / 80%) differ from "What the plans promise today" ($19 / $79 / $299) and
Phase 0's 30%; see the open questions.

## Self-serve relaunch (decided 2026-11-08)

Decisions: the prices above; a 14-day trial of the chosen plan; a card is
required before anything works; the margins in Phase 3 are accepted.

How the card gate works, now built:

- Following the signup link creates the account and an owner, plus a
  subscription row in status `pending_payment` carrying the chosen plan. No
  active subscription means the shell, queries, ingest and the usage page all
  answer "no active subscription" (the same path a cancelled account takes).
- `/api/auth/me` reports `pending_payment_tier`; the SPA redirects any page
  except the payment step to `/signup/payment` while it is set.
- The payment step creates the Stripe subscription (14-day trial, missing card
  at trial end cancels it) and attaches its id to the pending row. The webhook
  activates the row on `customer.subscription.updated` only when the
  subscription is `active`, or `trialing` with a default payment method. A
  trialing subscription exists and gets an update before any card is entered,
  so without that check the plan would activate on the first update.
- Pinned by `TestSignup_VerifyLeavesAccountAwaitingCard`,
  `TestSignup_RefusesBolt` and
  `TestStripeWebhookPromotesPendingSubOnlyOnceCardIsSaved`.

What a trial costs us: the plan in full for 14 days, with no cold data yet
because nothing is older than the hot window, so about 14/30 of the hot-storage
and cron lines of the COGS table, at most roughly $7 for a Yard trial that
uses every limit and never converts.

Still to do before opening signup:

- *(Done 2026-11-08, live mode, account w3ft `acct_1UAThfCuPDquPgxf`)* The two
  flat prices exist: Swatch $29 `price_1UZbNTCuPDquPgxfCY4sETNA` (product
  `prod_VBOfEnhLcWj9w1`) and Yard $99 `price_1UZbNUCuPDquPgxf4bubk3vK` (product
  `prod_VBOf1kHQVpORwx`). Still to do: point `STRIPE_PRICE_SWATCH` and
  `STRIPE_PRICE_YARD` at them (config in Terraform, not ad-hoc SSM) and run one
  signup end to end in Stripe test mode. The old $19 / $79 / $299 prices are
  still active in Stripe; archive them once nothing references them.
- **The overage prices in that account were not metered (fixed 2026-11-08).**
  `Weft * Overage` (for example `price_1UB1s3CuPDquPgxfAjamhOjL`) were
  `usage_type: licensed` with `meter: null`, and the account had no billing
  meter, so the reporter's events went nowhere and a licensed item added with no
  quantity billed nothing. Nothing in weft, `infrastructure-live`,
  `infrastructure-catalog` or `kevcons` creates these Stripe objects. Created in
  live mode: meter `weft_ingestion_kb` (`mtr_61VizaPKQWPEYZ0LE41CuPDquPgxf9xw`,
  sum, customer mapped by `stripe_customer_id`, value key `value`), and metered
  graduated prices attached to it: Swatch `price_1UZbdFCuPDquPgxfBrV7Ju4Y`
  (10,000,000 KB free, then 0.0002 cents a KB) and Yard
  `price_1UZbdFCuPDquPgxfCPMXj4LB` (100,000,000 KB free, then 0.0001 cents a
  KB). No Bolt equivalent, since Bolt is quoted. The account had no
  subscriptions at all, so no live subscription carries the old items.
- `infrastructure-live/prod/weft/main.tf` now names the four new IDs. Not yet
  applied: the plan also carries unrelated changes (cells DynamoDB tables, an S3
  lifecycle change, launch-template pins, an RDS parameter group), and the SSM
  blob also drops the unread `KEYED_ARCHIVES` key. Config is read at instance
  start, so applying only changes what a newly launched instance uses.
- Check in test mode that meter events reported during the trial (the reporter
  sends every ingested KB for any org with a Stripe customer) land on the
  first invoice the way the graduated price expects.
- Abandoned signups keep their org name reserved in `pending_payment`. There is
  no sweep for them yet, unlike unverified accounts.
- A status page, terms of service and a privacy policy; none exist. The
  decisions and a draft are in `service-commitments.md`.

## Phase 4: decisions the model should inform

Candidate levers, in rough order of expected impact:

1. **Retention as the price lever.** If cold Lambda shards make old data nearly
   free, long retention is cheap to give and marketable; if cold scans are
   expensive for heavy users, bound it with a scan allowance instead.
2. **Rebase included ingest vs price.** Swatch's $29 for 10 GB and Yard's $99
   for 100 GB give an effective $2.90 / $0.99 per GB (it was $1.90 / $0.79 /
   $0.60 with Bolt at $299 for 500 GB, before the 2026-11-08 reprice). Check each against measured COGS per GB and against market pricing.
3. **Cron limits.** Min interval and count are cost controls disguised as
   features; set them from the measured per-run cost.
4. **Overage rates.** Keep strictly above marginal cost; consider a flat
   minimum and a cap or auto-upgrade nudge.
5. **Annual discount, trial shape, seats.** Seats cost ~nothing; use them as a
   free differentiator rather than a limit that costs sales.
6. **Whether to keep three paid plans.** Only if the model shows two tiers
   have overlapping margin structure. (No enterprise tier in this pass.)

Market comparison (needs web research, not yet done): Datadog, Axiom,
Better Stack, Papertrail/SolarWinds, Grafana Cloud Loki, Logtail, plus
CloudWatch itself since that is the customer's baseline. Compare $/GB ingested,
retention, query/seat pricing.

## Phase 5: implementation touchpoints (after Paul approves numbers)

- `store/user.go`: `VolumeAllowance`, `OverageRatePerGB`, `LogRetention`,
  `WatchLimit`, `MinWatchInterval`, `TagsPerRun`, `SeatLimit`, tmp allowances.
- `frontend/src/components/PricingTable.tsx` (landing page copy).
- Stripe: prices are immutable. Create new prices and point
  `StripePrices` at them. With no real paying customers there is no
  grandfathering to design, but confirm that before changing live prices.
  Only the two flat prices change: Swatch $29 and Yard $99, recurring monthly,
  on the existing products. The metered overage prices carry the free quota
  and the per-KB rate ($0.000002 Swatch, $0.000001 Yard), and those rates are
  unchanged, but the existing overage prices were not metered, so new metered
  ones replace them (see "Still to do"). The Bolt prices are not used by
  self-serve any more.
- `docs/completed/stripe.md` is stale on Bolt; correct or supersede it.
- `docs/manual` pricing/limits pages, then `make docs` (pre-commit hook does it).
- Tests that pin tier values (`api/rest_write_permissions_test.go`, `store` tests).

## Sequencing

1. Stripe/code reconciliation (blocking, small); Phase 0 is already decided.
2. Phase 1 and Phase 2 measurements run in parallel; they touch different data.
3. Phase 3 model once both land.
4. Phase 4 review with Paul, then Phase 5.

## Open questions for Paul

- The scale-out topology milestones in Phase 1b are my draft; I will ask you to
  correct them before costing them.
- Do vimgym or gravitty ingest into weft (and so count as weft tenants)?
- (Settled 2026-11-08) Self-serve signup is coming back, with a 14-day free
  trial and a card up front. See "Self-serve relaunch".
- (Settled 2026-11-08) The model's numbers are the decided ones: $29 / $99 /
  contact-us for the third tier, with the margin targets in `tiers.yaml`. The
  "What the plans promise today" table and Phase 0's 30% floor are stale and
  need rewriting.
- Does a scan budget of 1 pass per month suit the way Yard customers use cold
  data? The measured rate says we could afford more; the question is the
  product shape. See `cold-scan-cap.md`.
