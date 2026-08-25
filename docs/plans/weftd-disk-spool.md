# Plan: weftd disk spool

Bounded, on-disk buffer so weftd can survive a coordinator outage longer than its in-memory batch (currently 100 lines). Spool to disk when the upstream stream is unavailable; replay in order on reconnect.

## Why add this

`cmd/weftd/main.go` buffers file-tailer output in an in-memory `[]string` and flushes every 100ms or 100 lines through `IngestStream`. If the coordinator is unreachable, `openStream()` retries with exponential backoff up to 30s; during that time the in-memory buffer grows without bound, then the goroutine blocks on `stream.Send` and the tailer's channel fills. A longer outage — coordinator rolling restart, customer-side network blip, DNS misbehavior — drops data silently.

Every mature agent (Fluent Bit `filesystem` storage, Vector's disk buffer, Datadog Agent's `dd_url` retry store) has the same fix: spool to disk, replay on reconnect. We need the same thing.

There's an adjacent-but-distinct issue — the file tailer seeks to end on open (`weftd/tailer.go:32`) so lines written while weftd is *down* are missed. That's a file-offset-persistence problem, not a spool problem. Flagged in Open Questions; not in v1 scope.

## Architecture

```
Tailer emits line
  → in-memory send buffer (100-line batch)
    → stream.Send
      ├─ ok → done
      └─ err → spill buffer to disk, set "spooling" mode
                ↓
      spooling mode:
        every new line → append to active spool file
        reconnect attempt loop in background
          → on success: replay spool files oldest-first,
                         deleting each as we confirm ack,
                         then clear spooling mode
```

Spool layout under `/var/lib/weftd/spool/<app>/`:

- `current.log` — append-only file being written to now.
- Rotated when it hits 10 MB → `000001.log`, `000002.log`, … with a monotonic counter.
- On replay, files are read oldest-first; each row goes through `stream.Send` one at a time; file is `unlink(2)`ed after the last row in it is sent.
- A cap — `spool.max_mb` default 500 MB — bounds total on-disk bytes. When exceeded, the *oldest* file is deleted and a counter incremented (customer sees "dropped 142 MB of spooled rows during 6h outage" in the next metering report).

Framing per line: length-prefixed protobuf-serialized `pb.LogRow` (varint length + marshaled bytes). No JSON, no per-line checksum — if the disk is silently corrupting bytes we have bigger problems, and the coordinator already rejects malformed `LogRow`s.

## Components to build

1. **`weftd/spool.go`** — new package-internal:
   - `Spool.Append(row *pb.LogRow)` — write to current file, rotate at 10 MB.
   - `Spool.Replay(ctx, sendFn)` — iterate oldest-first, call `sendFn` per row, unlink file on clean pass.
   - `Spool.Bytes()` — current on-disk usage; for metering and the cap.
   - `Spool.Trim(maxBytes)` — drop oldest to fit under cap; return bytes dropped.
2. **`cmd/weftd/main.go`** — replace `sendLineWithReconnect` with a two-state machine (`live` / `spooling`). Entering `spooling` spills the in-memory batch; exiting replays via `Spool.Replay`. Tests: inject a failing stub stream, assert rows land on disk, restore the stream, assert they arrive in order.
3. **Config**:
   ```yaml
   spool:
     dir: /var/lib/weftd/spool    # default
     max_mb: 500                  # default
     rotate_mb: 10                # default
   ```
   Enabled by default; `spool.disabled: true` to turn off entirely (small-footprint deployments that would rather drop than write).
4. **Systemd unit update** — the `.deb` package already installs `weftd.service`; add `StateDirectory=weftd` so systemd creates `/var/lib/weftd` with the right owner on install.
5. **Metering** — on flush of a spool file, weftd writes a local stat line summarizing `(rows_sent, bytes_sent, spool_time)`; the coordinator rolls these into `usage_daily` so outage recoveries are visible in the Account page.
6. **Docs** — extend `docs/manual/weftd.md` with a "Disk spool" subsection: where it lives, cap knob, what the `max_mb` counter means.

## Crash semantics

- **weftd crash mid-append.** Worst case: one partial row at the tail of `current.log`. Replay detects a truncated varint and stops at that offset; subsequent writes append cleanly over whatever the partial bytes were (or the file is rotated and the partial is abandoned). Net: at most one row is dropped per crash.
- **Coordinator rejects a row mid-replay.** Treat the same as a transport error: bail out of the replay, leave the remaining spool files in place, go back to retry loop. No poison-pill concern because rows that were valid when tailed remain valid on replay (the schema doesn't move under us; if it did, that's a coordinator-side migration concern, not a spool concern).
- **Disk full.** Every `Append` checks `Spool.Bytes()` against the cap before writing; over-cap drops oldest rather than writing past the limit. If the filesystem itself is full (cap not yet hit), the append fails, we log it, and fall back to in-memory-only behavior for that batch — which then blocks/drops like today once the in-memory buffer fills.

## Open questions

- **Offset persistence.** This plan fixes "in-flight rows during outage"; it does *not* fix "rows written to the source file while weftd was down." Separate concern with separate tradeoffs (the stat-based offset check has to cope with logrotate, copytruncate, file recreation). If we want end-to-end durability we need both — see `docs/plans/weftd-offset-checkpointing.md`, the v2 follow-up this points to.
- **Per-source vs. global spool.** Current draft is per-app under `/var/lib/weftd/spool/<app>/`. Alternative is one global spool with `app` in the framed row, which is slightly less code but mixes per-app replay pacing. Per-app is simpler to reason about at the cost of more files.

## Estimated effort

- `Spool` package + rotate + cap + replay: **1.5 days**
- Two-state machine in `main.go`: **0.5 day**
- Config + systemd `StateDirectory`: **0.25 day**
- Metering surface: **0.5 day**
- Docs + tests (fault-injection and crash-mid-append): **1 day**

**Total: ~3.5 days**.

## Non-goals

- **No end-to-end exactly-once.** At-least-once on replay; duplicates are possible if we re-sent a row whose ack was lost. Today's coordinator ingest is idempotent-enough (dedup via `(app, line_num)` on the shard side if/when that becomes a real problem); we don't need a transactional protocol for this.
- **No file-offset persistence.** See `docs/plans/weftd-offset-checkpointing.md`.
- **No cross-process shared spool.** One weftd process, one spool directory. We don't run multiple weftd processes on the same host.
- **No encryption at rest.** Spool files contain raw log bytes for a brief window; full-disk encryption is the expected answer for customers who care. Not revisiting unless a customer names it.
