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
- **Phase 2 ramp applied.** All 37 crons are on claims: batch A (5) 14:45,
  batch B (22) 15:22, batch C (7 paging) 15:59, batch D (2 hourly) 16:41 UTC.
  All four passed their gates. **Phase 2 is complete.** Results under Phase 2.
- **Phase 3 code done** (not yet deployed at the time of writing): the lease
  path, tombstone and mode machinery, shadow claimer, `weft-admin cron-mode`
  and its RPCs are removed; every enabled cron runs under claims by default.
  The tombstone rows in the database are deliberately left (see Phase 3).
- **Overdue guard deployed** (`0cf7130`, rolled 12:56-13:09 UTC). Log-only for
  tenants without a `system` alias.
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

Gate queries for a soak (read-only, via `kevcons db query`). Real claim keys
are `cron:<id>:<interval>:<index>` (id is field 2, index field 4); shadow keys
are `shadow:cron:<id>:<interval>:<index>` (fields 3 and 5). Using the shadow
field numbers on real keys returns nothing, silently.

```sql
-- continuity per cron: claims (n) vs the span of slot indexes; holes = span - n
SELECT split_part(key,':',2) AS id, count(*) AS n,
       max(split_part(key,':',4)::bigint)-min(split_part(key,':',4)::bigint)+1 AS span,
       max(claimed_at) AS last_claim
  FROM cron_claims
 WHERE key LIKE 'cron:%' AND split_part(key,':',2) IN ('<id>', ...)
 GROUP BY 1 ORDER BY 1::int;
-- run recency for every enabled cron; hourly needs its own case, because
-- the '*/N' pattern does not match '0 * * * *' and it would read as every minute
SELECT count(*) AS crons,
       count(*) FILTER (WHERE age > 2*mins*60) AS overdue,
       round(max(age/(mins*60.0))::numeric,2) AS worst_age_in_intervals
  FROM (SELECT extract(epoch FROM now()-last_run_at) AS age,
               CASE WHEN schedule ~ '^0 \* ' THEN 60
                    ELSE COALESCE(substring(schedule from '^\*/([0-9]+) ')::int, 1) END AS mins
          FROM cron_watches WHERE enabled AND deleted_at IS NULL) t;
```

Results rows prove a run only for crons that print output; for quiet crons
use `cron_watches.last_run_at` or the coordinator log. `weft-admin cron-mode
list` (via `kevcons ssh`) shows each cron's mode and lease holder.

Operating notes: `kevcons ssh` without `--instance-id` picks an instance for
you, and `kevcons db query` relays through one too; either can stall or fail
if it picks an instance that is being replaced. Gate checks that read logs
must be run on each coordinator (`--instance-id`), since a claim can be won on
either. Do the log counts with simple greps: nested quoting through
`--command` fails with a usage dump.

**Results (2026-10-05).**

| Batch | Crons | Applied (UTC) | Gate |
|---|---|---|---|
| A | 8, 10, 18, 20, 22 (collectors) | 14:45:33 | 1m crons 33 claims / span 33, 5m crons 6 / 6; 0 holes; all ran within 2 intervals |
| B | 22 non-paging crons incl. org 2's 28 and 29 | 15:22:01 | 205 slots, 0 holes, >= 2 real slots each; 0 overdue; 0 errors |
| C | 2, 3, 4, 13, 17, 24, 25 (paging) | 15:59:34 | 132 slots, 0 holes, >= 3 slots each; 0 overdue; worst run age 1.00 intervals; 0 errors |
| D | 15, 21 (hourly) | 16:41:06 | three real slots each (17:00, 18:00, 19:00); see final gate |

**Final gate (19:25 UTC, all 37 crons).** 4,202 claim slots, 0 holes, no cron
with fewer than 3 slots; 0 overdue, worst last run 0.97 intervals; 0 errors,
overdue lines or panics on either coordinator. Both coordinators were
replaced at 18:12 and 18:18 by an instance refresh (a deploy), and both new
ones came up with all 37 crons in `claims`: the whole claim-mode fleet went
through a full coordinator roll with no holes.

The canary (`coord-uptime`) had 724 claims over a span of 724 from 03:15 to
15:18 across three coordinator replacements.

What the ramp showed:

- **Alert dedupe works in claim mode.** `gc-pause-spike-coordinator` crossed
  its threshold at 16:21:55 and paged once (Slack and PagerDuty); the five
  later runs still over the threshold sent nothing. That alert was a real GC
  pause p99 of 84 ms, present in lease mode before batch C.
- **A restart during a batch is a free test.** A coordinator was replaced at
  16:02 between batch C's apply and its gate; claims stayed contiguous.
- **Switch-over transition.** Windows differ between modes, so a switch can
  leave a gap (a minute-cron lost ~3s) or overlap (an hourly cron's first
  claimed window starts at the previous hour, inside what its last
  lease-mode run covered). Harmless for rates and counts; note it when reading
  data around a cron's switch.
- **Monitors that count lease events** (`lease-holders`, `lease-flap-rate`,
  `lease-contention`) read `lease_event` log lines for every use of the lease
  primitive, not only crons. Once crons stop using leases they keep measuring
  the remaining ones (NAT, the shard FSM leader, the overdue guard), so they
  need no change; their numbers just drop. If visibility into claim contention
  is wanted, add a monitor on `cron_claim result=lost|error` lines.

### Phase 3: cleanup

**3a, code (this change).** Every enabled cron runs under claims; there is no
lease path and no per-cron mode. Removed: `runWatch` and its per-cron lease,
the sliding `[now-interval, now]` window (`watchQueryRange`), the immediate run
on acquire, the forward quiet period, the shadow claimer and
`CRON_SHADOW_CLAIMS`, the tombstone/rollback/mode store APIs, the three
`WeftAdmin` cron-mode RPCs and `weft-admin cron-mode`. Kept: the claim
scheduler, `cron_claims` and its hourly prune, and the overdue guard (it
doesn't depend on the scheduling mode). Behaviour changes to know about:

- A new cron's first run is its next slot (a slot whose window closed before
  the cron was created is skipped), not an immediate run on creation.
- A coordinator that starts takes the latest due slot if no one else has;
  that is normal claim behaviour, not a duplicate.

**3b, tombstone rows (later, separate step).** The tombstones are `leases` rows
named `cron:<id>` with `holder = 'claims'` and `expires_at = 'infinity'`. They
are inert to the new code. Do not delete them in the same release as 3a: during
the roll an old coordinator that no longer sees a tombstone would start that
cron under a lease while a new one claims it, a double run. Delete them only
once every coordinator is on the 3a code and it has been stable for a while:

```sql
-- tombstones plus any stale per-cron lease rows from the lease era
DELETE FROM leases WHERE name LIKE 'cron:%';
```

Until 3b, rolling the code
back is safe, because the previous code still reads the tombstones.

Also after the 3a roll: remove the by-hand `CRON_SHADOW_CLAIMS` key from the
SSM config blob (new code ignores it).

## Guards

- **Forward quiet period** (removed in Phase 3a, with the lease path it
  protected): claim mode ignored slots whose fire time was before
  `tombstone.acquired_at + 15s`, so a previous lease holder's last tick could
  not overlap the first claimed run.
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

Per-cron rollback (`weft-admin cron-mode rollback`) existed only while both
paths did, and was removed in Phase 3a. Now:

- To go back, deploy the previous commit. Its coordinators read the tombstones
  (3b has not run) and run every cron in claim mode, exactly as before 3a.
- Do not run the 3b `DELETE` until you no longer want that option: once the
  tombstones are gone, the pre-3a code would run every cron under a lease
  again, and any rolling deploy mixes the two paths for a few minutes.
- Before 3a (history): per cron, set the tombstone row to `holder='rollback'`,
  `expires_at = now() + 45s`, so claim mode stops and the lease path cannot
  take over until the 45s pass.

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

- Store: claim insert is exactly-once under concurrency; cleanup removes old
  claims.
- Scheduler simulation, fake clock, several coordinators and a restart: every
  slot runs exactly once, windows tile with no gap or overlap and span exactly
  one interval.
- Fault injection: claim-store errors mean no run and a retry.
- A cron created after a slot closed skips that slot; every enabled cron
  starts under claims with no tombstone and takes no lease; disabling stops
  it and re-enabling restarts it.
- Overdue guard: see `coordinator/cron_overdue_test.go`.
- Retired with the lease path: tombstone-beats-`AcquireLease`, the rollback
  state, lease -> tombstone -> claims transitions, and lost-lease-then-restart
  (the latent bug above, which cannot occur without the per-cron lease).

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
- Unrelated to this rollout, cause already found elsewhere: **coordinators
  fail their ELB health check every few hours and are replaced by the ASG**
  (10-04 10:42, 12:14, 16:04 and 10-05 06:56, 09:00, 11:50, 16:02 UTC, the
  first three before shadow or claims existed). The cause is
  `RingBuffer.Drain` in `coordinator/ring_buffer.go`: it never cleared drained
  slots, so up to 1M rows stayed reachable. The fix (nil the slot, with tests)
  was uncommitted as of 2026-10-05, and a byte-bounded capacity is still open.
  The 16:02 victim's runtime metrics fit it: heap rose in a straight line from
  320 MB (13:47) to ~1.2 GB (16:00), about 6-8 MB/min from boot, flat across
  all three ramp batches and while the instance held no cron leases.
- Shard hosts' logs went sparse after a coordinator replacement. Host
  `...29-158` heartbeated every minute until 16:03, then about every ten
  minutes. `shard-host-count` therefore flips 4/5 depending on which
  five-minute windows contain its heartbeat, and `shard-host-loss` paged at
  17:00:36 on a -1 delta. This is in the data, not an artifact of claim
  windows (the raw heartbeats show it, and `...28-225` was sparse for hours
  before the ramp). Whether the host is unhealthy or its weftd reconnected
  badly after the replacement is not established.
- Unrelated, uninvestigated: the coordinator log shows `drain: DLQ'd ~50
  rows ... (no shard serves this service)` for `loom/api`, `loom/worker` and
  `loom/edge`, continuously.
