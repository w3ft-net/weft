# Seamless shard upgrade: warm spares + pause/redirect

## Why

A shard roll (`weft-admin roll-*`, run by every deploy whose shard binary
SHA changed) breaks before it makes. UPGRADING tears the shard's connection
down at once, the old instance is reaped, and only then is a replacement
launched and the volume reattached. Traced in prod on 2026-10-23 for
`shard-20261019T0000-bc9344c4` (INGESTING):

| time (UTC) | event |
|---|---|
| 17:59:50 | enter UPGRADING; connection torn down: reads stop |
| 18:00:49 | old instance reaped; volume detaches |
| 18:02:15 | new instance launched with the volume reattached |
| 18:04–18:29 | the other coordinator, its registry still naming the old address, sends writes there: 634 DLQ'd batches (the DLQ is a no-op, so lost) |
| 18:21, 18:25 | FSM leader degrades it after 1222, then 1656, consecutive ingest failures |

Since 69be93fa a query over a shard that is UPGRADING or relaunching fails
(a missing shard is a catastrophe, not something to answer around), so
today every roll is also minutes of failed queries and crons.

What we want: a deploy upgrades a shard with the same id, window, volume and
data, never routed away from, never lost a row, and callers see a pause of
seconds as latency, not errors.

Rejected alternatives:

- **Seal and rotate** (mint a same-window replacement, rotate the old shard
  to READ_ONLY): build-first like window rotation, but it pushes a shard to
  READ_ONLY on every deploy, multiplying shards per window.
- **In-place binary handoff** (listener fd passing on the same instance):
  covers binary changes only, keeps instances on their original AMI, and
  makes "replace shard code over RPC" a capability.
- **Client-only pause**: every client (both coordinators, every compute
  worker) would have to stop and acknowledge before the old shard closes, a
  membership and ack protocol; the client can't learn the successor's
  address (compute has no registry) or know whether a call cut off by the
  shutdown was applied.
- **A standby per shard**: doubles the shard fleet. A volume belongs to an
  AZ, not a shard, so one spare per AZ serves any shard there.

## Mechanism

Two parts. A **warm pool** keeps booted, volume-less shard instances ready,
so no path waits on EC2. A **shard-enforced pause** makes the swap of a
volume from one instance to another a short pause instead of an outage, the
way pgbouncer's PAUSE works, with the shard as the single choke point for
its data.

An upgrade:

1. **Claim a spare** on the current AMI in the shard's AZ. The old instance
   keeps serving throughout. No spare available (the pool is still
   refilling, or another path took it): launch one on demand, the same as
   a spare but a few minutes slower.
2. **Pause.** The FSM leader tells the old shard: pause, your successor is
   at `addr`. Its server interceptor refuses every new call with a
   `paused` status carrying the successor address, and lets in-flight calls
   finish.
3. **Swap.** In-flight at zero: the old shard checkpoints, closes DuckDB,
   and the volume is unmounted. The leader detaches the volume and attaches
   it to the spare, which mounts it, opens DuckDB from `shard.json`, and
   starts admitting calls. The shard's `Addr` / `InstanceID` move to the new
   instance.
4. **Resume.** Held calls (below) land on the new instance. The old
   instance keeps refusing with the redirect until the new one has
   heartbeat and every coordinator's registry carries the new address, then
   it is terminated. The pool backfills.

Expected gap: unmount, detach, attach, mount, DuckDB open, roughly 10–30s.

## Warm pool

### Sizing: only for buckets that exist

One spare per (tier, AZ) pair some existing bucket uses. A bucket's stripe
`i` lands in `StripeAZ(tier, i)` (`i % len(AZsForTier(tier))`), so the
desired set is

```
{ (b.Tier, StripeAZ(b.Tier, i)) : b in buckets, 0 <= i < b.TargetIngestingCount }
∪ { (sh.Tier, sh.AZ) : sh a live shard }
```

The second term keeps a spare where an older shard still lives after a
bucket's Target shrank. A tier with no bucket costs nothing; deleting the
last bucket of a tier retires its spares.

### Spares

- Launched from the tier's launch template into the AZ's subnet, like a
  shard, but tagged `role=shard-spare`, `tier`, `az` (never `shard_id`, so
  `findLiveByShard` and the instance-correlation path ignore them).
- Recorded in a `shard_spares` table (instance id, tier, AZ, image id,
  launched at, claimed by, claimed at) so the pool survives a coordinator
  restart and a leader change. The reconciler cross-checks it against EC2
  (`DescribeInstances` by tag) and adopts or forgets rows that disagree.
- Boot in **spare mode**: the shard binary runs, mounts nothing, opens no
  database, heartbeats as a spare (instance id, tier, AZ, binary SHA), and
  refuses every data RPC with `not ready`. It is never a shard until a path
  claims it.

### Reconciler (FSM leader, every provisioner tick)

1. Compute the desired set; read the spares (table ∩ EC2).
2. Launch a spare for every desired pair without an unclaimed one.
3. **Stale AMI**: a spare whose image differs from the coordinator's own
   (the AMI shards launch from) is replaced, new first: launch the
   replacement, and terminate the stale spare once the new one heartbeats,
   so the pair is never uncovered. A deploy therefore refreshes the pool
   before `roll-all` needs it.
4. Terminate spares for pairs no longer desired.
5. A spare that never heartbeats within a launch budget is terminated and
   relaunched (same budget idea as `LaunchRetryBudget`).

### Claiming

A claim is one store write (`UPDATE shard_spares SET claimed_by = $shard,
claimed_at = now() WHERE instance_id = (SELECT … unclaimed, matching tier
and AZ, current image … LIMIT 1 FOR UPDATE SKIP LOCKED) RETURNING …`), so
two paths can't take the same spare. Three paths claim:

| path | today | with the pool |
|---|---|---|
| **upgrade** (UPGRADING) | reap, then launch, then reattach: minutes, reads and writes lost | claim, pause, swap the volume: seconds of held calls |
| **recovery** (SUSPENDED: the instance died) | reap, launch, boot, reattach: minutes | claim, force-detach the dead instance's volume, attach: the boot time is gone. Still an outage, which still fails queries, but shorter |
| **minting** (WARMING, fills, scale-up) | launch a fresh instance and volume, wait for boot | claim, create and attach a fresh volume, Configure |

Every path falls back to an on-demand launch when no spare is free, so the
pool only ever shortens a path, never blocks one. A claimed spare that the
path abandons (a rollback, a mint that failed) is terminated, not returned:
it may have had a volume attached.

## The interceptors

They do different jobs, not mirror ones.

**Server side (shard): correctness.** One unary + one stream interceptor on
every shard RPC:

- `serving`: pass through, counting in-flight calls.
- `paused`: refuse with a dedicated status (code + detail carrying the
  successor address) before the handler runs. A refused call was never
  applied, and for the streaming `Query` the refusal precedes the first
  message, so a retry can neither double-apply an `Ingest` nor double-send
  rows.
- `not ready` (a spare, or a claimed spare before its database is open):
  refuse until the volume is mounted and DuckDB open.

Only the shard knows when its in-flight count is zero, whatever the number
of clients or the state of their registries, which is why the pause lives
here.

**Client side (`shard.GRPCShard`): latency instead of errors.** Every
outbound shard connection is a `GRPCShard`: compute's reads
(`compute/resolver.go`) and the coordinator's writes and Configure (handles
from `logdb/provisioner.go`, `logdb/reconstruct_shard.go`). Heartbeats go
shard→coordinator and don't pass through it, so the spare can announce
itself during the pause.

- On `paused`: point the connection at the successor and retry until it
  admits the call. The repoint uses a per-`GRPCShard` manual resolver
  (`google.golang.org/grpc/resolver/manual`), so the address changes on the
  existing ClientConn.
- On `not ready` from the successor: keep holding.
- The hold is bounded (`pauseHoldTimeout`). Past it the call fails: a query
  with `compute.MissingRowsError`, consistent with 69be93fa.
- Compute's engine cache rebinds on the next descriptor whose `GrpcAddr`
  differs (`Engine.resolveShard`), and the coordinator's handle reconnects
  on the first heartbeat from the new address (`ObserveHeartbeat`), so the
  redirect is only needed for the interim.

## Rollback

The old instance is not terminated until the new one serves. If the claimed
spare never becomes ready (attach stuck, mount fails, DuckDB won't open,
timeout), the leader detaches the volume from it, reattaches it to the old
instance, which remounts, reopens and unpauses. Calls held against the
successor never got in, so their retries go back to the old address once the
redirect is withdrawn. The spare is terminated.

## FSM

UPGRADING stops meaning "routed away". It becomes a serving state with
persisted phases (mirrored onto the entry, like the export columns):

| phase | routing | done when |
|---|---|---|
| `claiming` | serving (old instance) | a spare is claimed and heartbeating (on-demand launch if none) |
| `pausing` | serving; calls held by interceptors | old shard reports in-flight 0 and closed |
| `swapping` | serving; calls held | the spare reports DuckDB open |
| `resumed` | serving (new instance) | every coordinator routes to the new address; old terminated → INGESTING / READ_ONLY / DEGRADED (the state it came from) |
| `rolling_back` | serving; calls held | old instance reopened and unpaused |

`queryable` treats UPGRADING as queryable again (69be93fa's
"UPGRADING: its instance is being replaced" goes away). A query during
`pausing` / `swapping` is routed normally and held by the interceptor.
SUSPENDED is unchanged: an instance that died is a real outage and fails
queries; the pool only shortens it.

Every phase is re-derivable after a coordinator restart or leader change:
the leader reads the phase, the claim row, the volume's attachment (EC2
`DescribeVolumes`) and the instances' heartbeats, and drives forward or
rolls back.

## Pieces to build

1. **Spare boot mode.** `deploy/startup.sh` mounts `/dev/sdf` at boot and,
   after ~60s with no volume, falls back to the root volume ("dev/container
   mode"), and the shard then starts blank awaiting Configure. A spare must
   be an explicit mode (from its `role=shard-spare` tag or user data),
   never that fallback:
   - the binary doesn't start a shard on the root volume; it heartbeats as a
     spare;
   - the shard runs as `app`, so mounting after a claim needs a root helper
     (a oneshot systemd unit the binary triggers, or a narrow sudoers rule),
     including the `mkfs` a fresh mint needs;
   - a spare's heartbeat must not reach `shardByInstance` /
     `promoteOnHeartbeat` until a claim binds it to a shard.
2. **Pool**: the `shard_spares` table (a migration above the schema's max),
   the reconciler, the claim, and the on-demand fallback in each path.
3. **Pause RPC + server interceptor** on the shard: `Pause(successor)`,
   `Resume()` (rollback), in-flight accounting, the refusal statuses, and
   the checkpoint / close / unmount sequence once drained.
4. **Client interceptor + manual resolver** in `shard/grpc_client.go`: hold,
   redirect, bounded wait.
5. **Leader orchestration**: the UPGRADING phases in the shard SM, plus
   recovery and minting through a claim, driving attach/detach,
   termination, and rollback.
6. **Drain isolation.** `StartDrain` is one goroutine writing to shards in
   sequence, so a held `Ingest` would stall every write on that coordinator.
   Each shard needs its own send path (a per-shard worker and queue) so a
   paused shard holds only its own rows.
7. **weft-admin**: `roll-*` waits for UPGRADING → back in the origin state
   with the target `binary_sha`, as today; only the meaning of the wait
   changes. With one spare per (tier, AZ), consecutive rolls in one AZ wait
   on the pool's backfill or fall back to on-demand launches; both work.

## Related hazards found along the way

- **The ring buffer overwrites silently.** `RingBuffer.Write` drops the
  oldest rows once its 1M-row capacity is full, with no signal. A long pause
  (or any drain stall) would lose rows without a trace. It should apply
  backpressure or at least count and report the drops, and the hold timeout
  must stay well inside what the buffer absorbs at peak ingest.
- **The DLQ is a no-op**, so every "DLQ'd" line in a roll today is lost
  data.

## Tests (each phase starts with a failing one)

- Pool: the desired set follows buckets (a new bucket's tier and stripes get
  spares; deleting the last bucket of a tier retires them); a stale-AMI
  spare is replaced new-first, never leaving its pair uncovered; two
  concurrent claims never take the same spare; the pool re-derives from the
  table and EC2 after a leader change.
- Spare boot: a spare's heartbeat never promotes or christens it; it refuses
  data RPCs with `not ready`.
- Server interceptor: a call during `paused` is refused before its handler
  (an `Ingest` refused leaves the row count unchanged), and in-flight calls
  complete before the shard reports drained.
- Client interceptor: against two in-process shard servers, a `Query` and an
  `Ingest` issued during the pause both complete on the successor, each
  exactly once; past `pauseHoldTimeout` the query fails with
  `MissingRowsError`.
- FSM: each UPGRADING phase re-derives after a simulated leader change; a
  spare that never readies rolls back, and the old instance serves again;
  with the pool empty, every path falls back to an on-demand launch.
- Drain isolation: with one shard paused, rows for a service on another
  shard keep landing.
- End to end (in-process launcher, fake EC2 volume attach): a roll under
  continuous query + ingest load loses no row and fails no query.

## Open questions

- `pauseHoldTimeout`: it must sit inside query and cron run budgets and
  inside what the ring buffer absorbs. 60s?
- Recovery force-detaches the dead instance's volume. If the instance is
  impaired rather than gone, force-detach risks the filesystem; terminate
  first, then detach?
- Detach of a volume the old instance hasn't cleanly unmounted during an
  upgrade (it hung): rolling back is safer than force-detaching.
- Should a spare be claimable across the image change during a deploy (an
  old-AMI spare for a recovery while the new ones boot)? Faster recovery,
  but it lands a shard on the old AMI that the roll then has to upgrade.
