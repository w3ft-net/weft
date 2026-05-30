# roll-shard heartbeat race — silent no-op

## The bug

`weft-admin roll-shard <id>` (and therefore `roll-cohort` /
`roll-all`) reports success without actually rolling the shard. The
instance is not terminated, the volume is not re-attached, and the
binary is not refreshed — same EC2 instance, same addr, same
`binary_sha` before and after. The shard merely transits INGESTING →
SUSPENDED → INGESTING in ~30–60s.

## Why

`Provisioner.RelaunchShard` (in `logdb/provisioner.go`) fires
`TriggerRelaunchRequested`. The shard SM enters SUSPENDED, runs
`onEnterSuspended`, which:

```go
sh.Addr = ""
sh.Live = nil
return nil
```

Pure-stateless by design — no AWS side effects. The doc comment says:

> Evaluate must reap before relaunching. (Pure: no AWS side effects
> here; the reap + relaunch is Evaluate's job, so a restart that lands
> in SUSPENDED re-drives it.)

The reap (`Launcher.Terminate(deadID)`) is inside `evaluateSuspended`,
which runs on the 5-min reconciler tick.

Meanwhile, the EC2 instance is **still running**. The weft-shard
process re-establishes its gRPC connection and heartbeats. The
heartbeat handler dispatches `TriggerHeartbeatReceived`, gated by
`heartbeatMatchesSuspended`:

```go
func (sh *Shard) heartbeatMatchesSuspended(_ context.Context, args ...any) bool {
    hb, ok := heartbeatArg(args)
    if !ok { return false }
    return hb.ShardID == sh.ID && hb.Addr != ""
}
```

This gate **does not require** `InstanceID == ""` (i.e., proof that
the reap has happened). So the still-alive process trivially satisfies
the gate, the SM promotes SUSPENDED → INGESTING, and
`evaluateSuspended` never runs.

Sequence observed in prod 2026-05-30 03:59:34–04:00:22:

```
03:59:34  RelaunchShard RPC → TriggerRelaunchRequested
03:59:34  shardsm a13e6f03: enter SUSPENDED — connection torn down; Evaluate will reap i-08eb0fc3392d03664 and relaunch
03:59:34  provisioner: relaunch requested for a13e6f03 (was INGESTING); driving via SUSPENDED → LAUNCHING
…         shard process reconnects, sends heartbeat
04:00:22  shardsm a13e6f03: SUSPENDED → INGESTING   ← BUG: never reaped, never relaunched
```

`shard_records` after: same `instance_id`, same `addr`, same
`binary_sha`. The `--target-sha` skip-if-current logic in weft-admin's
`shardsToRoll` would even fire on the next roll, treating the shard
as already-current despite never having been rolled.

## Why HeartbeatStale doesn't hit this

The other path into SUSPENDED — `evaluateActive` firing
`TriggerHeartbeatStale` — happens when heartbeats have *stopped*
(instance dead, network gone, etc.). By the time SUSPENDED is
entered, there are no more heartbeats to race the reap. So
`heartbeatMatchesSuspended`'s missing InstanceID check is harmless
there. It's only the manual `RelaunchShard` path against a healthy
shard that exposes the race.

## Fixes (any of these)

### A. Gate heartbeatMatchesSuspended on reap completion (recommended)

```go
func (sh *Shard) heartbeatMatchesSuspended(_ context.Context, args ...any) bool {
    hb, ok := heartbeatArg(args)
    if !ok { return false }
    sh.mu.Lock()
    instanceID := sh.InstanceID
    sh.mu.Unlock()
    if instanceID != "" {
        return false // pre-reap heartbeat; wait for Evaluate to reap + relaunch
    }
    return hb.ShardID == sh.ID && hb.Addr != ""
}
```

After `evaluateSuspended` reaps, it calls `SetShardLaunchResult(sh.ID,
"", az, volumeID, stripeIndex)` to clear InstanceID — so the gate
opens once recovery has moved to LAUNCHING. The replacement instance's
first heartbeat (with `InstanceID` now empty in the SM, since launch
hasn't stamped a new one yet) goes through cleanly. This is minimal
and matches the intent in the existing doc comment ("the dead instance
Evaluate must reap before relaunching").

### B. Synchronously reap in onEnterSuspended (when entered via RelaunchRequested)

Distinguish recovery (instance already dead) from roll (instance
alive). RelaunchShard fires `TriggerRelaunchRequested`; the path
HeartbeatStale fires `TriggerHeartbeatStale`. If we route them to
different handlers in `Permit`, the RelaunchRequested handler can call
`Launcher.Terminate` synchronously. Side effects in OnEntry break the
"pure" design constraint quoted above, though, so this is the more
invasive fix.

### C. Have RelaunchShard kill the shard process first (worst option)

Hit `/shutdown` over HTTP from the coordinator before firing the
trigger. Adds a synchronous network round-trip to the roll path,
duplicates state management between weftd and the FSM, and breaks the
"single source of truth" design. Don't do this.

## Knock-on: kevcons deploy

If kevcons had successfully invoked `weft-admin-run roll-all` at deploy
end (it didn't — see `cleanup.md` §1 / the deploy that hit "completed
with errors"), every shard would have rolled to "success" without
actually picking up the new binary. The Phase-3 `--target-sha` check
that promised "skip if current" would not have caught it either —
because the shard's `binary_sha` is whatever was stamped when its real
launch happened, not what was claimed by the roll-shard no-op.

Net: the deploy was *doubly* broken. kevcons didn't fire roll-all, and
roll-all would not have done what it claimed even if it had fired.

## Repro

In a sandbox shard (or against any INGESTING shard):

```
ID=<some INGESTING shard id>
BEFORE=$(kevcons db query "SELECT instance_id, addr, binary_sha FROM shard_records WHERE id='$ID';")
kevcons ssh --command "sudo /opt/app/bin/weft-admin-run roll-shard $ID"
sleep 60
AFTER=$(kevcons db query "SELECT instance_id, addr, binary_sha FROM shard_records WHERE id='$ID';")
diff <(echo "$BEFORE") <(echo "$AFTER")
# expected post-fix: instance_id and addr differ
# observed today: empty diff
```
