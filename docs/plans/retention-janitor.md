# Archive retention janitor

> **Status: proposed.** This plan retires the S3 half of the "illusion"
> described in [`retention-enforcement.md`](retention-enforcement.md).
> It **supersedes** that doc's "S3 parquet: lifecycle + delete" section
> (object tagging + per-tier S3 Lifecycle rules), which assumed objects
> are keyed by customer. They are not — see below. The Postgres
> per-customer sweep section of that doc still stands.

## Why object-level lifecycle doesn't work

`retention-enforcement.md` proposed tagging each archived object with
the owning customer's tier and letting one S3 Lifecycle rule per tier
expire it. That only works if an object belongs to a single customer.
It doesn't.

Archived parquet is keyed by **service**, not customer:

- Archive prefix is `{PARQUET_DIR}/{service-label}/{shard-id}.parquet`
  (`shard/shard.go:325` `Archive`, `logdb/shardsm.go:652`
  `archiveS3Prefix`). The prefix is the service set; the customer never
  appears in the key.
- The `customer` dimension lives **inside** the rows (`shard/shard.go:77`),
  and the coordinator enforces tenancy with an unconditional
  `WHERE customer = '...'` predicate on every shard query
  (`docs/architecture.md`). Row-level RLS is only necessary because
  shards are **multi-tenant**.
- **Buckets mix customers by default.** (A future dedicated-bucket
  "whale" would not mix — see the escape hatch in §7 — but the default
  and current reality is mixed.)

So a single `shard-2026w16-small-01.parquet` interleaves a bolt
customer (keep 1y), a yard customer (90d), and a swatch customer (30d).
An S3 Lifecycle rule deletes the whole object or nothing. The only
correct enforcement is a **row-level surgical delete that rewrites the
parquet**, which is also why "compact / refresh statistics" is part of
the job: the rewrite is the mechanism.

## Retention recap

Per-tier retention (`store/user.go` `LogRetention`):

| tier   | retention |
|--------|-----------|
| trial  | 1 day     |
| swatch | 30 days   |
| yard   | 90 days   |
| bolt   | 365 days  |

Mapping: `logs.customer` (string) → `customers.name` → `customers.id`
→ active `subscriptions.tier` → `LogRetention(tier)`. A row
`(customer C, ts)` is over-retained when `ts < now − LogRetention(tier(C))`.

Today this is enforced **query-side only** (`coordinator/registry.go`
`MatchingEntries` clamps the scan window). On disk the rows live
indefinitely. This janitor makes the deletion real.

---

## 1. Capture per-customer archive stats at archive time

Nothing currently knows *which customers* or *what time spans* live in
an archived file without scanning S3. Capture it while the DuckDB
handle is still open and local, inside `evaluateArchiving`
(`logdb/shardsm.go:567`), just before/after the export:

```sql
SELECT customer, MIN(ts), MAX(ts), COUNT(*) FROM logs GROUP BY customer
```

Persist to a new table:

```sql
CREATE TABLE shard_archive_stats (
    shard_id   text NOT NULL REFERENCES shard_records(id) ON DELETE CASCADE,
    customer   text NOT NULL,
    min_ts     timestamptz NOT NULL,
    max_ts     timestamptz NOT NULL,
    row_count  bigint NOT NULL,
    PRIMARY KEY (shard_id, customer)
);
```

This is the single biggest cost lever: the planner (§4) classifies
every file as delete / keep / rewrite **without touching S3**. Stats
are refreshed after every janitor rewrite.

## 2. Retention resolver (live join, cached per run)

`RetentionResolver` loads `customer name → LogRetention(tier)` once per
run from `customers ⋈ subscriptions (status='active')`. Use the **live
join** (option 1 in `retention-enforcement.md`), not a baked
`retain_until` column, so tier **downgrades** are honored without a
retroactive backfill.

Two deliberate safety rules — deletion is irreversible:

- **Unmapped customer → do not delete, flag it.** A customer with rows
  but no active subscription (Stripe lag, provisioning race) must not be
  treated as trial-tier and erased in a day. Emit a metric + log and
  keep the rows. A missing subscription row is the wrong thing to
  trust.
- **Grace buffer.** Delete at `retention(tier) + GRACE` (configurable,
  default 7d), not exactly at the advertised boundary. Cushions clock
  skew and the downgrade-then-re-upgrade-loses-data complaint, while
  keeping the on-disk illusion honest within a small margin.

## 3. The rewrite (janitor core)

For a file with at least one over-retained customer past the grace
buffer:

1. **Stage cutoffs.** Build a DuckDB temp table
   `cutoffs(customer TEXT, cutoff TIMESTAMP)` from the resolver (mapped
   customers only).
2. **Filter-rewrite to a NEW key** (never in place), reusing the
   in-process DuckDB + `S3Config` path (same engine as `ColdShard`
   in-process mode and `Archive`'s `COPY`):

   ```sql
   COPY (
     SELECT l.* FROM read_parquet('<old>') l
     LEFT JOIN cutoffs c USING (customer)
     WHERE c.customer IS NULL          -- unmapped: keep (safety)
        OR l.ts >= c.cutoff            -- mapped: keep only within retention
   ) TO '<new-key>' (FORMAT PARQUET)
   ```
3. **Recompact + refresh statistics for free.** A fresh `COPY` rewrites
   row-group min/max statistics and re-running `pickPartitionGranularity`
   (`shard/shard.go:367`) re-sizes the output: collapse a shrunken
   hive-partition dir back to a single file, or re-partition if still
   large. Compaction is not a separate step — it falls out of the
   rewrite.
4. **Atomic cutover.** One Postgres txn:
   `UPDATE shard_records SET parquet_path = '<new-key>'`. Cold-shard
   readers resolve `parquet_path` per query (`shard/cold_shard.go:121`),
   so flipping the pointer *is* the cutover; no reader sees a
   half-written file.
5. **Deferred GC of the old object.** Don't delete the old key
   immediately — an in-flight Lambda cold read may still be streaming
   it. Tombstone it (`old_parquet_path`, `delete_after`) and sweep past
   a grace window.
6. **Empty result → retire the shard.** If 0 rows survive, delete the
   object, clear `parquet_path`, transition the shard to `GONE` (the
   existing cleanup sink).

---

## 4. Scheduling: a daily job on ephemeral compute

This is **not** a provisioner sweep. It runs once a day, does real
I/O, and the provisioner stays clean.

### The unit of work is one parquet *file*, not one shard

"Takes real resources" is true in aggregate (a day's worth of corpus)
but not per unit. The archiver caps files at ~150 MB and
hive-partitions large shards (`pickPartitionGranularity`,
`shard/shard.go:354`). If the work unit is **one file**, every unit
fits a 10 GB / 15-min Lambda comfortably — DuckDB-in-Lambda is already
proven by the cold-shard path. Aggregate scale is a **fan-out
concurrency** problem, not a per-unit sizing problem.

The time-partitioned layout collapses most units to a no-op. A
partition file spans `[t0, t1]`; a row `(customer C, ts)` is deleted
iff `ts < now − retention(C)`. Therefore, using `shard_archive_stats`
alone (no S3 read):

- **Wholesale-delete** (`DeleteObject`, no rewrite) when
  `t1 < now − max(retention of customers present)` — even the
  longest-retention tenant has fully aged out. Bolt keeps a file alive;
  once bolt is gone too, drop the object.
- **Wholesale-keep** (skip) when `t0 ≥ now − min(retention present)`.
- Only the **boundary band** between those two needs a row-level
  rewrite (§3).

A realistic daily run object-deletes a few aged-out files and rewrites
a thin band of boundary files. That is squarely Lambda-shaped.

### Four roles, none of them the provisioner

```
EventBridge Scheduler (daily cron)
  └─> Planner      Postgres only: shard_archive_stats ⋈ RetentionResolver
                   → work list of {file, action: delete|rewrite|skip, cutoffs}
  └─> Compactor    per file, fanned out: DuckDB read-filter-write to a NEW key
                   returns {new_key, rows_kept, rows_deleted, bytes}
  └─> Finalizer    holds Postgres: atomic parquet_path swap, refresh
                   shard_archive_stats, write audit row, tombstone old key
```

The planner and finalizer are the only Postgres-touching roles and are
cheap (no S3 I/O) — they can be one small orchestrator. The compactor
is the only heavy role and is pure S3 + DuckDB.

`next_purge_at` on `shard_records` (the earliest future time any present
customer crosses its cutoff, derived from `shard_archive_stats` +
resolver) gates the planner: shards not yet eligible never enter the
work list, so the daily run is bounded by churn, not total corpus.

## 5. Why Lambda is the default — it sidesteps the internal API

The reason to reach for an internal worker API is the spot-instance
pull/report loop. Lambda dissolves it:

- **Invocation payload = the work** (`{parquet_path, cutoffs, action}`);
  **response = the result** (`{new_key, counts, bytes}`).
- Compactor Lambdas **never touch Postgres** — they read/write S3 and
  return metadata. The finalizer (already holding the DB) does the
  pointer swap from the response.
- No standing service, no worker→coordinator API, no DB creds on
  workers.

Extend the cold-shard Lambda's role with S3 `PutObject`/`DeleteObject`,
or — cleaner — stand up a **sibling "compactor" Lambda** so read-only
cold reads and mutating rewrites have separate IAM blast radius. This
is less infrastructure than the spot path and keeps the mutation
surface tiny.

## 6. Crash / interruption safety (both substrates)

- Compaction writes a **new key**; the Postgres `parquet_path` swap is
  the only commit point. A dead worker leaves an orphaned temp key
  (GC'd by a sweep), never a torn read.
- A `janitor_runs(file_key, run_date, state, leased_until)` table makes
  the work list idempotent and resumable — a re-run skips files already
  finalized today.
- The finalizer caps concurrent compactor invocations (reserved Lambda
  concurrency, or spot fleet size) so a big day can't stampede S3 or the
  account Lambda limit.

## 7. Escape hatch: spot EC2 + claim/report API (document, don't build)

The one case Lambda can't hold: a **whale** (dedicated, un-mixed bucket)
whose single boundary file exceeds 10 GB / 15 min. Then a longer-lived
spot worker batching files is warranted, and *that* is when an internal
API earns its place:

- A small gRPC/HTTP endpoint on the coordinator (not a new service):
  `ClaimFiles(n) → [assignment]` with a lease, `ReportResult(file,
  result)`. The coordinator does the Postgres pointer swap on report, so
  workers still never get DB creds. Lease + idempotent new-key write
  covers spot interruption: a killed worker's lease expires, the file
  re-enters the queue, the orphaned temp key is GC'd.

Per the no-speculative-code rule, **write this into the plan but don't
build it.** Ship Lambda fan-out; stand up the claim/report API only when
a real unit exceeds Lambda's envelope — which, by construction, only
happens for un-mixed whale buckets.

## 8. Compliance hard-delete (same mechanism, different predicate)

Customer offboarding / GDPR "prove it's gone" is the **same rewrite**
with predicate `customer = $X` (ignore age) across every shard where
`shard_archive_stats` shows that customer. Build the rewrite engine
parameterized by predicate so routine retention and compliance deletion
share one path. Write a `purge_operations` audit row per rewrite — that
trail is what turns "filtered from responses" into "provably deleted":

```sql
CREATE TABLE purge_operations (
    id           bigserial PRIMARY KEY,
    shard_id     text NOT NULL,
    customer     text,            -- NULL for routine multi-customer passes
    reason       text NOT NULL,   -- 'retention' | 'compliance'
    rows_deleted bigint NOT NULL,
    bytes_before bigint NOT NULL,
    bytes_after  bigint NOT NULL,
    ran_at       timestamptz NOT NULL DEFAULT clock_timestamp()
);
```

---

## Touch list

New:
- `store/migrations/NNN_retention_janitor.sql` — `shard_archive_stats`,
  `purge_operations`, `janitor_runs`, and columns on `shard_records`:
  `next_purge_at`, `last_purged_at`, `old_parquet_path`, `delete_after`.
- `logdb/janitor.go` (or a `janitor/` package) — `RetentionResolver`,
  planner classification, the predicate-parameterized rewrite engine,
  deferred GC.
- `cmd/lambda-compactor/` — sibling Lambda handler invoking the rewrite
  engine, S3-only IAM.
- EventBridge daily schedule (Terraform, `infrastructure-{live,catalog}`).

Edit:
- `logdb/shardsm.go` `evaluateArchiving` — capture `shard_archive_stats`.
- `shard/shard.go` — reuse `pickPartitionGranularity` / `exportParquet`
  / `S3Config` from the compactor.

Reuse:
- DuckDB in-process + `S3Config` (cold-shard read path), `COPY ... TO
  's3://'`, the partition-sizing logic.

## Risks

1. **Irreversible deletion on bad tier data** → unmapped-skip + grace
   buffer + `purge_operations` audit.
2. **Rewrite cost** → `shard_archive_stats` classification (no blind S3
   scans) + `next_purge_at` gating + wholesale-delete of aged-out
   partitions (object delete, no rewrite).
3. **Reader tearing during cutover** → write-new / flip-pointer /
   deferred-GC ordering.
4. **Hive-partitioned shards** → rewrite reads the `/**/*.parquet` glob,
   re-exports, deletes the whole old dir. Needs an explicit test; extend
   `docs/plans/shard-archive-test.md`.

## Order of operations

1. `shard_archive_stats` capture at archive time + migration (small,
   unblocks everything; no behavior change yet).
2. `RetentionResolver` + planner classification (Postgres-only, testable
   without S3).
3. Rewrite engine + compactor Lambda, exercised on dev Docker-Compose
   parquet first.
4. EventBridge daily schedule + finalizer pointer-swap + deferred GC.
5. Compliance hard-delete predicate (reuses 3) + audit surfacing.
6. Spot + claim/report API — only if/when a whale unit exceeds Lambda.
