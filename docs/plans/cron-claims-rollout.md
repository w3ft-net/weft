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

### Phase 1: canary

Tombstone one low-stakes per-minute read-only cron (`coord-uptime`). Verify:
old holder logs the loss; a claim row per slot; `cron_results` has exactly one
row per slot, no duplicates; that cron's lease writes stop. Then restart one
coordinator and confirm no duplicate and no unexplained skip.

### Phase 2: ramp

1, then 5, then every cron without a page notification, then the paging ones
(~8 use `page`/`weft-page`), last. Each step needs a soak and abort criteria:
any duplicate run, any overdue cron, or elevated lease/DB errors. Abort means
rollback (below).

### Phase 3: cleanup

Remove the lease path and tombstones. New crons default to claims.

## Guards

- **Forward quiet period.** Claim mode ignores slots whose fire time is before
  `tombstone.acquired_at + 15s` (10s renew interval plus margin), so the old
  holder's last tick cannot overlap the first claimed run.
- **Fail closed.** A failed claim write means no run and a retry on the next
  tick; never a run without a claim. Worst case is a skipped slot and a logged
  gap, never a duplicate.
- **Overdue-cron check.** Alert if a cron has no `cron_results` row for
  2x its interval. Covers a stopped cron in either mode.

## Rollback runbook

Per cron: do not delete the tombstone. Set the row to `holder='rollback'`,
`expires_at = now() + 45s`. Claim mode stops when the tombstone is gone;
lease mode cannot take over until the 45s pass, so nothing overlaps.
`weft-admin cron-mode rollback <id|all>` does this.

If the *code* is rolled back while tombstones exist, those crons stop (they
don't double-fire). The rollback script must therefore clear tombstones to
expiring leases first. Order: `cron-mode rollback all`, wait 45s, then roll
code.

## Latent bug to fix in Phase 0 (from reading the code, not reproduced)

In `runWatch`, when the lease is lost mid-run the goroutine exits, but
`ce.cancels[id]` stays set (`forgetWatch` only runs when the *initial* acquire
is denied). Reconcile then believes the cron is still running and never
restarts it on that coordinator. Any lease loss, including a DB outage, stops
that coordinator's crons until restart. The dual-mode runner must forget the
watch on lease loss, otherwise rollback won't resume crons. Write the failing
test first.

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

## Open decisions

- Shadow flag default off, enabled through runtime config after the deploy is
  confirmed healthy (assumed).
- `coord-uptime` as the canary (assumed).
