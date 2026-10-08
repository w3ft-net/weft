# Plan: File-offset checkpointing in weftd

Persist each tailed file's position to disk so weftd resumes where it left off across restarts, instead of unconditionally seeking to EOF on open — and make the persisted position mean "delivered", not "read", so resuming never skips rows that were in flight when weftd stopped.

## Why add this

`weftd/tailer.go:24-30` — `NewTailer` always does `f.Seek(0, io.SeekEnd)` on open, with no read of any prior state. Every weftd restart — a systemd bounce during a `.deb` upgrade, a self-update (`auto_update`, on by default since v0.20.0), an OOM kill, an EC2 reboot, a Kubernetes DaemonSet pod being rescheduled — loses every line written to a tailed file while weftd was down. There's no offset file, no inode tracking, no resume path, and the gap is silent.

This isn't Kubernetes-specific; DaemonSet pods just restart more often. Every comparable agent solves it the same generic way — Vector checkpoints to its data dir, Fluent Bit uses a SQLite offset db, the OTel `filelog` receiver uses `file_storage`, Datadog mounts a `pointerdir` volume.

`docs/plans/weftd-disk-spool.md` flagged offset persistence as its v2 follow-up. This is that plan.

## What the first draft got wrong

The first draft of this plan proposed checkpointing `tailer.Offset()` — the tailer's read position — on a timer and at shutdown. Against the current code that loses *more* data than today, for these reasons. They shape everything below.

1. **Read position ≠ delivered position.** Between "tailer read it" and "coordinator has it" sit the tailer's 1024-slot channel, `runFileTailer`'s `buf` (≤100 lines), and gRPC's send window. `IngestStream` is client-streaming (`proto/weftpb/weft.proto:358`) — the only response is `IngestStreamResponse` at half-close, so a nil `Send()` means "buffered locally", nothing more. `sendLineWithReconnect` also silently drops a line whose post-reconnect send fails (`cmd/weftd/main.go:1295`). A checkpoint at the read position can sit *ahead* of delivery; the draft's claim that a stale checkpoint "can re-read, not lose" was only true if the checkpoint trails delivery.

2. **Graceful shutdown would guarantee a gap.** On `ctx.Done()` (`main.go:814`), `runFileTailer` calls `flushWithReconnect(ctx, …)` with the already-cancelled ctx: a needed reconnect fails immediately and `buf` is likely dropped today. Then the draft saved a final checkpoint past those lines. Graceful shutdowns are the *common* restart (self-update, `.deb` upgrade, DaemonSet rollout).

3. **`len(line)+1` byte counting drifts.** `bufio.Scanner` strips `\r` (CRLF under-counts), returns a partial final line at EOF without a newline (half a line ships as its own row, the remainder later as another, and the counter adds a `+1` for a newline that doesn't exist), and stops on lines >64KB with `ErrTooLong` — after which the tailer builds a fresh scanner and whatever the old one had buffered is gone. Every one desyncs a counted offset from the true file position.

4. **Rotation-while-down resumed at EOF.** The draft's flow said "inode mismatch → EOF" while its test said "→ 0". A file that replaced the checkpointed one while weftd was down is entirely unread; EOF drops everything written since rotation. Correct answer is 0.

5. **Inode alone isn't identity.** ext4/xfs commonly hand a just-freed inode to the next create, so delete+recreate can produce the same inode and we'd seek into unrelated bytes. And a `copytruncate`'d file that has regrown past the old offset passes the "offset ≤ size" check.

6. **Pre-existing tailer bugs undercut any guarantee:**
   - No truncation handling while running, despite the type comment claiming it. After `copytruncate`, the fd position is past the new EOF and reads return nothing until the file regrows past it.
   - On `Rename`, the old file is closed immediately (`tailer.go:90-103`) — its unread tail is dropped.
   - `watchGlob`-discovered files go through `NewTailer`, which seeks to EOF — lines written between the file's creation and our open are dropped, as are whole files created while weftd was down.

7. **Pruning by path-nonexistence races rotation** — a flush in the window between rename and recreate drops the entry.

## Design

### Commit point

The checkpoint for a file advances only to the end offset of the last line that has been **handed off**:

- **Phase 1 (no spool):** handed off = covered by an `IngestStreamResponse`. The coordinator's handler sends that response after the rows are in its ingest ring buffer (`coordinator/service/server.go`, `ingestBatch` → `s.Coord.Ingest.Write`). That's not durable on the coordinator side, but it is the only acknowledgement the protocol has, and it moves the agent-restart loss window to zero.
- **Phase 2 (spool shipped):** handed off = appended to the disk spool. The spool then owns delivery and its own ack bookkeeping; the checkpoint just tracks the spool's input.

In both phases `Tailer.Offset()` is never what gets persisted.

### File identity

A checkpoint entry is keyed by resolved absolute path (`filepath.EvalSymlinks` — Kubernetes' `/var/log/containers/*.log` are symlinks into `/var/log/pods/`) and carries:

```
{ "dev": 2049, "ino": 123456, "fp": "<hex sha256 of first 1024 bytes>",
  "fp_len": 1024, "offset": 48213, "updated_at": "..." }
```

`fp` is the fingerprint (Vector/Filebeat's approach). If the file is shorter than 1024 bytes at checkpoint time, `fp_len` records how many bytes were hashed, and resume hashes the same length. A file matches its checkpoint iff `dev`, `ino`, **and** `fp` all match.

### Resume rules

On opening a file:

| Situation | Start at | Logged |
|---|---|---|
| No `state.json` at all (fresh install / checkpointing just enabled) | EOF | — |
| `state.json` exists, no entry for this path (file appeared while down, or a new glob match) | 0 | `new file, reading from start` |
| Entry matches identity, `offset ≤ size` | `offset` | `resuming at N (M bytes behind)` |
| Entry matches identity, `offset > size` (truncated) | 0 | `truncated while down` |
| Identity mismatch (rotated/replaced while down) | 0 | `rotated while down; previous file's unread tail lost` |
| `size - start > max_resume_bytes` | `size - max_resume_bytes`, then forward to the next `\n` | `resume backlog N bytes exceeds cap; skipped M bytes` |

Files discovered by `watchGlob`'s `fsnotify.Create` handler *while running* start at 0 — they're brand new by construction.

Rotation-while-down loses the old file's unread tail. Recovering it (find the checkpointed `dev/ino` among the path's rotated siblings, finish it, then switch) is a reasonable later addition but not v1 — compressed rotations (`.gz`) make it unreliable anyway.

Every non-trivial row in that table increments a per-source counter (`skipped_bytes`, `resumed`, `rotated_while_down`, `truncated`) reported through the existing stats reporter (`runParseStatsReporter`), so gaps stop being silent.

### State file

`<state_dir>/checkpoints/state.json`, one per host:

- **Single writer.** `flock(LOCK_EX|LOCK_NB)` on `checkpoints/.lock` at startup. If it's held (DaemonSet `maxSurge`, a stray second weftd), log loudly and run with checkpointing disabled rather than corrupting state.
- **Atomic flush.** Write `state.json.tmp`, `fsync`, `rename`, `fsync` the directory. One flusher goroutine on `flush_interval` plus a final flush at shutdown. Per-file goroutines only update the in-memory map under a mutex.
- **Corruption tolerant.** Unparseable `state.json` → log, rename it to `state.json.corrupt`, and behave as "no `state.json`" (EOF everywhere). Never fatal: a fatal-config agent can't auto-upgrade out of the problem (see the weftd release-pinning notes).
- **Pruning by age only:** drop entries not updated in 7 days. Never prune on "path doesn't exist", which races rotation.

## Phases

### Phase 0 — tailer correctness (ships alone)

Fixes real bugs today with or without checkpointing; it's also the foundation the offsets depend on.

1. **Byte-accurate reader.** Replace `bufio.Scanner` in `weftd/tailer.go` with `bufio.Reader.ReadSlice('\n')`, tracking the true file position. Emit only complete lines; a trailing partial line is held until its `\n` arrives (and flushed as a final line only on rotation/close). Lines over a cap (default 1 MiB) are truncated with a marker and the rest consumed through the next `\n` — never silently dropped, never desyncing the position.
2. **Lines carry offsets.** The channel type becomes `Line{Text string; End int64; File FileID}` where `End` is the byte offset just past the line's `\n` and `FileID` is `{dev, ino, fp}`. `\r` is stripped from `Text` but counted in `End`.
3. **Truncation while running.** On each write event, `fstat` the fd: `size < pos` → seek to 0, count it, log it.
4. **Drain before reopen on rotation.** On `Rename`/`Remove`, read the old fd to EOF (it's still valid after rename) before opening the new path.
5. **`NewTailer(path, start int64)`** — explicit start offset; callers pass `-1` for EOF.
6. **New glob files from 0.** `watchGlob`'s Create path passes `0`.

Tests (failing first, per house rule — all of these fail on today's tailer):
- Partial line: write `"abc"`, wait for an event, write `"def\n"`; assert exactly one line `"abcdef"`.
- CRLF: `End` offsets equal the true byte positions.
- Long line: a 200 KiB line followed by a normal line; assert the normal line arrives and offsets stay exact.
- Copytruncate while running: write, consume, truncate, write; assert the post-truncate lines arrive.
- Rename rotation with unread tail: write lines, rename before the tailer drains, create a new file; assert the old tail arrives before the new file's lines.
- Glob Create race: create a file with content before `startFile` runs; assert the content arrives.

### Phase 1 — acked checkpoints

1. **Unacked window in `runFileTailer`.** Keep sent-but-unacked `Line`s in a bounded in-memory window (default 8 MiB). When it's full, stop reading from the tailer channel — backpressure is fine here, because the file is the buffer.
2. **Ack cycling.** Every `commit_interval` (default 15s), and whenever the window is full, half-close the stream (`CloseAndRecv`). On a successful response, commit the `End` of the last line in the window to the store, clear the window, and open a fresh stream. On stream error, open a fresh stream and **resend the whole window** (at-least-once; duplicates are the accepted cost, same as the spool plan).
   - Cost: one stream open per file per `commit_interval`. Every open runs `authenticateStream` plus an `agent_tokens` `UPDATE` (`server.go:518-533`). On a host tailing 50 files at 15s that's ~3.3 writes/s per host to RDS, which is a 1 GB t4g.micro shared with vimgym. **Before shipping, rate-limit that `last_seen_*` write on the coordinator (e.g. skip if updated <60s ago)**, or the fleet rollout turns into an RDS load test.
   - The coordinator idle reaper (`IngestStreamIdleTimeoutSeconds`) closes quiet streams with an error; that's just a stream error → resend → fine. On a quiet file, the empty window means nothing to resend.
3. **Shutdown drain.** On `ctx.Done()`, flush the window using a fresh `context.WithTimeout(context.Background(), 5*time.Second)`, not the cancelled ctx. Commit only on a successful `CloseAndRecv`; whatever doesn't make it stays uncommitted and is re-read next start.
4. **Fix the silent drop.** `sendLineWithReconnect`'s "send failed after reconnect" path must surface the error so the window is retained, instead of logging and moving on.
5. **`CheckpointStore`** (`weftd/checkpoint.go`) with the identity, resume, lock, flush, and pruning rules above; one instance built in `main()` and passed to every `runFileTailer`.
6. **Config:**
   ```yaml
   checkpoint:
     dir: /var/lib/weftd/checkpoints   # default
     enabled: true                     # *bool, nil = on (same pattern as auto_update)
     flush_interval: 5s
     commit_interval: 15s
     max_resume_bytes: 256MiB
   ```
   weftd uses plain `yaml.Unmarshal` (no `KnownFields`), so older agents ignore the new key and the rollout order doesn't matter.
7. **Packaging.** The `.deb` unit gets `StateDirectory=weftd` (pulled forward from the spool plan if it hasn't shipped). The Kubernetes DaemonSet manifest (`docs/plans/weftd-k8s.md`) **must** mount `/var/lib/weftd` as a `hostPath` — a container-local or `emptyDir` state dir dies with the pod, which is exactly the motivating case.

Tests (failing first):
- **Today's data loss, captured:** run the real `runFileTailer` against an in-process fake coordinator, write lines, cancel ctx, restart, write more; assert the pre-restart lines that were in flight are missing. This is the bug.
- **Graceful-shutdown drop:** with lines in `buf`, cancel ctx; assert the fake coordinator received them (fails today — the cancelled-ctx flush).
- Round trip: consume N, ack, restart via store, write M; assert exactly N+M, no gap.
- Crash before ack: send N, kill the stream before the response, restart; assert all N are received again (dupes allowed, no loss).
- Each resume-table row: same-inode-different-fingerprint, truncated-while-down, rotated-while-down, new-glob-file-while-down, backlog over `max_resume_bytes`.
- Corrupt `state.json` → `.corrupt` rename + EOF, no crash. Held lock → checkpointing disabled, tailing continues.

### Phase 2 — spool-backed commit

When `weftd-disk-spool.md` ships, the commit point moves from `IngestStreamResponse` to "appended to the spool"; ack cycling moves into the spool's replay loop. The checkpoint store, identity, and resume rules don't change. Coordinate with that plan rather than shipping both commit paths permanently — Phase 1's cycling code is deleted in Phase 2.

## Open questions

- **Replayed-row timestamps.** `buildLogRow` stamps `TsUnixNs = now` (`main.go:1259`). Resumed backlog *without* a parser lands at replay time, not write time — rows from a 2-hour outage all claim "now". With a parser, rows keep their real (old) timestamps, so they arrive late relative to the ingesting window. Need to confirm what the coordinator ingest drain does with rows whose timestamp falls in a window whose shard is already READ_ONLY (route to the current shard? drop?) before calling resume correct. If it drops them, resume needs either a coordinator-side "late row" policy or a client-side cap on how far back resumed parsed rows may go.
- **Coordinator durability.** The Phase 1 ack only means "in the coordinator's ring buffer". A coordinator crash, or ring-buffer overflow with no ingesting shard, still loses acked rows. That's out of scope here, but means end-to-end durability needs a coordinator-side story too.
- **journald.** Still a non-goal, but the same restart gap exists (`journalctl -f -n 0`). journald's `--cursor-file` makes it cheap; worth a small follow-up once this lands.

## Non-goals

- **No socket-source checkpointing.** `runSocketIngester` has no backlog to resume from.
- **No multi-line stitching** (`docs/plans/weftd-multiline.md`). Note though that Phase 0's reader rewrite is the natural place multiline will hook in, so sequence multiline after Phase 0, not in parallel with it.
- **No cross-host checkpoint storage.** A replaced host's unshipped tail is gone.
- **No exactly-once.** At-least-once; duplicates on resend/crash-before-ack are accepted.

## Rollout

weftd fixes need a version bump; the self-update is strictly-newer and restart-gated. Cut a release per phase. Phase 1's very first restart *onto* the new version still loses in-flight lines: the old binary does the shutdown, so it's the old code's behavior. The guarantee starts from the second restart.

Ship the coordinator `last_seen_*` rate-limit (Phase 1 item 2) and deploy it before the Phase 1 weftd release.

## Estimated effort

- Phase 0 — reader rewrite, truncation, rotation drain, glob-from-0, tests: **1.5 days**
- Phase 1 — unacked window + ack cycling + shutdown drain + `CheckpointStore` + config + packaging + tests: **3 days**
- Coordinator `last_seen_*` rate limit: **0.25 day**
- Docs (`docs/manual/weftd.md` — what's checkpointed, the knobs, the resume table, the k8s hostPath requirement): **0.5 day**
- Phase 2: folded into the spool plan's estimate.

**Total through Phase 1: ~5.25 days.**
