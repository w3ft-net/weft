# Shard rolling upgrade

## Why

The recent incident exposed two related problems:

1. The kevcons deploy was triggering `start-instance-refresh` on the shard
   ASGs. That bypasses the FSM, terminates data-bearing instances out of
   band, and orphans the replacements (the FSM doesn't adopt instances it
   didn't mint).
2. There was no FSM-driven way to upgrade a shard's binary in place. Neither
   "wait for window rotation" (a week in prod) nor `ForceArchive` (exports
   data + retires the volume — wrong for mid-window upgrade) fits.

The right mechanism is the FSM's existing **SUSPENDED → Relaunch** recovery
branch, deliberately triggered. That branch already preserves volume +
identity (it was built for instance death recovery). All we're missing is
an admin trigger to drive it, a CLI to invoke it one shard at a time, and a
way to skip when the shard binary hasn't changed.

The Terraform-side hardening (suspend `ReplaceUnhealthy`/`HealthCheck` on
shard ASGs, drop the `instance_refresh` block from the shard variant of the
module, gate the migration lifecycle hook to coordinator-only) is the
counterpart of this plan and lands in `infrastructure-catalog`. This doc is
the weft-side half.

## Mechanism: the FSM edge

Add one trigger:

```
TriggerRelaunchRequested:
  permitted from: StateIngesting, StateReadOnly, StateDegraded
  target:         StateSuspended
```

That's the only new FSM surface. The rest is already wired:

- `SUSPENDED.OnEntry` drops the live handle (the volume is untouched).
- `evaluateSuspended` reaps the dead instance via
  `ASGProvisioner.TerminateForReplacement` and fires `TriggerRelaunch` →
  `StateLaunching`.
- `evaluateLaunching` calls `ASGProvisioner.LaunchInstance`, which scales
  the ASG using the **current** launch template — i.e. the new AMI.
- The launch path reattaches the surviving EBS volume in the same AZ (FSM
  striping pins recovery to the volume's AZ).
- The new instance boots, `shard.json` is on the reattached volume,
  `NewServerFromIdentity` reconstructs the shard, it heartbeats with its
  new InstanceID, and the existing promote path (now with the idempotent
  `Configure` from 29e3338 and the dial tolerance) lifts it to INGESTING.

Net effect: same shard id, same window, same volume, same data — new
binary on a fresh instance.

States we do **not** permit from:

- `LAUNCHING` — already coming up; the launch will pick up the current AMI
  on its own.
- `SUSPENDED` — already there; the evaluator will drive it.
- `ARCHIVING` / `ARCHIVED` / `GONE` — no point.

## Admin RPC

Sibling of `ForceArchiveShard` / `DeleteArchivedShard` on `WeftCoordinator`:

```
rpc RelaunchShard(RelaunchShardRequest) returns (RelaunchShardResponse);

message RelaunchShardRequest { string shard_id = 1; }
message RelaunchShardResponse {}
```

Coordinator handler mirrors `ForceArchiveShard`: load entry, validate state,
fire `TriggerRelaunchRequested`, evaluate. Reuses `shardOpStatus`:
`ErrShardNotInRegistry` → `NotFound`, `ErrShardWrongState` →
`FailedPrecondition`. No new error types.

`api/admin.go` does **not** get a forwarding handler — this is an
internal-only admin op, called only from the coordinator-resident CLI
below. (We can add a forwarded HTTP endpoint later if we want it in the web
admin UI, but it isn't on the deploy critical path.)

## Skip-if-current: detecting whether a shard needs an upgrade

Shards are the thinnest binary in the system (gRPC over DuckDB + a heartbeat
loop). Most deploys touch coordinator/api/compute and don't change shard
code at all. The deploy should be a no-op for shards in that case — never
fire a relaunch unless the binary actually changed.

The signal we need: **shard reports its build SHA on every heartbeat;
coordinator persists it; CLI compares it to a target SHA.**

### Build-side

Embed the build SHA in `cmd/shard` the same way `cmd/weft` and `cmd/weftd`
already do (see `docs/versioning.md`):

```go
// cmd/shard/version.go
package main

var (
    Version = "dev"
    GitSHA  = ""
)
```

Packer's `go build` for the shard binary sets `GitSHA` via `-ldflags`.

### Wire

Extend `ShardHeartbeatRequest`:

```
message ShardHeartbeatRequest {
    ...
    string binary_sha = 12;   // shard's compiled-in GitSHA
}
```

The shard fills this from its compile-time `GitSHA`.

### Persistence

Add `binary_sha TEXT` to `shard_records`. `TouchHeartbeat` writes it
alongside `row_count` and `last_heartbeat_at` (slot-in extension of the
heartbeat-persistence change in 043fce5 / 00ed995). The column is purely
informational from the runtime's perspective; only `weft-admin` reads it.

### Comparison

`weft-admin roll-* --target-sha <sha>` reads `binary_sha` for each shard
and skips any whose value equals `<sha>`. With no `--target-sha`, rolls
unconditionally (useful for "ship now" emergencies and for tests).

The target SHA is computed by kevcons at build time — `git log -1
--pretty=%H -- cmd/shard/ shard/ proto/weftpb/` against the build's HEAD
(includes proto because a wire change can require shard rebuilds). kevcons
embeds it in the build manifest and passes it to `weft-admin`.

## CLI: weft-admin on the coordinator

Ship a new binary on the coordinator AMI (`cmd/weft-admin/`, built by
Packer like the others) that connects to the coordinator's gRPC on
`localhost`. Thin client over the admin RPCs.

Subcommands:

- `roll-shard <shard-id> [--per-shard-timeout=10m]`
  Fires `RelaunchShard`. Polls `GetShard` until the shard is back in
  INGESTING with a fresh heartbeat and (if `--target-sha` is set) the
  expected `binary_sha`. Exits non-zero on timeout.

- `roll-cohort <bucket-id> [--target-sha=<sha>] [--per-shard-timeout=10m] [--continue-on-failure]`
  Reads the bucket's live shards (LAUNCHING/INGESTING/READ_ONLY/DEGRADED),
  iterates in deterministic id order, rolls each one at a time, waits for
  each to come back before the next. Skips any whose `binary_sha` already
  matches `<sha>`.

- `roll-all [--target-sha=<sha>] [--per-shard-timeout=10m] [--continue-on-failure]`
  Iterates every bucket and runs roll-cohort on each.

Deploy-time invocation:

```
kevcons ssh --command \
  'sudo /opt/app/bin/weft-admin roll-all --target-sha <shard-sha> --per-shard-timeout 10m'
```

## Sequencing

Strict one-at-a-time across the entire fleet for the first version. The
iterator:

1. Resolve target shards (filter to LAUNCHING/INGESTING/READ_ONLY/DEGRADED).
2. Sort by id (deterministic).
3. For each shard:
   - If `target-sha` matches → log "skip <id> (already current)", continue.
   - Call `RelaunchShard`. Returns FailedPrecondition → log+skip; other
     non-OK → abort (unless `--continue-on-failure`).
   - Poll `GetShard` every 5s until: state == INGESTING **and**
     `last_heartbeat_at > now - 90s` **and** (if target set)
     `binary_sha == <sha>`. Timeout aborts.
4. Next shard.

Across buckets: also sequential (a deploy is rare enough that fleet-wide
parallelism is not worth the failure-mode complexity in v1).

Inside a bucket: never roll two shards concurrently — the cohort serves
queries and writes during the roll, and we preserve `Target - 1`
availability throughout. (The bucket SM doesn't need to know about the
roll; the SUSPENDED transition naturally drops the rolling shard out of
the ingest cohort.)

## Deploy integration (kevcons)

In the kevcons deploy flow, after the coordinator instance refresh has
completed and the new coordinator is InService + healthy:

1. Compute `<shard-sha> = git log -1 --pretty=%H -- cmd/shard/ shard/
   proto/weftpb/` at the build commit (stored in the build manifest).
2. `kevcons ssh --command 'sudo /opt/app/bin/weft-admin roll-all
   --target-sha <shard-sha> --per-shard-timeout 10m'`.
3. Non-zero exit halts the deploy and surfaces the failure for the
   operator. The coordinator is already on the new AMI, so the deploy is
   "half-rolled" — operator decides whether to resume.

Critically: kevcons stops calling `start-instance-refresh` on shard ASGs.
Coordinator/api/compute ASGs continue to use instance_refresh (they're
stateless). Shard ASGs are FSM-driven, full stop.

When the shard SHA hasn't changed (the common case for most deploys),
roll-all logs "skip <id> (already current)" for every shard and exits in
seconds.

## Failure handling

- **Shard fails to relaunch** (volume reattach fails, instance won't boot,
  configure dial keeps failing): caught by the existing `evaluateLaunching`
  retry budget. After `LaunchRetryBudget` (default 5) attempts the FSM
  moves it to GONE. `weft-admin` notices the GONE state during its
  wait-and-verify and aborts the cohort iteration — the operator
  investigates.
- **Shard relaunches but reports the wrong SHA**: final-step failure in
  the wait-and-verify (we required `binary_sha == target_sha`). Halts the
  cohort. Likely a mis-built AMI.
- **`--continue-on-failure`** for emergencies (run all rolls, report
  per-shard outcomes at the end).
- **Deploy aborts mid-roll**: half-rolled fleet is a stable state — the
  coordinator and the already-rolled shards are on new code; the rest are
  on old code, still serving. Re-running `roll-all --target-sha <sha>`
  resumes (skip-if-current handles the already-rolled ones).

## Tests

- **FSM** (`logdb/shardsm_test.go`):
  - `TestShardSM_TriggerRelaunchRequested_FromIngesting_GoesSuspended`.
  - Same for `StateReadOnly`, `StateDegraded`.
  - Rejected from `StateLaunching`, `StateArchiving`, `StateArchived`,
    `StateGone`, `StateSuspended` (no permit).
- **Admin RPC** (`coordinator/service/shard_admin_test.go`):
  - `TestRelaunchShard_UnknownShard_NotFound`.
  - `TestRelaunchShard_WrongState_FailedPrecondition`.
- **Skip-if-current**: unit test the iterator's decision logic with a
  fake coord client that returns `binary_sha` per shard.
- **End-to-end roll-cohort** (integration-style, in-proc launcher +
  stubConfigurer): drive a 3-shard cohort, mark two with a stale
  `binary_sha`, call roll-cohort, assert exactly those two cycled through
  SUSPENDED→INGESTING and the third was skipped.

Each phase below ships with a failing test first.

## Out of scope (called out for clarity)

- **Terraform hardening** — separate work in `infrastructure-catalog`:
  - `suspended_processes = ["ReplaceUnhealthy", "HealthCheck"]` on shard
    ASGs.
  - Remove the `instance_refresh` block from the shard variant of the
    `asg-application` module (or gate it on a flag the shard call doesn't
    set).
  - Gate the migration lifecycle hook by role (coordinator-only).
- **Web admin UI for relaunch** — once `weft-admin` works end-to-end,
  optionally surface it through `api/admin.go` as a forwarded HTTP
  endpoint. Not on the deploy critical path.
- **Parallel cohort rolls** — could speed up fleet-wide rolls when many
  buckets exist, but v1 stays serial. Revisit when the bucket count makes
  it worth the complexity.
- **Auto-rollback on bad shard binary** — if a relaunched shard reports an
  unexpected SHA or fails to come up, v1 just halts. A future revision
  could pin the previous AMI and re-relaunch onto it.

## Implementation phases

Each phase ships independently with TDD.

**Phase 1 — Single-shard relaunch end to end.**
- `TriggerRelaunchRequested` FSM edge (logdb).
- `RelaunchShard` admin gRPC (proto + coordinator handler).
- `cmd/weft-admin` with `roll-shard <id>`.
- Packer ships `weft-admin` to the coordinator AMI.
- Operators can manually roll one shard. Unblocks ad-hoc upgrades and
  validates the FSM + RPC + CLI scaffolding.

**Phase 2 — Cohort / fleet sequencing.**
- `roll-cohort <bucket-id>` and `roll-all` subcommands.
- Wait-and-verify loop with timeout + abort semantics.
- `--continue-on-failure`.

**Phase 3 — Skip-if-current.**
- `binary_sha` in `cmd/shard/version.go`, populated via ldflags.
- `ShardHeartbeatRequest.binary_sha` proto field.
- `shard_records.binary_sha` column (migration).
- `TouchHeartbeat` persists it.
- `--target-sha` flag on the roll commands; skip-if-match.

**Phase 4 — Kevcons deploy integration.**
- kevcons computes the shard SHA from the build commit.
- kevcons calls `weft-admin roll-all --target-sha <sha>` after the
  coordinator refresh.
- kevcons stops calling `start-instance-refresh` on shard ASGs.

**Phase 5 — Infra hardening (parallel, in `infrastructure-catalog`).**
- See "Out of scope" above. This work is independent of weft phases 1-4
  and is the defense-in-depth layer.

Phases 1-3 are self-contained in the weft repo. Phase 4 needs kevcons +
deploy-flow changes. Phase 5 is the infra repo.
