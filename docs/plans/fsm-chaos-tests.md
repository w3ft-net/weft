# FSM-backed shard management — chaos tests

Exercises the per-AZ ASGProvisioner + shard FSM in prod against the
running cohort after the 2026-05-29 shard-roll deploy. Each test has
**(a)** the action, **(b)** what we expect the FSM to do, and **(c)**
acceptance criteria. Tests are ordered safest → most destructive; each
leaves the system in the same baseline state (2 INGESTING, both on
`c52ee15`) so the next can run.

Baseline at test start:
- bucket 1 (default, small, target=2, STASIS)
- `ad3060eb` INGESTING us-east-2b on `i-0877d9675aa0bcbaa`
- `a13e6f03` INGESTING us-east-2a on `i-08eb0fc3392d03664`

## Test 1 — roll-shard (single, in-place)

**Action**: `weft-admin-run roll-shard <id>` on `a13e6f03`.

**Expected**:
1. coordinator log: `provisioner: relaunch requested for <id> (was INGESTING); driving via SUSPENDED → LAUNCHING`
2. shardsm transition INGESTING → SUSPENDED (TriggerRelaunchRequested)
3. SUSPENDED.Evaluate reaps the dead instance, transitions to LAUNCHING
4. asg-provisioner launches a *new* instance on `weft-shard-small`,
   re-attaching the same EBS volume (`DeleteOnTermination=false`)
5. shard reads `shard.json` on the reattached volume, christens with
   same id, heartbeats
6. promote path: LAUNCHING → INGESTING
7. `binary_sha` re-stamped to `c52ee15` (same SHA — but the post-relaunch
   heartbeat after the cutoff is what `wait-and-verify` actually checks)

**PASS if**:
- shard returns to INGESTING within `--per-shard-timeout` (10m default)
- new `instance_id` ≠ old, new `addr` ≠ old (fresh boot)
- `row_count` is preserved (volume reattached, data intact)
- the *other* shard never leaves INGESTING (per-shard roll, not cohort)

**FAIL signals**:
- shard stuck in LAUNCHING > 5m after the asg-provisioner launch line
- `row_count` resets to 0 (volume *not* reattached — would mean
  DeleteOnTermination behaved wrong)
- both shards SUSPENDED simultaneously (would mean we hit a global
  registry race)

## Test 2 — roll-all

**Action**: `weft-admin-run roll-all` (no `--target-sha`).

**Expected**: Same as Test 1, sequentially across both shards. Each
waits to reach INGESTING before the next fires.

**PASS if**:
- Both shards rotate to fresh instances + new addrs
- ingest continuity: at any moment ≥1 INGESTING shard (the sequential
  ordering guarantees this)
- exit code 0; both shards INGESTING with fresh heartbeats

**Why this matters**: kevcons was supposed to invoke this at the end of
the deploy and didn't (that was the "completed with errors" — see
`cleanup.md` §1). Running it manually here proves the binary side works,
isolating the bug to kevcons.

## Test 3 — Scale up / down (target 2→3→2)

**Action**:
```
kevcons db query "UPDATE buckets SET target_ingesting_count = 3 WHERE id = 1;"
# wait one bucket tick (5m)
kevcons db query "UPDATE buckets SET target_ingesting_count = 2 WHERE id = 1;"
```

**Expected**:
1. **Scale up**: STASIS Evaluate sees `ingesting(2) < Target(3) &&
   launchingCur(0) == 0` → fires `TriggerScaleUpRequested` → WARMING →
   `onEnterWarming` mints stripe 2 in current window. asg-provisioner
   launches a 3rd instance. WARMING → ROTATING (via the
   `evaluateWarming` gate, once the new shard is INGESTING) → STASIS.
2. **Scale down**: STASIS Evaluate sees `ingesting(3) > Target(2)` →
   fires `TriggerScaleDownRequested` → SCALING_DOWN → `onEnterScalingDown`
   rotates the excess (RotateRequested on the latest-added shard's
   stripe) → READ_ONLY. Eventually the new READ_ONLY shard's retention
   expires → ARCHIVING → ARCHIVED.

**PASS if**:
- Scale-up brings up a third INGESTING shard within 1–2 ticks (~5–10m)
- Scale-down rotates ingesting count back to 2 without disturbing the
  original two
- bucket transits STASIS → WARMING → ROTATING → STASIS → SCALING_DOWN
  → STASIS

**FAIL signals**:
- WARMING hangs (the gap we just patched around — should not recur
  since this is a STASIS-driven mint, not a re-mint inside WARMING)
- SCALING_DOWN rotates one of the *original* shards (it should pick
  the newest stripe)

## Test 4 — pkill weft-shard

**Action**: Pick the shard with the smaller `row_count`. SSH and:
`sudo systemctl stop weft-shard` (or `kill -9 $(pgrep weft-shard)` if
systemd has been swapped out).

**Expected**:
- Two phases possible:
  - **(A) systemd restarts the unit** within seconds; heartbeat resumes;
    FSM never observes a stale heartbeat; no state transition.
  - **(B) systemd doesn't restart** (or weft-shard fails to restart):
    heartbeat stops, `actuallyStale` fires after the boot-anchor +
    stale window, → SUSPENDED → reap (terminate instance) → LAUNCHING
    → new instance + reattached volume → INGESTING.

We don't pre-know which (systemd config is what it is in prod). Either
outcome is acceptable; the test reveals which we have.

**PASS if**: shard is back to INGESTING within 10m, EITHER via systemd
auto-restart (no FSM transition) OR via full SUSPENDED-reap.

**FAIL signals**:
- Process never comes back AND FSM doesn't notice for > 10m (would mean
  HeartbeatStale isn't being fired or stale window is too long)

## Test 5 — EC2-terminate directly (NOT via ASG)

**Action**: Pick the shard with the smaller `row_count` (so cleanup is
quick). Via EC2:
```
kevcons aws ec2 terminate-instances --instance-ids <iid>
```

**Expected**:
1. Instance disappears from EC2
2. heartbeat stops
3. shardsm Evaluate sees stale heartbeat → SUSPENDED (via
   `HeartbeatStale`)
4. SUSPENDED.Evaluate: nothing to reap (instance already gone), proceed
   to LAUNCHING via `RelaunchReady`
5. asg-provisioner launches a fresh instance, reattaches the EBS volume
   in the same AZ (DeleteOnTermination=false → volume survives)
6. shard reads `shard.json`, heartbeats
7. LAUNCHING → INGESTING with same id + preserved row_count

**PASS if**:
- shard recovers to INGESTING within 10m
- `row_count` is preserved (volume reattached)
- ASG `Desired` matches what FSM wants (1 in us-east-2a OR us-east-2b
  depending on which we picked)

**FAIL signals**:
- ASG launches a replacement instance *itself* (would mean MinSize > 0
  or ReplaceUnhealthy is firing — Phase 5 of shard-roll plan is
  supposed to neutralize these)
- Fresh shard comes up with `row_count = 0` (volume not reattached;
  DeleteOnTermination behaved wrong, or AZ-mismatch)
- shard stuck SUSPENDED > 10m (Evaluate not firing or reap-path bug)

## Live monitoring (all tests)

Run this in parallel to capture FSM transitions:
```
LAST=""
while true; do
  kevcons ssh --command 'sudo tail -300 /opt/app/logs/coordinator.log' \
    | awk '/^--- Command Output ---/{p=1; next} p' \
    | grep -E "provisioner:|shardsm |bucketsm |asg-provisioner|relaunch|launched|mint|reap|Trigger|enter (SUSPENDED|LAUNCHING|INGESTING|READ_ONLY|ARCHIV|WARMING|STASIS|ROTATING)"
  sleep 30
done
```

DB snapshot (re-run per test for before/after):
```
kevcons db query "SELECT id, state, instance_id, addr, row_count, NOW() - last_heartbeat_at AS hb_age, binary_sha FROM shard_records WHERE state NOT IN ('ARCHIVED','GONE') ORDER BY created_at;"
```
