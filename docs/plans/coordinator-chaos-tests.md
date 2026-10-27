# Coordinator N=2 — remaining chaos tests

`coordinator_desired = 2` has been live in prod since 2026-09-19. Of the
eight chaos scenarios written for it, six were run or retired on
2026-09-19 (NAT re-election, cron double-fire, TailFanout under Redis
disruption, slow-shard convergence stall and steady-state load passed, with
the gaps they surfaced recorded in `docs/known-issues.md`; poller
double-ingest was skipped because `coordinator/poller` no longer exists).
Two were never run and are kept here. Each has an **action**, **expected
behavior** and **PASS/FAIL criteria**. Run against prod directly, no
isolated environment (Paul's explicit call).

## Before running anything

- **Holder identity is the EC2 instance-id, stable across process
  restarts.** A bare `pkill -9 -f weft-coordinator` is not a failover test:
  `Restart=always`/`RestartSec=5` brings the process back in ~6s and it
  reclaims its own leases (`AcquireLease`'s `holder = self` branch) before
  a healthy peer's 10s retry tick can win them.
- **`sudo systemctl stop weft-coordinator` works, slowly.** `GracefulStop()`
  does not return within systemd's 90s `TimeoutStopSec` (see
  `docs/known-issues.md`), so the unit is force-killed, and since a stop was
  requested systemd does not restart it. That is a real ~90s holder outage.
  Add `&& sudo systemctl mask weft-coordinator` to hold the instance down,
  and `unmask` + `start` to restore it. An outage that long also lets the
  coordinator ASG's health check replace the instance on its own, so check
  `describe-auto-scaling-groups` before assuming your manual recovery is
  what keeps the fleet at 2.
- **No `kevcons` command mutates leases.** The only levers are killing the
  holding process, or editing `leases` through `kevcons db query`, which
  opens at most a `LeaseRenewInterval` (10s) window before a live holder
  reclaims it.
- **Always pass `--instance-id` to `kevcons ssh`.** With two coordinators
  and no `--instance-id` it picks `connectedInstanceIDs[0]`, which is not
  deterministic.

**Find the coordinators:**
```
kevcons aws autoscaling describe-auto-scaling-groups --auto-scaling-group-names weft-coordinator
```

**`leases` table** (`store/schema.sql`): `name` (PK), `holder` (the
coordinator's instance-id, identical to the `coordinator_id` telemetry
field), `expires_at`, `acquired_at`. `LeaseTTL = 30s`,
`LeaseRenewInterval = 10s`. Names in play: `nat-holder`, `shard-fsm-leader`,
`cron:<cron_watches.id>`.
```sql
SELECT name, holder, expires_at, acquired_at FROM leases ORDER BY name;
```

**Live monitoring** (one loop per instance; logs are per-host files):
```
for id in <coord-a-id> <coord-b-id>; do
  ( while true; do
      kevcons ssh --instance-id "$id" --command \
        'sudo tail -n 300 /opt/app/logs/coordinator.log' \
        | grep -E "lease_event|evaluate_event|evaluate_tick"
      sleep 15
    done ) &
done
```

---

## Test 1 — shard-fsm-leader flap under load

**Baseline**: `SELECT * FROM leases WHERE name = 'shard-fsm-leader';`

**Action**: take down the current leader (see "Before running anything" —
`systemctl stop` + `mask`, since this tests cross-instance failover, not
restart recovery) —
`evaluateStateMachines()` runs all shard `Evaluate` calls sequentially in
one loop (not per-shard goroutines), so killing at any moment has a real
chance of landing mid-tick; no special timing needed.

**Expected**:
1. `evaluate_tick`/`evaluate_event` lines stop flowing from the killed
   instance immediately.
2. Deadman's switch: if the *survivor* were the stale one it would
   self-release before renewing (`progressStale`: no progress for >
   2×`PollInterval` = 2 minutes) — not the expected path here, since the
   kill target is the leader itself, not a stalled one. The expected path
   is simple TTL expiry: once `shard-fsm-leader`'s `expires_at` passes,
   the survivor's own `leaderTick` (10s cadence) wins `AcquireLease`.
3. Survivor logs `provisioner: acquired shard-fsm-leader lease` and
   `lease_event ... event=acquired`; `evaluate_tick` lines resume from
   the survivor's `coordinator_id`.
4. `EC2Provisioner.Launch` checks for a live instance already tagged with
   the target `shard_id` before calling `RunInstances`, so a leadership
   handoff mid-launch should not mint a duplicate.

**PASS if**: re-election completes within ~40s; `evaluate_tick` lines
resume without gap beyond that window; no shard ever shows two
concurrent EC2 instances tagged with the same `shard_id`:
```
kevcons aws ec2 describe-instances --filters "Name=tag:shard_id,Values=<id>" "Name=instance-state-name,Values=pending,running"
```
should return exactly one instance, before and after.

**FAIL signals**: no re-election within ~2×`LeaseTTL`; two live instances
for the same `shard_id`; any shard's persisted state showing an
obviously torn write.


## Test 2 — Scale event (`coordinator_desired` 2→3→2)

**Note**: scaling to 3 requires bumping `max_capacity` too.
`infrastructure-catalog/stacks/asg-weft-application/main.tf`'s
`coordinator_asg` module call hardcodes `max_capacity = 2`, so a bare
`coordinator_desired = 3` edit fails validation. Both are protected IaC
applies: `terraform plan` first, human-run `apply`.

**Action**:
1. Edit `infrastructure-live/prod/weft/terraform.tfvars`:
   `coordinator_desired = 3`; bump `max_capacity` in the `coordinator_asg`
   block; `plan` then `apply`.
2. Wait for the third instance to reach `InService` and rejoin (~1-2 ticks).
3. Revert both edits (`coordinator_desired = 2`, `max_capacity` back down);
   `plan` then `apply`.

**PASS if**:
- After 2→3: three `InService` coordinators, and `nat-holder` and
  `shard-fsm-leader` each show exactly one live (non-expired) holder:
  ```sql
  SELECT name, holder, expires_at FROM leases WHERE name IN ('nat-holder','shard-fsm-leader');
  ```
- After 3→2: any `nat-holder`/`shard-fsm-leader`/`cron:*` lease the
  terminated instance held is reclaimed by a survivor within one tick, and
  no lease row is left held by an instance that no longer exists:
  ```sql
  SELECT * FROM leases WHERE expires_at < now();
  ```

**FAIL signals**: two coordinators both driving `evaluateStateMachines`
(overlapping `evaluate_tick` lines from two different `coordinator_id`s),
which is true split-brain, or an orphaned lease row that never expires out.
