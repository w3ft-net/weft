# Cron claims rollout

Moves cron execution from per-watch leases (`cron:<id>`, 30s TTL, 10s renew) to
per-slot claims, using the pure slot logic already on this branch
(`coordinator/cron_slot.go`: `LatestDueSlot`, `CronSlot.Key/Window/FireAt`).
This doc covers only the *rollout*: how we get there without a double-run or an
unexplained skip while coordinators are rolled and crons are live. Treat it as
a production change.

## Why a marker, not "no lease means claims"

A missing or expired lease is a normal state today (TTL expiry, renewal error,
a database blip). If "no lease" meant "use claims", a coordinator that merely
lost its lease would start running that cron under claims while the other
coordinator still runs it under a lease: a double-run. So the switch is an
explicit marker.

## The marker: a tombstone lease

A row in `leases` for `cron:<id>` with `holder = 'claims'` and
`expires_at = 'infinity'` (the column is `timestamptz NOT NULL`, so infinity is
valid).

- Works against code already deployed: `AcquireLease` only succeeds when
  `holder = me OR expires_at < now`, so a tombstoned lease is denied forever.
- The current holder's next renewal (within 10s) gets `RowsAffected = 0`
  ("lost to another holder") and stops. An in-flight run finishes.
- New code sees tombstones during the reconcile it already does every 30s, so
  the signal is data, not a deploy.

## Status (2026-10-05)

- **Phase 0 done.** Dual-mode code deployed (`cb98b33`); migration 079
  applied. Shadow soaked 01:17-02:50 UTC with one coordinator replacement
  mid-soak; every gate item passed (numbers under Phase 0).
- **Phase 1 canary done.** `coord-uptime` (id 12) has run in claim mode since
  03:14:30 UTC, through a second coordinator replacement. One claim and one
  `cron_results` row per slot, no duplicate, no skipped slot.
- **Not started:** Phase 2 ramp. The overdue guard (below) ships first.
- **Temporary state to remove after the rollout:** `CRON_SHADOW_CLAIMS=true`
  was set with `kevcons config set --no-apply` (not Terraform). The SSM
  parameter is Terraform-managed, so the next `terraform apply` silently
  switches shadow off (fails safe). Don't apply infrastructure mid-soak.

## Phases

### Phase 0: dual-mode code, every cron still on leases

Ship: `cron_claims` table + cleanup job (must exist before shadow is enabled),
the claim scheduler, tombstone-aware reconcile, `weft-admin cron-mode
list|set|rollback`, and the shadow claimer. Old and new versions both run
leases, so a roll is safe. Shadow is behind a runtime config flag, default off;
the flag is also the kill switch.

**Shadow claims** are a dry run: on each due slot every coordinator computes
`LatestDueSlot` per enabled cron and tries
`INSERT INTO cron_claims (key, holder) VALUES ('shadow:'||key, me) ON CONFLICT DO NOTHING`.
Nothing executes; lease mode keeps running the crons. Each result is logged
(`cron_claim shadow result=won|lost key=... lag_ms=...`).
Shadow code recovers panics and never blocks or fails lease-mode runs.

Shadow validates: coverage (exactly one winner per slot), cadence (won claims
per cron vs `cron_results` rows, by count not by who won), jitter/lag after
`FireAt`, clock skew between winner and loser, claim write rate (~1,500/h
predicted), and real gap frequency around rolls.

Shadow cannot validate: execution in claim mode, tombstone transitions, the
quiet period. Those are the canary's job.

Soak: 60 minutes, which covers cadence for the 33 crons with sub-hour
schedules. The 2 hourly crons (`christen-failure-rate-trend`,
`disk-fill-rate`) get at most one slot, so they go last in the ramp after at
least two observed slots. Rare events are replaced by deterministic tests (below) and one deliberate
coordinator restart mid-shadow.

Gate to Phase 1: one winner per slot, no holes, cadence matches, claim writes
within 2x prediction, restart showed continuity and a logged gap at worst.

**Measured (92 min soak, 37 crons):**

| Gate item | Result |
|---|---|
| One winner per slot | primary key guarantees it; no key logged `won` twice |
| No holes | 1,410 slots, 1,410 expected, 0 holes, across a coordinator replacement |
| Cadence | 35 of 37 match `cron_results`; the other 2 were quiet crons (see below), confirmed on schedule in the coordinator log |
| Claim write rate | ~881/h against the ~1,500/h predicted (the prediction was high) |
| Lag after `FireAt` | winners p50 636ms / p99 800ms; losers p50 892ms / max 939ms. Both within the 1s poll; no visible clock skew |
| Restart | no holes; lease-mode crons moved in <= ~60s (`denied` at :26, `acquired` at :56, one reconcile tick later) |

Two artifacts to ignore when reading shadow logs: the first claim per cron
after shadow is enabled has a large `lag_ms` (it claims the latest due slot,
which may have fired minutes ago), and a handover shows one extra lease-mode
run in that 10-minute bucket (old holder's last run plus the new holder's
immediate one). Neither is a duplicate slot.

**`cron_results` undercounts quiet crons.** A cron with no output (an alert
pipeline that is healthy) writes no result rows, and in multi-row mode (an
`alert_key` set) an empty run writes none. `mem-pressure` and
`weft_shard_disk` showed 15-20 minutes with no rows while the coordinator log
showed them running every interval. Cadence checks and the overdue guard
must use `cron_watches.last_run_at` (written at the end of every run in both
modes) or the `cron[...]: ok` log lines, never row counts alone.

### Phase 1: canary

Tombstone one low-stakes per-minute read-only cron (`coord-uptime`). Verify:
old holder logs the loss; a claim row per slot; `cron_results` has exactly one
row per slot, no duplicates; that cron's lease writes stop. Then restart one
coordinator and confirm no duplicate and no unexplained skip.

**Result:** the old holder logged `lost to another holder` 6s after the
tombstone and stopped; claim mode started at the next reconcile tick and
skipped the first slot as `quiet`; slots 03:15-03:25 each have one claim and
one result row; the lease row stayed the tombstone (`claims`, `infinity`).
Deleting a coordinator that had won every slot made the other win the next
slot (claimed 03:24:31.212), with no skip or duplicate.

**Transition gap.** Lease and claim windows are not aligned (lease windows
are `[last_run, now]`, claim windows are epoch-aligned), so switching a cron
leaves a small uncovered span: ~3s for `coord-uptime`, and at most one
interval in the worst case. Accepted; note it when judging a cron's data
around its switch.

### Phase 2: ramp

1, then 5, then every cron without a page notification, then the paging ones
(~8 use `page`/`weft-page`), last. The two hourly crons go in the final batch
(they need two observed slots to be judged). Do not start until the overdue
guard is deployed.

Each step is a `weft-admin cron-mode set <ids> --yes` (dry run first), then a
soak of at least 30 minutes (>= 2 slots of the slowest cron in the batch).
Abort and `cron-mode rollback <ids>` on any of:

- a `cron_overdue` log line or page for a cron in the batch;
- a slot with two result rows, or a claim row with no result row (a skipped
  run) for a cron in the batch;
- sustained `cron_claim result=error` lines (claim store failing);
- elevated lease or DB errors on the coordinators.

Gate queries for a soak (read-only, via `kevcons db query`):

```sql
-- one claim and one result per slot, for the batch's ids
SELECT key, holder, claimed_at FROM cron_claims
 WHERE key LIKE 'cron:<id>:%' ORDER BY claimed_at;
SELECT ran_at, count(*) FROM cron_results
 WHERE cron_watch_id = <id> AND ran_at >= '<tombstone time>' GROUP BY ran_at;
-- holes in a cron's slot sequence (shadow keys: 'shadow:cron:%')
WITH k AS (SELECT split_part(key,':',3)::int id, split_part(key,':',4) iv,
                  split_part(key,':',5)::bigint idx FROM cron_claims
            WHERE key LIKE 'cron:%')
SELECT id, count(*) n, max(idx)-min(idx)+1 span FROM k GROUP BY id, iv
HAVING max(idx)-min(idx)+1 <> count(*);
```

Results rows prove a run only for crons that print output; for quiet crons
use `cron_watches.last_run_at` or the coordinator log. `weft-admin cron-mode
list` (via `kevcons ssh`) shows each cron's mode and lease holder.

### Phase 3: cleanup

Remove the lease path and tombstones. New crons default to claims.

## Guards

- **Forward quiet period.** Claim mode ignores slots whose fire time is before
  `tombstone.acquired_at + 15s` (10s renew interval plus margin), so the old
  holder's last tick cannot overlap the first claimed run.
- **Fail closed.** A failed claim write means no run and a retry on the next
  tick; never a run without a claim. Worst case is a skipped slot and a logged
  gap, never a duplicate.
- **Overdue-cron guard** (`coordinator/cron_overdue.go`). A cron is overdue
  when `now - last_run_at > 2 x interval` (`created_at` if it never ran), in
  either mode. It keys on `cron_watches.last_run_at`, not `cron_results`: a
  quiet cron writes no result rows, so a results-based check would page on
  healthy crons. It runs inside the coordinator, under the
  `cron-overdue-guard` lease so exactly one instance checks, once a minute. It
  must not be a weft cron: crons stop together when the database blips (see
  the latent bug below), and a cron that watched for stopped crons would
  stop with them. It logs `cron_overdue name=... id=... age=...` on entering
  overdue and `cron_overdue_cleared id=...` on recovery. Crons below their
  tier's minimum interval are skipped (they never run). A new guard holder
  re-reports anything still overdue. Blind spot: if every coordinator is
  down, nothing pages.

  **Who gets paged.** Aliases live in each org's `notify.yaml`, so there is
  no platform-wide alias or setting. A tenant opts in by defining the reserved
  alias `system` (`store.SystemAlias`), an ordinary alias with ordinary
  targets:

  ```yaml
  system:
    pagerduty: [platform-oncall]
    slack: ["#ops"]
  ```

  It is for alerts the platform raises on the tenant's behalf, as opposed to a
  cron's own `alert` stage; today the only one is a cron going overdue. Crons
  that newly went overdue in a check are sent as one grouped message per org
  (so an outage that stops every cron at once is one page per tenant, not one
  per cron), once per episode. Every overdue cron is included, whether or not
  it has an alert stage of its own. A tenant without the `system` alias is not
  paged; the failure is only logged.

## Rollback runbook

Per cron: do not delete the tombstone. Set the row to `holder='rollback'`,
`expires_at = now() + 45s`. Claim mode stops when the tombstone is gone;
lease mode cannot take over until the 45s pass, so nothing overlaps.
`weft-admin cron-mode rollback <id|all>` does this.

If the *code* is rolled back while tombstones exist, those crons stop (they
don't double-fire). The rollback script must therefore clear tombstones to
expiring leases first. Order: `cron-mode rollback all`, wait 45s, then roll
code.

## Latent bug fixed in Phase 0 (confirmed in production before the fix)

In `runWatch`, when the lease is lost mid-run the goroutine exits, but
`ce.cancels[id]` stays set (`forgetWatch` only runs when the *initial* acquire
is denied). Reconcile then believes the cron is still running and never
restarts it on that coordinator. Any lease loss, including a DB outage, stops
that coordinator's crons until restart. The dual-mode runner must forget the
watch on lease loss, otherwise rollback won't resume crons. Write the failing
test first.

**Confirmed in prod.** At 22:46:27-38 UTC on 2026-10-04, Postgres dial
timeouts made one coordinator log `renew error, giving it up` and lose every
cron lease within 11 seconds; the old code never restarted them, so crons
(including the alerting ones) were dark until the next deploy's roll about
two hours later. Fixed in `efed344`, with `forgetWatch` comparing the
goroutine's own entry so a late cleanup cannot remove its replacement.

Lead, uninvestigated: Postgres logs showed at least 6 distinct coordinator
client IPs over the Oct 4 hours sampled, more replacement than rolls explain.

## Tests required

- Store: tombstone beats legacy `AcquireLease`; the rollback state expires;
  claim insert is exactly-once under concurrency; cleanup removes old claims.
- Scheduler simulation, fake clock, several coordinators, across every
  transition (lease -> tombstone -> claims -> rollback): at most one run per
  slot, bounded gap.
- Fault injection: claim-store errors mean no run and a retry; lease path
  unaffected.
- Lost-lease-then-restart (the latent bug above).

## Settle time

A slot fires at window close + `cronSettle` + per-watch jitter, so rows
still in flight can land before the window is queried; an early query
undercounts that window permanently. `cronSettle` is 30s. That covers weftd
(sub-second) and the coordinator's 2s shard batching with room to spare, and
most of CloudWatch's 5-60s agent batching. It does not cover the CloudWatch
tail: a cron over CloudWatch-sourced logs can still undercount, and
`below`/`absent` alerts on one can false-fire. The real fix is a per-source
settle derived from the sources a pipeline reads; not built. Lease mode has
no settle at all, so this is not a regression. Cost of 30s: alerts fire up
to 30s later, small against the 5m minimum interval for trial tier.

## Open decisions

- Shadow flag: enabled via `kevcons config set --no-apply` for the rollout
  (temporary). Remove it, and the shadow code, in Phase 3.
- Per-source settle for CloudWatch-sourced crons (not scheduled).
- Tenants without a `system` alias are not told when a cron goes overdue.
  Decided: that should be visible in the product (the cron list), not only in
  a log. Not built yet: which surface (the web list API, the `/cron.d` mount,
  the CLI) and how it is shown is open.
- A platform-wide outage pages every tenant that defined `system`, once each
  per episode, with all of their stopped crons in one message.
- Operators have no page of their own for a tenant's crons going overdue, only
  the `cron_overdue` log line.
- Unrelated, uninvestigated: the coordinator log shows `drain: DLQ'd ~50
  rows ... (no shard serves this service)` for `loom/api`, `loom/worker` and
  `loom/edge`, continuously.
