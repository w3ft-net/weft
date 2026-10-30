# Operations runbook

Operator-facing notes for running the weft coordinator in production. For
architecture see `architecture.md`; for shard lifecycle see
`shard-lifecycle.md`.

## Diagnosing a coordinator goroutine / memory leak

The coordinator exposes two non-destructive introspection surfaces. Both
are reachable only from the box itself — there is no public debug port —
so you drive them over an SSM session with `kevcons ssh coordinator`.

### pprof (localhost:6060)

`net/http/pprof` is served on `127.0.0.1:6060` (flag `--debug-port`; set
to `0` to disable). Same trust model as the WeftAdmin port: bound to
loopback, never exposed to the network.

The fastest way to spot a goroutine leak is the **aggregated** goroutine
profile — it groups goroutines by identical stack and prints a count, so
the leaking call site is whichever stack carries the outlier count:

```bash
kevcons ssh coordinator --command \
  'curl -s "localhost:6060/debug/pprof/goroutine?debug=1" | head -30'
```

```
goroutine profile: total 602
243 @ 0x... 0x...        # <- 243 goroutines parked at one stack = the leak
243 @ 0x... 0x...
 29 @ 0x...
```

`?debug=2` gives full symbolized stacks (function + file:line) so you can
read exactly where the leaked goroutines are parked:

```bash
kevcons ssh coordinator --command \
  'curl -s "localhost:6060/debug/pprof/goroutine?debug=2" > /tmp/gr.txt;
   grep -oE "github.com/graybeardcorp/weft/[a-zA-Z0-9_/]+\.[A-Za-z0-9_.]+" /tmp/gr.txt \
     | sort | uniq -c | sort -rn | head'
```

Note: `go version -m` on the binary won't show a VCS revision — the AMI
builds from a source tarball, not a git checkout (see `packer/`).

Heap, allocations, and CPU profiles are available at the usual paths
(`/debug/pprof/heap`, `/debug/pprof/profile`) for `go tool pprof`.

### SIGUSR1 goroutine dump

Sending `SIGUSR1` writes a full all-goroutine stack trace to
`/tmp/weft-coordinator-goroutines.txt` **without** bouncing the process —
useful when curl isn't handy or you want a file to copy off:

```bash
kevcons ssh coordinator --command \
  'sudo systemctl kill -s SIGUSR1 weft-coordinator;
   sleep 1; cat /tmp/weft-coordinator-goroutines.txt | head -60'
```

(`SIGINT`/`SIGTERM` still trigger graceful shutdown — only `SIGUSR1` is the
dump.)

### Query deadline

Every one-shot query (not `tail -f`) is cancelled after `QUERY_DEADLINE_SECONDS`
(default 60, the same for every caller; `0` disables). The caller gets
`This query did not finish in the time allowed` and the `explain` report of
their pipeline, never an internal error. Cancelling stops the shard and compute
work too. Tune it live, no deploy:
`kevcons config set QUERY_DEADLINE_SECONDS=<n>` (read on each query, within the
60 s config refresh). The coordinator logs each trip as `query rpc: deadline`
with the customer and pipeline: that line is the data for choosing the value.
Cron runs are not under it.

### Known leak class: abandoned ingest streams

Symptom: linear goroutine/heap growth on the
`memory-pressure-coordinator` dashboard, eventual OOM restarts. The
aggregated profile shows many goroutines parked in
`(*Server).IngestStream` → `stream.Recv()`. Cause: a producer opens an
ingest stream, ships a batch, then goes silent without half-closing
(client bug, crash, or NAT idle-drop). Mitigations already in place:

- **Idle reaper** — the coordinator closes a stream after
  `GRPC_INGEST_STREAM_IDLE_TIMEOUT_SECONDS` (default 300) with no rows.
  Tune via `kevcons config set GRPC_INGEST_STREAM_IDLE_TIMEOUT_SECONDS=<n>`
  (`0` disables); takes effect as agents reconnect.
- **weftd** holds one persistent stream per source (it previously opened
  one per journald line — fixed; fleet auto-upgrades).

Confirm a fix by watching the profile total plateau instead of climbing,
or via the `coord-goroutine-leak-trend` cron.
