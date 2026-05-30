# Bucket WARMING re-mint gap

## The bug

`BucketSM.evaluateWarming` (in `logdb/bucketsm.go`) is **gate-only**.
It checks whether the warming cohort has reached
`ingesting == total && ingesting >= Target` and, if so, fires
`TriggerAllReplacementsReady` to advance WARMING → ROTATING. It never
mints new shards.

Minting only happens in `onEnterWarming`, the state-entry hook. So
once the bucket has entered WARMING with a cohort of N stripes, that
cohort is fixed for the lifetime of WARMING. If one of those stripes
gets permanently stuck — e.g. the LAUNCHING instance dies before
heartbeat and the row is later DELETEd per the §4 cleanup playbook —
the cohort shrinks below Target and the bucket sits in WARMING
forever:

- `total` and `ingesting` both drop to the live count (say 1)
- gate is `1 > 0 && 1 == 1 && 1 < Target(2)` → never fires
- next tick: same gate, same outcome, indefinitely

## How we hit it

Post-IR cleanup after the 2026-05-29 shard-roll deploy: the IR killed
3 shards mid-launch. `ad3060eb` recovered (the FSM's SUSPENDED-reap
path handled it). The other two (`c7677161`, `94563133`) sat in
LAUNCHING with dead InstanceIDs; per `cleanup.md` §4 I DELETEd both.
The bucket should then have minted a replacement to get back to
Target=2 — instead it sat in WARMING for ~30 min before I noticed
and force-flipped `buckets.state = 'STASIS'` to kick `evaluateStasis`
into action.

## The fix (sketch)

`evaluateWarming` needs a re-mint branch parallel to
`evaluateStasis`'s scale-up:

```go
// after the gate check, before returning nil:
if total < b.Target && launchingCur == 0 {
    // shrink-due-to-loss; re-mint missing stripes in the
    // warming window so the gate can eventually fire.
    return b.reMintMissingStripes(ctx, warmStart, warmEnd)
}
```

`reMintMissingStripes` is essentially the fill branch of
`onEnterWarming` factored out — compute `covered := stripeSet(live,
warmStart)`, mint for each i in `0..Target-1` not covered.
`onEnterWarming` itself documents the operation as idempotent
(line 193 comment), so the factor-out is mechanical.

The recovery path I used (manual `UPDATE buckets SET state='STASIS'`)
is a fine break-glass but doesn't belong in the normal cleanup
playbook — Phase-N FSM hardening should land this re-mint so a
DELETE under §4 self-heals on the next tick as `cleanup.md` already
claims it will.

## Cleanup.md alignment

`cleanup.md` §4 promises:

> The bucket SM sees `ingesting < Target` on the next tick (5 min in
> prod) and mints a fresh replacement from the current AMI under FSM
> control.

That statement is true only when the bucket is in STASIS at delete
time. Either: (a) fix `evaluateWarming` per above so the statement
holds universally, or (b) extend §4 with a "if bucket.state =
WARMING, also flip to STASIS first" step. (a) is the right fix.
