# Plan: orphan-christen bug — root-cause and fix

A spam of two related errors in prod's `coordinator.log`:

```
lifecycle: handle i-X: no LAUNCHING row for tier small; instance i-X is an orphan
provisioner: christen ADDR: no LAUNCHING row for instance i-Y — reconciler/ASG out of sync
```

The first means an ASG-launched instance reached the `Pending:Wait`
lifecycle hook and `ClaimLaunchingForTier(tier, instanceID)` returned
`(zero, false)` — no unclaimed LAUNCHING row exists for the tier. The
hook ABANDONs, ASG terminates the instance.

The second means a shard-binary heartbeat arrived from an instance
whose `instance_id` does not match any LAUNCHING row's `instance_id`.
The provisioner can't christen, so the shard never leaves blank
state. Subsequent heartbeats from that instance keep retrying
christen and keep failing.

Downstream effects:

- `christen-failure-rate-trend` cron pages every 10 min.
- `shard.json` never gets written, so `$NODE` stays NULL — the
  signal `shards-without-node` was supposed to surface.
- Heartbeat-snapshot rows pile up under `weft/shard/heartbeats` for
  blank shards that never become INGESTING.
- Ingest writes have nowhere to land for buckets whose target shards
  never christen.

Tracked separately from the alerting work because the fix lives in
the provisioner / lifecycle / reconciler triangle, not the cron
config.

## What we know about the lifecycle

```
reconciler tick (every 30s)              lifecycle hook (per launch)               provisioner.christen (per heartbeat)
─────────────────────────────            ──────────────────────────────             ─────────────────────────────────────
1. Read bucket.Target from DB            1. ClaimLaunchingForTier(tier, instID)    1. heartbeat from blank shard arrives
2. Read LaunchingCount + IngestingCount     → claim oldest LAUNCHING row              with addr + instID
3. If under target, mint LAUNCHING       2. tag claim with instance_id             2. FindLaunchingByInstance(instID)
   rows (provisionForBucket)             3. CreateForShard volume                  3. dial shard, send Configure
4. syncASGDesiredCapacity(tier, target)  4. CompleteLifecycleAction(CONTINUE)      4. on success, registry.Add(entry)
5. ASG observes desired_capacity bump,
   launches an instance
```

A breakage anywhere in steps 1 → 4 (reconciler) or 2 (lifecycle) or
2 (christen) produces the orphan we see in prod.

## Hypotheses ranked by likelihood

### H1. No bucket exists with `label="default"`

`Buckets.Default()` returns `(nil, nil)` when no bucket row has
that label. `OnboardApp(app)` short-circuits with a log line and
never adds the app. Reconciler iterates `Buckets.All()`; if it's
empty, `targetByTier` is empty, `desired = 0` for every tier, and
no LAUNCHING rows ever get minted.

**Evidence**: searching `coordinator.log` for `provisioner:` and
`reconciler:` lines returns nothing in the 5000-line tail —
expected if the reconciler has nothing to do.

**But the orphan log says instances are launching** — so something
is bumping `desired_capacity` despite the reconciler being silent.
Candidates:
- A leftover from an earlier deploy where `desired_capacity` was
  set to `>0` and ASG hasn't been ratcheted back.
- Manual operator action (`aws autoscaling set-desired-capacity`).
- A previous deploy that minted rows that have since been reaped.

Once the ASG has `desired > 0` and no LAUNCHING rows exist, every
launch becomes an orphan in the steady state.

**Fix shape**: seed a default bucket on first coordinator boot if
none exists (idempotent, log if newly created). Set its `Target`
to `1` so a single shard comes up and starts accepting traffic.

### H2. Default bucket exists but its `Target` is 0

Same outcome as H1 from the reconciler's POV — `Op 2` (under-
capacity) iterates `effective` to `Bucket.Target`, which is 0,
so no LaunchShardOps emit. ASG `desired_capacity` set to 0 too
(by `syncASGDesiredCapacity`), but if it was previously set
higher and ASG ratchets down slowly, lingering instances can
race the ratchet.

**Fix shape**: same as H1 plus ensure the default bucket's
`Target` defaults to `1` on creation. A bucket with target=0 is
almost never what the operator wants.

### H3. Reaper kills LAUNCHING rows before instance heartbeats

`staleLaunchingAge = 10*time.Minute` (`provisioner.go:62`). If an
instance takes >10 min from ASG launch to first heartbeat, the
reaper deletes the row, the instance heartbeats, christen looks
up the instance_id, no row, fail.

**Evidence**: shard AMI builds can be slow; first boot after a
new AMI takes longer than warm boots. We've also been deploying
frequently, so reaping windows have been compressed.

**Fix shape**: bump `staleLaunchingAge` to `30*time.Minute` (or
make it per-tier, since "large" instances may legitimately take
longer). Add a log line when the reaper fires so we can spot
this in the future.

### H4. Reconciler isn't ticking

The provisioner's tick goroutine starts in `Start(ctx)`. If
`Start` isn't called or the goroutine panicked early, no
reconciler runs ever. `adoptFromHeartbeats`, `reapDeadShards`,
`reapStaleLaunching`, and `check` all share that goroutine.

**Evidence to gather**: look for any one of those four functions'
log lines in coordinator.log over a longer window than 5000
tail. If absent, the goroutine never started or died.

**Fix shape**: wrap the tick body in a `defer recover()` that
logs and re-spawns. Add a heartbeat log line ("provisioner:
tick complete in N ms") at INFO so silence is loud.

### H5. Bucket targets exist but ASG has stale `desired_capacity`

`syncASGDesiredCapacity` should converge desired to `Σ targets`.
If `SetDesiredCapacity` is failing silently for some tiers,
desired stays at whatever was last set successfully. Errors are
logged at WARN — we'd see `provisioner: sync desired:
SetDesiredCapacity(small, N): ...` lines.

**Evidence to gather**: filter coordinator.log for
`SetDesiredCapacity`. If recent entries say "AccessDenied" or
similar, the role is missing the `autoscaling:SetDesiredCapacity`
permission, and the reconciler can't drive the fleet. This is
plausible after recent IAM changes.

**Fix shape**: confirm IAM. If broken, fix in the TF instance-
role module and redeploy. Add a startup self-check that calls
`DescribeAutoScalingGroups + SetDesiredCapacity(current)` once
at boot so a permission gap surfaces immediately, not on first
reconcile attempt.

### H6. Race: hook claims row → row deleted before christen

The hook's `ClaimLaunchingForTier` succeeds, the volume gets
created, the instance comes up. Meanwhile the reaper sees the
row has been claimed but the instance hasn't heartbeated yet, so
it's not "stale" — but if the row is somehow deleted in between
(another tick's reconcile? a manual `DELETE`?), christen fails
at lookup time.

This is the explanation that fits the second error message
("provisioner: christen X: no LAUNCHING row for instance Y") —
the instance_id was successfully stamped on a row, but the row
is gone by the time christen looks it up.

**Fix shape**: `FindLaunchingByInstance` should also look in a
tombstone table (or a soft-delete column) so christen can give
a clearer "your row was reaped before you came up" error. But
the real fix is figuring out *who* deletes a claimed row. If
nothing should — the reaper only deletes rows beyond
`staleLaunchingAge`, and only those with empty instance_id (per
`reapStaleLaunching`'s first branch) — then this hypothesis
points to a bug in the reaper's filter.

## Investigation steps

Before writing any code:

1. **Check the buckets table on prod** — `kevcons db tunnel -- psql
   -c 'SELECT id, label, apps, target, tier FROM buckets'`. Is
   there a row with label='default'? What's its target?
2. **Check the shard_records table** — `SELECT id, state,
   instance_id, tier, created_at FROM shard_records WHERE state =
   'LAUNCHING' ORDER BY created_at DESC LIMIT 20`. Any rows?
   Created when? Have instance_ids?
3. **Check ASG desired_capacity** — `kevcons aws autoscaling
   describe-auto-scaling-groups --auto-scaling-group-names
   weft-shard-small --query
   'AutoScalingGroups[].[DesiredCapacity,MinSize,MaxSize]'`.
   Is desired > 0 with min=0?
4. **Look for reconciler heartbeat in a longer log window** —
   1 hour worth: `aws logs filter-log-events ... --filter-pattern
   "reconciler:"`. Truly silent, or just below the 5000-line tail?
5. **Look for SetDesiredCapacity errors** — same logs, filter
   `SetDesiredCapacity`.
6. **Check IAM** — confirm the coordinator's instance-role has
   `autoscaling:SetDesiredCapacity` and
   `autoscaling:DescribeAutoScalingGroups` for each shard ASG.

The investigation result determines which hypothesis is the actual
cause, and only one or two need code changes.

## Fix surface (depending on investigation)

| Hypothesis | Code touched | Effort |
|---|---|---|
| H1, H2 | new `coordinator/seed_buckets.go` + `cmd/coordinator/main.go` boot wiring | 0.25 day |
| H3 | `coordinator/provisioner.go` (`staleLaunchingAge` per-tier, log line on reap) | 0.25 day |
| H4 | `coordinator/provisioner.go` (panic-recover + tick heartbeat log) | 0.25 day |
| H5 | TF instance-role module (`infrastructure-catalog/modules/asg-application/iam.tf`) + a coordinator boot self-check | 0.5 day |
| H6 | `coordinator/provisioner.go` (`reapStaleLaunching` audit + tombstone table or NOT EXISTS guard) | 0.5 day |

Total: 1–2 days depending on how many hypotheses turn out to be
in play (likely H1+H4 together).

## Validation

- After deploy: `coordinator.log` shows `reconciler: ...` lines on
  every tick (or at least every tick where work happens).
- `christen-failure-rate-trend` cron stops paging within 30 min.
- `shards-without-node` cron drops to 0 — every metrics row gets
  a non-empty `_node`.
- `weft sh -c 'tail -n 1 /log/weft/shard/heartbeats.log'` shows
  rows whose `instance` and `service` columns are populated and
  consistent with the registry.
- `kevcons db tunnel -- psql -c 'SELECT id, state, instance_id
  FROM shard_records'` shows zero LAUNCHING rows older than
  ~1 minute (hot launches; reaper hasn't kicked in yet).

## Out of scope

- **Heartbeat-based dead-shard reaping** (TODO.md item 7). Once
  this fix lands, dead-shard removal becomes the next-most-pressing
  registry hygiene item, but it's its own design.
- **ASGLauncher removal from bootstrap** (TODO.md item 9). The
  current bug is upstream of that refactor; the launcher's
  `LaunchInstance` path isn't in play during steady state.
- **Bucket-split heuristics** for high-throughput apps. Today
  every app lands in `default`. Splitting is a Phase 2 product
  concern.

## Open questions

- Should the default bucket be created at coordinator boot, or by
  the first `OnboardApp` call (lazy)? Boot-time is more
  predictable; lazy avoids creating buckets for environments that
  never see traffic.
- For H3 (reaper too aggressive), does AWS Lambda warm-pool
  semantics change the picture for the "large" tier? Cold-pool
  large instances may legitimately take 5+ min to boot.
- For the per-tier reaper interval, should the tier be a config
  knob or a hardcoded map? Probably hardcoded — these aren't
  customer-tunable.
