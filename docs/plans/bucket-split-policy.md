# Bucket-split policy (future work)

Placeholder. The bucket-split path — moving a dominant app out of the
shared `default` bucket into its own `dedicated-<app>` bucket — is
disabled today because the initial implementation has a runaway in it.
This doc captures what the fix needs to do before splitting can be
re-enabled. Recorded here so `shard-lifecycle.md`'s "dedicated fleets
for high-throughput apps" note has somewhere to point to.

## What's disabled

`coordinator/app_buckets.go:EvaluateSplit` is a `return "", nil` stub.
The body of `Split` is still intact and correct for the narrow case it
was written for (remove app from source, create dedicated bucket with
the same tier/window/retention). It just has no caller.

## What went wrong (2026-04-22)

Production ran for a while with the split path live. Result: five
buckets (`default` + four `dedicated-*`), each with
`target_ingesting_count = 2`, summing to a computed
`SetDesiredCapacity(small) = 10` against an intended fleet of 2.

Two distinct bugs interacted:

1. **`Split` inherits, doesn't move.** In `app_buckets.go:223` the
   dedicated bucket is created with `Target: src.Target`, and `src`
   is saved back with the same `Target` unchanged. Every dedicated
   bucket adds another `target` to the tier-wide sum in
   `syncASGDesiredCapacity`. Splitting is supposed to reshape a
   fleet; today it multiplies it.

2. **`OnboardApp` re-adds split-off apps to `default` after
   coordinator restart.** `SeenApps` is an in-memory dedup; every
   coordinator bounce resets it, so on the next ingest tick each app
   runs through `AddAppToBucket(default, app)` unconditionally.
   Apps that had been split out of `default` get put back in,
   alongside their existing dedicated bucket. This is what keeps
   feeding fresh "dominant app" candidates to `EvaluateSplit` —
   without it, the first pass would split and the ratchet would
   stop.

3. **Ratio-only predicate cascades.** `EvaluateSplit`'s only gate is
   `ratio > SplitThreshold` (0.6). After one app is carved out, a
   different app can dominate the REMAINING volume and trigger
   another split. No absolute-volume floor, no cooldown, no
   awareness that the app already has a dedicated bucket.

## Requirements before re-enabling

- **Split must move capacity, not inflate it.** Some sensible
  shape: `dedicated.Target = min(src.Target, policy-chosen)`;
  `src.Target -= dedicated.Target`. The tier's total `Σ
  bucket.target` must be invariant across a split.

- **`OnboardApp` must respect existing dedicated buckets.** When a
  new heartbeat arrives for an app that already has a dedicated
  bucket, the app belongs in that bucket, not `default`. Options:
  a DB-side lookup by app → bucket in `OnboardApp`, or moving the
  dedup out of in-memory `SeenApps` into the DB so a restart
  doesn't forget.

- **`EvaluateSplit` must skip apps that already own a dedicated
  bucket.** Defensive — even if (2) is fixed, belt-and-braces.

- **Cooldown or budget.** Prevent cascading splits inside a single
  reconcile window. Suggested: at most one split per tick, and no
  second split on the same source bucket until ingestion stats have
  been re-observed for ≥ some minimum period.

- **Absolute-volume floor.** Ratio-only dominance on a low-volume
  bucket is noise. Add a minimum total-row threshold before a
  bucket is eligible for splitting.

## Rough shape of a working policy

```
ShouldSplit(bucket, stats):
  if total(stats, bucket.Apps) < MinRowsForSplit: return false
  for app, count in stats[bucket.Apps]:
    if bucket.hasDedicated(app):                 return false  // already split
    if count / total > SplitThreshold:           return true, app
  return false

Split(app):
  src := bucketOf(app)
  move := min(src.Target, MaxMovePerSplit)
  dedicated := NewBucket{Target: move, Tier: src.Tier, ...}
  src.Target -= move
  save both in one txn
```

Exact constants (`MinRowsForSplit`, `MaxMovePerSplit`, cooldown
interval) are policy knobs to tune once we have real ingestion
data. Day-1 values could be derived from the observed volume of the
noisy neighbours that motivated splitting in the first place.

## Testing

Once re-enabled, the regression contract is:

- `Σ bucket.target(tier) == constant across Split(*)` on any tier.
- After `OnboardApp(X)` followed by a coordinator restart and
  another `OnboardApp(X)`, app X is in exactly one bucket.
- A bucket with a single dedicated app never re-enters
  `EvaluateSplit`.
- No single tick produces more than one split, no matter how skewed
  the stats.

Each bullet is a direct-unit-test target.
