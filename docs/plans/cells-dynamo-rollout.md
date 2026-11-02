# Rolling out cells and the DynamoDB control plane without downtime

> **Status: proposed (2026-10-29).** The architecture is in
> [`cost-and-pricing.md`](cost-and-pricing.md) section 1c: org-partitionable
> data lives in per-org Postgres cells; only truly cross-org data (org
> directory and indexes, identity, shard fleet, fleet lease) lives in DynamoDB;
> the existing `weft-postgres` becomes cell 0. This document is only about
> getting there without a maintenance window. Nothing here is built.

## Method

Every stage is **expand, dual-write, backfill, shadow-read, flip reads, flip
writes, soak, contract**, and every stage is reversible until a named point of
no return. Three things make that cheap here:

- **No customer data moves.** Existing orgs stay on cell 0. Only orgs created
  after cell 1 opens are routed there. The only data that migrates is the global
  tables, which are tiny (the whole production database is 99 MB), so backfills
  are seconds, not events.
- **The data plane does not depend on the stores being cut over.** Ingest and
  queries run from the coordinators' in-memory shard registry and the shards
  themselves; a registry cutover does not sit in a request's path.
- **Postgres stays authoritative until the last step.** Dual-write keeps it
  current, so rolling back is a read-side flip, not a restore.

## Facts from the code that shape the plan

- Two processes open Postgres: `cmd/coordinator` and `cmd/api` (the API server
  checks auth against the store directly). Both must learn routing, and a
  release must be compatible with the previous one running beside it.
- Deploys are rolling instance refreshes behind the NLB (two coordinators), so
  **N and N-1 always overlap**. There is no runtime version negotiation between
  coordinator, shard and compute, so every stage below needs a **bridge release**
  that works with both worlds.
- **No hot config reload.** A flag flip needs a restart (decision 1).
- The registry is already behind an interface: `logdb.RegistryStore`
  (`SaveShard`, `ListShards`, `GetShard`, `TouchHeartbeat`,
  `SetShardIngestFailures`, `ListShardServices`, `AddShardService`). A DynamoDB
  implementation and a dual-write wrapper slot in without touching the FSM.
- Leases are one small conditional upsert (`store/leases.go`), and the FSM
  fences with an in-process `leading()` check (`logdb/fence.go`), not an atomic
  one.
- A shard transition touches a handful of rows, nowhere near the 100-item limit
  of `TransactWriteItems` (confirmed by Paul).

## Stages

### Stage 0: seams and infrastructure, shipped dark

- Narrow interfaces in the single `store` package (one Postgres-backed
  implementation each, no behaviour change): **Directory** (org to cell, name
  reservation, stripe-id index), **Identity** (users, magic links, device codes,
  notices, user-to-orgs index), **Leases**, and the existing **RegistryStore**
  extended to the other fleet tables. Call sites are mechanical: roughly 25 files
  touch users, 14 memberships, 11 organizations. A `gopatch` pass is the right
  tool. The router lives in `Store` (`store/tenant.go` is the hook) and always
  returns cell 0.
- Terraform: DynamoDB tables (on-demand, point-in-time recovery, deletion
  protection), IAM for the coordinator and API roles, a DynamoDB gateway VPC
  endpoint (free, like the existing S3 one). Local dev: DynamoDB Local in
  docker-compose. Run the Terraform safety check on every apply.
- One **contract test suite** per interface, run against the Postgres
  implementation and the DynamoDB one, so the two cannot drift.
- Gate: all tests green, a release soaked with the interfaces in place and
  behaviour unchanged. Reversible: yes, fully.

### Stage 1: org directory, still one cell

- Backfill the directory (name to org id and cell `c0`, stripe id to org) from
  `organizations`; dual-write on create, rename, and stripe-id change
  (Postgres authoritative).
- Shadow-read: resolve through both on the request path, count mismatches,
  alert on any. After a clean soak, flip reads to DynamoDB with Postgres
  fallback on a miss.
- New agent tokens are minted with an opaque `cell` and an HMAC over cell and
  public id. Verification accepts "no hint" as cell 0 for now, so existing
  tokens keep working.
- This proves the routing path, the new dependency, and its failure behaviour in
  production **before a second cell exists**, with zero customer-visible change.
- Reversible: yes (Postgres still authoritative).

### Stage 2: identity

- Order inside the stage: dual-write (bridge release on **both** api and
  coordinator), backfill, shadow-read, flip reads (DynamoDB primary, Postgres
  fallback on a miss, so nobody is logged out), flip writes (DynamoDB
  authoritative, Postgres still written), soak, stop the Postgres writes.
- `users` needs an email-uniqueness item written in the same transaction
  (`TransactWriteItems`), mirroring today's unique lowercase index.
  `last_active_org_id` is a plain attribute update on org switch.
- Magic links and device codes are short-lived: skip the backfill, write new
  ones to DynamoDB, read both stores until the longest TTL has passed.
- The user-to-orgs index is derived from cell memberships and fed by a repair
  job; it serves only login and the org picker. Per-request authorization stays
  in the cell.
- Per-request session build becomes: identity from DynamoDB (cached) plus
  membership, permissions and tier from the cell. The cache TTL is 60
  seconds (decision 2).
- Reversible: until the Postgres writes stop (point of no return 1).

### Stage 3: shard fleet registry and the fleet lease (the hard one)

1. DynamoDB implementation of the fleet stores behind the same contract suite,
   with the key and GSI rules from `cost-and-pricing.md` (heartbeats on their own
   item, no low-cardinality GSI on heartbeat-updated attributes, no scans,
   strongly consistent reads for lease and FSM state).
2. Dual-write wrapper (Postgres primary), including `TouchHeartbeat`; backfill.
3. Shadow-read: `RefreshFromStore` already reloads from the store, so diff the
   two listings there and alert on any difference.
4. **Lease bridge.** The new release takes the `shard-fsm-leader` lease in both
   Postgres and DynamoDB and leads only if it holds both; if the DynamoDB
   acquire fails it releases the Postgres lease. An old-version coordinator only
   knows Postgres, so a bridge coordinator holding both locks it out. That keeps
   one leader across mixed versions. Followers need nothing.
5. Flip reads to DynamoDB after N consecutive clean diffs. Writes stay dual, so
   rollback is flipping reads back. Then flip the leader's FSM writes to
   `TransactWriteItems` with a `ConditionCheck` on the lease item (this is where
   fencing becomes atomic), soak, then stop the Postgres fleet and lease writes.
6. `shard_services` gets a DynamoDB copy keyed by shard for the reverse lookup
   (decision 4), dual-written and diffed like the other fleet items.
- Customer impact: none expected. Ingest and queries use the in-memory registry
  throughout. A seconds-long gap with no FSM leader is harmless: the FSM is
  convergent, there is no separate reconciler.
- Reversible: until the Postgres fleet writes stop (point of no return 2).

### Stage 4: contract the global tables

After a soak at full dual-write, rename the global tables out of `weft-postgres`
(keep them for several weeks), take a final dump to S3, remove the dual-write
code in a cleanup release, then drop. `weft-postgres` is now cell 0 and has no
global tables. Point of no return 3 is the drop; the rename is reversible.

### Stage 5: cell-aware workers, still one cell

Everything that scans all orgs learns to loop over the cell list, which is
`[c0]` for now: the cron scheduler and its cell lease (in the cell's Postgres),
rollups (`pg_cron` is already per-database), the retention janitor, Stripe
usage reporting, usage rollups. Shipped with one cell, so it is a no-op that
proves the code path. The cell list comes from DynamoDB; connections are dialed
lazily per cell and pooled.

### Stage 6: the first real second cell

- Terraform: one more `modules/rds` instance, a secret, security group rules;
  bootstrap with `schema.sql` plus goose, then the `pg_cron` and partman
  reconstruction per instance (the reconstruction currently pins `pg_cron` to the
  `weft` database, so verify it for a new instance).
- Register the cell in the directory as `closed`. Create an **internal test org**
  assigned to it and exercise the whole lifecycle: signup, token, ingest, query,
  crons, alerts, rollups, janitor. Then the chaos test: stop cell 1's database
  and confirm only that org fails.
- Open the cell: new orgs are assigned to the newest open cell. No existing org
  moves. Turn on "hint required for non-c0 tokens".

## What could still cause downtime

- **Deploys.** Each stage is several releases and each is a rolling restart of
  two coordinators. No stage needs a maintenance window, but the whole rollout
  is only as zero-downtime as an ordinary deploy is today (ingest streams
  reconnecting, CLI sessions dropping). Measure that first with a deploy test;
  the stream-leak and shard-roll history says it is not free.
- **Mixed-version windows.** Dual-write bridges exist precisely for this;
  missing one (for example a path that writes only Postgres) shows up as a
  shadow-read mismatch, so the alert is a gate, not a nicety.
- **A bad flip with no hot reload** costs a restart to undo (decision 1). That
  is why each flip is its own release and the shadow-read gate precedes it.
- **DynamoDB throttling or an IAM mistake.** The Postgres fallback on a miss
  covers reads through Stage 2; for the fleet, the in-memory registry carries
  the data plane while a coordinator retries.
- **Cold cache after a restart** hitting DynamoDB at once is a small, bounded
  burst at current scale.

## Verification gates

Every flip is gated by: zero shadow-read mismatches for a soak period,
DynamoDB throttle count at zero, session-build latency within today's, and a
passing contract suite on both implementations. Mismatch and throttle counters
go through `weftsdk` logging (never stdlib `log`), with an alert on the
dogfood account, which stays out of git.

## Decisions (2026-11-02)

1. **Flips are a restart.** There is no rollout-state item and no hot reload. Each
   flip is a committed config change (Terraform) followed by a rolling restart of
   the coordinators and API; the state of the rollout lives in version control.
   Consequence: a rollback also costs a restart, so every flip is its own release
   and the soak lengths below are measured from the restart that completes it.
2. **Identity and permission cache TTL is 60 seconds.** Revocation takes effect
   within a minute; during a DynamoDB outage, cached identity is served for at
   most that long and then fails closed.
3. **Soak lengths are tiered.** Stage 1 (directory): 2 days per flip. Stages 2 and
   3 (identity, fleet and lease): 7 days per flip, and each soak must span at least
   one weekly cycle and one ordinary coordinator deploy.
4. **`shard_services` gets a DynamoDB copy keyed by shard** for the reverse
   lookup, written and repaired alongside the other fleet items. This lands in
   Stage 3 rather than waiting for Stage 6, so Stage 3 item 6 no longer defers it.
5. **Coordinators are shared across cells.** One fleet, per-cell connection pools
   dialed lazily; a failing cell's connections must never stall another cell's
   requests (bounded dial timeouts, per-cell circuit breaker).
6. **Isolation.** The work lives in its own worktree off `main`, not in
   `per-service-tables`, with the `gopatch` rewrite and generated code kept off
   other in-flight work.
