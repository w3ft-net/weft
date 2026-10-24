# Seamless shard upgrade: standby instance + pause/redirect

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

## Mechanism

Build the replacement first; make the swap one short, shard-enforced pause,
the way pgbouncer's PAUSE works, with the shard as the single choke point
for its data.

1. **Standby.** Launch the new instance (current AMI, the volume's AZ) with
   no volume. Its shard binary starts in standby mode for shard X and
   heartbeats as such. The old instance keeps serving throughout.
2. **Pause.** The FSM leader tells the old shard: pause, your successor is
   at `addr`. Its server interceptor refuses every new call with a
   `paused` status carrying the successor address, and lets in-flight calls
   finish.
3. **Swap.** In-flight at zero: the old shard checkpoints, closes DuckDB,
   and the volume is unmounted. The leader detaches the volume and attaches
   it to the standby, which mounts it, opens DuckDB from `shard.json`, and
   starts admitting calls. The shard's `Addr` / `InstanceID` move to the new
   instance.
4. **Resume.** Held calls (below) land on the new instance. The old
   instance keeps refusing with the redirect until the new one has
   heartbeat and every coordinator's registry carries the new address, then
   it is terminated.

Expected gap: unmount, detach, attach, mount, DuckDB open, roughly 10–30s.

### The interceptors

They do different jobs, not mirror ones.

**Server side (shard): correctness.** One unary + one stream interceptor on
every shard RPC:

- `serving`: pass through, counting in-flight calls.
- `paused`: refuse with a dedicated status (code + detail carrying the
  successor address) before the handler runs. A refused call was never
  applied, and for the streaming `Query` the refusal precedes the first
  message, so a retry can neither double-apply an `Ingest` nor double-send
  rows.
- `standby`: refuse with a `not ready` status until the volume is mounted
  and DuckDB open.

Only the shard knows when its in-flight count is zero, whatever the number
of clients or the state of their registries, which is why the pause lives
here.

**Client side (`shard.GRPCShard`): latency instead of errors.** Every
outbound shard connection is a `GRPCShard`: compute's reads
(`compute/resolver.go`) and the coordinator's writes and Configure (handles
from `logdb/provisioner.go`, `logdb/reconstruct_shard.go`). Heartbeats go
shard→coordinator and don't pass through it, so the standby can announce
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

### Rollback

The old instance is not terminated until the new one serves. If the standby
never becomes ready (attach stuck, mount fails, DuckDB won't open, timeout),
the leader detaches the volume from it, reattaches it to the old instance,
which remounts, reopens and unpauses. Calls held against the successor never
got in, so their retries simply go back to the old address once the
redirect is withdrawn. The standby is terminated.

## FSM

UPGRADING stops meaning "routed away". It becomes a serving state with
persisted phases (mirrored onto the entry, like the export columns):

| phase | routing | done when |
|---|---|---|
| `standby_launching` | serving (old instance) | standby heartbeats in standby mode |
| `pausing` | serving; calls held by interceptors | old shard reports in-flight 0 and closed |
| `swapping` | serving; calls held | standby reports DuckDB open |
| `resumed` | serving (new instance) | every coordinator routes to the new address; old terminated → INGESTING / READ_ONLY / DEGRADED (the state it came from) |
| `rolling_back` | serving; calls held | old instance reopened and unpaused |

`queryable` treats UPGRADING as queryable again (69be93fa's
"UPGRADING: its instance is being replaced" goes away). A query during
`pausing` / `swapping` is routed normally and held by the interceptor.
SUSPENDED is unchanged: an instance that died is a real outage and fails
queries.

Every phase is re-derivable after a coordinator restart or leader change:
the leader reads the phase, the volume's attachment (EC2 `DescribeVolumes`)
and the instances' heartbeats, and drives forward or rolls back.

## Pieces to build

1. **Standby boot.** `deploy/startup.sh` mounts `/dev/sdf` at boot and, after
   ~60s with no volume, falls back to the root volume ("dev/container
   mode"), and the shard then starts blank awaiting Configure. A standby
   must be an explicit mode (a launch tag or user-data flag), never that
   fallback:
   - the binary doesn't start a shard on the root volume; it heartbeats as
     standby-for-X;
   - the shard runs as `app`, so mounting after boot needs a root helper (a
     oneshot systemd unit the binary triggers, or a narrow sudoers rule);
   - a standby's heartbeat must not reach `shardByInstance` /
     `promoteOnHeartbeat`, which would christen it as a fresh LAUNCHING
     shard.
2. **Pause RPC + server interceptor** on the shard: `Pause(successor)`,
   `Resume()` (rollback), in-flight accounting, the refusal statuses, and
   the checkpoint / close / unmount sequence once drained.
3. **Client interceptor + manual resolver** in `shard/grpc_client.go`: hold,
   redirect, bounded wait.
4. **Leader orchestration**: the UPGRADING phases above in the shard SM,
   driving EC2 launch (no volume), detach/attach, termination, and rollback.
5. **Drain isolation.** `StartDrain` is one goroutine writing to shards in
   sequence, so a held `Ingest` would stall every write on that coordinator.
   Each shard needs its own send path (a per-shard worker and queue) so a
   paused shard holds only its own rows.
6. **weft-admin**: `roll-*` waits for UPGRADING → back in the origin state
   with the target `binary_sha`, as today; only the meaning of the wait
   changes.

## Related hazards found along the way

- **The ring buffer overwrites silently.** `RingBuffer.Write` drops the
  oldest rows once its 1M-row capacity is full, with no signal. A long pause
  (or any drain stall) would lose rows without a trace. It should apply
  backpressure or at least count and report the drops, and the hold timeout
  must stay well inside what the buffer absorbs at peak ingest.
- **The DLQ is a no-op**, so every "DLQ'd" line in a roll today is lost
  data.

## Tests (each phase starts with a failing one)

- Server interceptor: a call during `paused` is refused before its handler
  (an `Ingest` refused leaves the row count unchanged), and in-flight calls
  complete before the shard reports drained.
- Client interceptor: against two in-process shard servers, a `Query` and an
  `Ingest` issued during the pause both complete on the successor, each
  exactly once; past `pauseHoldTimeout` the query fails with
  `MissingRowsError`.
- FSM: each UPGRADING phase re-derives after a simulated leader change; a
  standby that never readies rolls back, and the old instance serves again.
- Drain isolation: with one shard paused, rows for a service on another
  shard keep landing.
- Standby boot: a standby heartbeat never promotes or christens.
- End to end (in-process launcher, fake EC2 volume attach): a roll under
  continuous query + ingest load loses no row and fails no query.

## Open questions

- `pauseHoldTimeout`: it must sit inside query and cron run budgets and
  inside what the ring buffer absorbs. 60s?
- Should SUSPENDED recovery also launch a standby first, so the replacement
  is already booted when the dead instance's volume frees up? It doesn't
  make that outage seamless, but it shortens it by the boot time.
- Detach of a volume the old instance hasn't cleanly unmounted (it hung):
  force-detach after a timeout risks the filesystem; rolling back is safer.
