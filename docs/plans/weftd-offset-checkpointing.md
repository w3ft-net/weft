# Plan: File-offset checkpointing in weftd

Persist each tailed file's read position to disk so weftd resumes where it left off across restarts, instead of unconditionally seeking to EOF on open.

## Why add this

`weftd/tailer.go:24-30` — `NewTailer` always does `f.Seek(0, io.SeekEnd)` on open, with no read of any prior state:

```go
func NewTailer(path string) (*Tailer, error) {
	f, err := os.Open(path)
	...
	f.Seek(0, io.SeekEnd)   // Seek to end — only tail new content.
```

Every weftd restart — a systemd unit bounce during a `.deb` upgrade, an OOM kill, an EC2 instance reboot, or (the case that prompted this) a Kubernetes DaemonSet pod being rescheduled — loses every line written to a tailed file while weftd was down. There's no offset file, no inode tracking, no resume path anywhere in `weftd/` or `cmd/weftd/main.go`, and the gap is silent: no error, no dropped-row counter, just missing rows.

This isn't Kubernetes-specific. It affects every deployment mode weftd runs in today; Kubernetes just makes it bite harder because DaemonSet pods restart far more often (rolling upgrades, node drain, OOM) than a long-lived EC2 instance does. Every comparable agent solves this the same generic way — Vector checkpoints read position to its data dir, Fluent Bit uses a SQLite offset db (`db` param on `tail`), the OTel Collector's `filelog` receiver uses the `file_storage` extension, Datadog mounts a `pointerdir` volume specifically to survive Agent restarts. None of these tie the mechanism to Kubernetes; it's a base agent capability that Kubernetes deployments simply lean on harder.

`docs/plans/weftd-disk-spool.md` already flagged this as an explicit non-goal and open question:

> **Offset persistence.** This plan fixes "in-flight rows during outage"; it does *not* fix "rows written to the source file while weftd was down." Separate concern with separate tradeoffs... If we want end-to-end durability we need both — flag it as a v2 follow-up.

This plan is that follow-up. Disk spool (in-memory-to-coordinator durability) and offset checkpointing (source-file durability) are complementary and independent; neither depends on the other shipping first.

## Architecture

```
weftd start
  │
  ├─ CheckpointStore.Load(path) → (inode, offset), ok
  │
  ├─ ok && inode matches current file's inode
  │     → NewTailer(path, offset)   // resume mid-file
  │
  └─ !ok || inode mismatch (file rotated away while we were down)
        → NewTailer(path, EOF)      // today's behavior; the pre-restart
                                     //   tail of the old file is an
                                     //   accepted, unrecoverable gap

while running:
  Tailer tracks cumulative bytes consumed → t.Offset()
  every flush tick (piggybacks on the existing 100ms send-batch ticker
  in runFileTailer) → CheckpointStore.Save(path, inode, offset)
  on ctx.Done() (graceful shutdown) → final Save before exit

CheckpointStore backing file: /var/lib/weftd/checkpoints/state.json
  { "<abs-path>": {"inode": 123456, "offset": 48213, "updated_at": "..."} , ... }
  rewritten atomically (temp file + rename) on each flush
```

Checkpoints are keyed by absolute path, with the file's inode stored alongside the offset as a validity check — not for identity. On resume, if the inode on disk no longer matches the checkpointed inode, the file was rotated (renamed away, or replaced) while weftd was down; the correct behavior is to treat it as a new file and start from EOF, not seek into unrelated bytes. This matches what `Tailer.watch()` already does for rotation detected *while running* (`weftd/tailer.go:90-103`) — we're just extending the same "new inode = new file" rule to cover the restart case.

If the checkpointed offset is larger than the current file size (truncated by `logrotate copytruncate`, or the file was replaced by something shorter), clamp to `0` rather than seeking past EOF.

## Components to build

1. **`weftd/checkpoint.go`** — new file:
   - `CheckpointStore` — loads `state.json` once at startup, holds an in-memory map guarded by a mutex, flushes to disk on `Save` (batched, not per-call — see below).
   - `Load(path string) (offset int64, inode uint64, ok bool)`.
   - `Save(path string, offset int64, inode uint64)` — updates the in-memory map; actual disk write happens on `Flush()`.
   - `Flush() error` — atomic rewrite of `state.json` (write to `state.json.tmp`, `os.Rename` over the target). Called on the existing periodic ticker and on shutdown, not on every line — matches the batching weftd already does for network sends.
   - Pruning: entries for paths that no longer exist (or haven't been touched in, say, 7 days — the source was removed from config or the file was deleted) are dropped on `Flush` so `state.json` doesn't grow unbounded across a long-lived host's lifetime.

2. **`weftd/tailer.go`** — extend `Tailer`:
   - `NewTailer(path string, startOffset int64) (*Tailer, error)` — when `startOffset > 0`, seek there instead of `io.SeekEnd`, after validating it against the file's current size (clamp per above).
   - Track a running byte offset as the scanner consumes lines (sum of `len(line)+1` per emitted line, reset to `0` on rotation-triggered reopen in `watch()`) and expose it via `t.Offset() int64` so callers can checkpoint without reaching into internals.
   - `Inode() (uint64, error)` — `Stat()` the open file and pull the inode (`syscall.Stat_t.Ino` on Linux; weftd is Linux-only per existing project convention, so no cross-platform shim needed).

3. **`cmd/weftd/main.go`**:
   - New top-level config section:
     ```yaml
     checkpoint:
       dir: /var/lib/weftd/checkpoints   # default
       disabled: false                   # default false = on; set true to opt out
       flush_interval: 5s                # default
     ```
   - `runFileTailer` (`main.go:731`): `store.Load(path)` before calling `weftd.NewTailer`; on the existing 100ms ticker (or a slower dedicated checkpoint ticker if 100ms proves too chatty for a full-map rewrite — decide during implementation based on typical tailed-file count per host) and on the `ctx.Done()` branch, call `store.Save(path, tailer.Offset(), tailer.Inode())` then let the batched `Flush()` cadence handle the actual disk write.
   - `watchGlob`-discovered files (`main.go:908`) already route through `runFileTailer` for each matched file, so they inherit checkpointing for free — no glob-specific code needed.
   - One `CheckpointStore` instance shared across all `runFileTailer` goroutines on a host, constructed once in `main()` and passed down.

4. **Systemd unit** — needs `/var/lib/weftd` to exist with correct ownership before weftd starts writing to it. `docs/plans/weftd-disk-spool.md` item 4 already adds `StateDirectory=weftd` to the `.deb`'s systemd unit for the spool directory; this plan reuses the same directory (`checkpoints/` subdir under it) rather than requesting a second `StateDirectory`. If disk-spool hasn't shipped yet when this lands, pull that one line of systemd-unit change forward into this plan instead of blocking on it.

5. **Tests** (per the "prove you understand the bug first" rule — start with a failing test that demonstrates today's data loss):
   - Failing-test-first: write lines to a temp file, run a `Tailer` to consume some, close it (simulating a restart) without a `CheckpointStore`, start a new `Tailer` on the same file, write more lines, assert the lines written *before* the restart are gone from the new tailer's output — this is today's bug, captured as a test.
   - Round trip: consume N lines, checkpoint, "restart" (new `Tailer` seeded from `CheckpointStore.Load`), write M more lines, assert exactly N+M lines total with no dupes and no gap.
   - Inode-mismatch: checkpoint against file A, delete and recreate the path (new inode) with different content, restart, assert tailing starts from `0` of the new file rather than seeking into it at the stale offset.
   - Truncation: checkpoint offset beyond current file size (copytruncate case), assert clamp to `0` rather than a failed seek or a hang.
   - `CheckpointStore.Flush` atomicity: kill the process mid-write (simulate via a truncated `state.json.tmp` left on disk), assert `Load` at next startup either recovers cleanly or falls back to EOF for the affected entries — never crashes on a corrupt state file.

## Estimated effort

- `CheckpointStore` (load/save/flush/prune) + tests: **1 day**
- `Tailer` offset/inode tracking + seek-on-open + tests: **0.75 day**
- Wire into `runFileTailer` + config + systemd dir: **0.5 day**
- Failing-test-first data-loss repro + round-trip/rotation/truncation/corruption tests: **1 day**
- Docs (`docs/manual/weftd.md` — what's checkpointed, the `disabled` knob, what happens on rotation): **0.25 day**

**Total: ~3.5 days**.

## Non-goals

- **No journald checkpointing.** `runJournalTailer` reads from systemd's journal, which already has its own durable cursor mechanism if we ever need resumability there — a different, journald-native concern, not this plan.
- **No socket-source checkpointing.** `runSocketIngester` has no backlog to resume from by construction; not applicable.
- **No multi-line event stitching.** `docs/plans/weftd-multiline.md` covers stitching stack traces / multi-line events into one row — a content-shaping concern, unrelated to *which bytes have been read*. Don't conflate the two despite both touching `weftd/tailer.go`.
- **No cross-host or shared checkpoint storage.** One host, one local `state.json`. If a host is replaced entirely (not just restarted), its tailed files' pre-replacement tail is gone — same accepted gap as today's rotation-while-down case.
- **No sub-second checkpoint durability guarantee.** Flush is batched on a timer; a crash between flushes can re-read (not lose) a handful of already-shipped lines from the old offset on next start, since seeking to a slightly-stale earlier offset produces at-least-once, not data loss. Duplicate rows from this are an accepted tradeoff, same as the disk-spool plan's replay semantics.
