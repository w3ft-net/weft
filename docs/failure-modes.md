# Failure modes

What keeps weft from losing logs when its own parts fail, and where that lives
in the source. Each row is a failure we postulated, the mechanism that stops it
costing rows, and the code to read. "Live" names the prod test that exercised it
on 2026-10-27 (scenario ids from `docs/plans/shard-seamless-upgrade-testing.md`);
rows with no "Live" are covered by unit tests only.

The guarantee is delivery, not freshness: a row sent by weftd is either on a
shard or in the DLQ, and is never dropped. Reads of an affected shard's window
can fail while it recovers (see [Not prevented](#not-prevented)).

## Shard replacement (seamless upgrade)

| Failure mode | Mechanism | Source |
|---|---|---|
| Old primary dies while the upgrade is pausing it | The upgrade ends and the shard returns to its origin state, where ordinary recovery relaunches it on the volume. No hold outlives the dead instance. Live: S5 | `logdb/upgrade.go`: `upgradePause`, `abortForDeadInstance` |
| Old primary is gone when a rollback tries to give the volume back | The rollback checks the instance before each reattach and gives up to recovery once it is gone or the rollback budget is spent, instead of retrying `AttachVolume` for ever. Live: S5 | `logdb/upgrade.go`: `upgradeRollBack`, `instanceGone`, `upgradeRollBackBudget` |
| Claimed spare dies mid-swap | The swap checks the spare each tick and rolls back: the volume returns to the old primary, which resumes. Live: S4 | `logdb/upgrade.go`: `upgradeSwap`, `successorGone` |
| Spare is alive but never opens the shard | The swap has a budget, and one tick's probe of the spare is capped to what is left of it. Past it, roll back. Live: S6, S8 | `logdb/upgrade.go`: `upgradeSwapBudget`, `swapProbeWindow`, `giveUpIfOver` |
| Crash or retry leaves the volume's location ambiguous (on the old primary, nowhere, or on the spare) | Each tick reads where the volume actually is and does only the next step (detach, attach, or neither). A location that is neither instance rolls back. | `logdb/upgrade.go`: `upgradeSwap` |
| Callers reach the old primary after the volume has moved | A paused gate refuses every call with a redirect naming the successor, and the caller re-runs it there. The demoted instance keeps redirecting until routes converge. | `shard/service/gate.go`: `Pause`; `shard`: `PausedError` |
| In-flight writes or queries are lost when the volume is released | Pause refuses new calls, waits for admitted ones (cutting queries past `pauseQueryGrace`), checkpoints and closes DuckDB, and unmounts. Only then does the leader detach. | `shard/service/gate.go`: `Pause` |
| Shard is paused with no sibling to take its writes | A mid-upgrade shard is still a write candidate when nothing else is ingesting, so senders wait for the successor rather than dropping rows to the DLQ. | `logdb/registry.go`: `IngestingShardExcept` |
| Leader coordinator dies mid-upgrade | The phase, the spare, and the timings are stored as facts in Postgres. The follower acquires the lease and resumes from the facts. Live: S7 | `logdb/upgrade.go`: `setUpgradeFacts`, `evaluateUpgrading`; `logdb/leader_election.go` |
| A stalled leader keeps driving EC2 after another has taken over | Every EC2, shard-row and pause call refuses a context whose lease has gone. A leader that stops converging releases the lease itself. Live: S8 | `logdb/fence.go`: `withLeaderFence`; `logdb/shardsm.go`: `ShardDeps.Leading`; `logdb/leader_election.go` (the deadman) |
| Crash between ending an upgrade and clearing its facts | The shard transition fires first, then the facts clear, so the worst case is stale facts on a shard no longer UPGRADING (ignored), never an UPGRADING shard with no facts. | `logdb/upgrade.go`: `endUpgrade` |

## Instance loss and boot

| Failure mode | Mechanism | Source |
|---|---|---|
| Primary dies outside an upgrade | Heartbeats go stale, the shard goes SUSPENDED, and recovery reaps it and relaunches onto its volume, claiming a warm spare when one is free. The volume is the state, so nothing on it is lost. Live: S1, S3 | `logdb/registry.go`: `heartbeatStaleThreshold`; `logdb/shardsm.go`: `evaluateRecovery`; `logdb/sparepool.go`: `ClaimOrLaunch` |
| A booting spare is claimed and retagged after `startup.sh` left its data directory unmounted, so the process comes up as a blank shard on the root disk and hides the real data | A process that manages a volume treats an unmounted data directory as "no volume yet" and waits, whatever its tag says. Live: S3 | `cmd/shard/volume.go`: `startsAsSpare`; `cmd/shard/main.go`; `deploy/startup.sh` |
| A spare serves before its volume is mounted and opened | A spare refuses every data call until `Activate` has mounted the volume and opened the database it carries. | `shard/service/gate.go`: `NewSpareServer`, `Activate` |

## Ingest path

| Failure mode | Mechanism | Source |
|---|---|---|
| A shard rejects a batch | The drain retries once on a different INGESTING shard for the same service. | `coordinator/drain.go`: `ingestBatch`; `logdb/registry.go`: `IngestingShardExcept` |
| A failing shard keeps receiving writes | Three consecutive failures trip a breaker: the shard goes DEGRADED, leaves write routing, and is retried every two minutes. Live: DLQ test | `logdb/registry.go`: `SuspendFailureThreshold`; `logdb/shardsm.go`: `evaluateIngesting`, `evaluateDegraded` |
| A hung shard blocks other shards' ingest | Each shard has its own queue and sender, so a hung shard stalls only itself. A queue past 200,000 rows overflows to the DLQ. | `coordinator/drain.go`: `enqueue`, `runSender`, `maxQueuedRowsPerShard` |
| No shard can take a batch: every shard for the service is down, DEGRADED or stale, the service has none yet, or both attempts failed | The rows go to the S3 DLQ as gzipped JSON lines. The leader replays the bucket every minute and deletes an object only after the ingest path has taken its rows. Rows that still cannot land are written back under a new key. Live: DLQ test | `coordinator/drain.go`: the nil-shard branch, `ingestBatch`, `StartDLQReplay`; `coordinator/dlq.go`: `Write`, `Replay` |
| The coordinator or the ingest stream dies before acknowledging rows | weftd keeps unacknowledged rows and advances its file checkpoint only after the ack, then resends from there. Delivery is at-least-once: a resend can duplicate rows. Live: S7, S8 | `cmd/weftd/filetail.go`; `docs/ingestion-weftd.md` |

## Not prevented

These are known and open, so a gap here is not the same as a gap in the table.

- **The ack is early.** weftd's checkpoint advances when the coordinator has put
  the rows in its in-memory ring buffer, not when a shard has them. A
  coordinator that dies between the ack and the drain loses those rows. Not
  exercised. See `coordinator/service/server.go` (`IngestStream`) and
  `coordinator/drain.go`.
- **Reads fail during recovery.** A killed primary leaves its window unreadable
  for about 40 to 60 s (S1, S3), longer if a claimed spare never opens. A query
  that needs a shard that is down fails; it is never answered partially.
- **Hold windows exceed the 10 s target.** Observed 10.3 to 12.8 s. The detach
  (1.9 to 5.6 s) and a spare's occasionally slow database open (0.2 to 7 s) are
  the largest parts.
- **A frozen spare may not recover after a thaw.** In S8 a spare thawed after
  being frozen across its attach never mounted its volume, and the swap rolled
  back at the budget. Cause not found.
- **A fenced call already in flight can still land.** The fence refuses new
  calls once the lease has gone, not ones already sent (`logdb/fence.go`).
- **Duplicates.** At-least-once delivery means a resend can store a row twice
  (676 of 24,001 in S8).
- **The audit does not see the DLQ.** `weft-admin audit` reported
  `ingestion_errors=0` through about 1,400 DLQ'd batches, because the error
  recorder is never constructed. It also did not notice a shard running from an
  unmounted volume. Do not read "healthy" as "nothing was DLQ'd". See
  `docs/known-issues.md`.
- **Not exercised live:** the queue-overflow DLQ branch, an empty spare pool in
  an AZ, and Tier 3 (several failures at once).
