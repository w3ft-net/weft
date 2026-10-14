# Plan: Multi-line event stitching in weftd

Stack traces, panic dumps, and multi-line structured events currently land as separate rows, one per physical line. Add per-source stitching rules so a Java stack or a Python traceback becomes one `LogRow` the shell can grep.

## Why add this

Right now `weftd/tailer.go` emits one `LogRow` per line, unconditionally. A Python exception like:

```
Traceback (most recent call last):
  File "/app/views.py", line 42, in handler
    result = do_work(request)
  File "/app/service.py", line 88, in do_work
    raise ValueError("missing field: user_id")
ValueError: missing field: user_id
```

becomes seven rows. `grep ValueError` hits only the last line; `grep do_work` hits a different line; the shape of the trace is only recoverable by sorting and pasting in your head. This is the single most common complaint against file-tailing agents and the reason Fluent Bit / Vector / Filebeat all have dedicated multi-line parsers.

The stitching is agent-side: the coordinator sees one already-stitched event. Storage splits it back into one row per line sharing an `event_id` ([line-storage](line-storage.md)), so the stitch is what groups the lines.

## Architecture

Per-source `multiline:` block in weftd's config, with one of three strategies:

```yaml
sources:
  - app: api
    path: /var/log/api.log
    multiline:
      mode: start_regex
      pattern: '^\d{4}-\d{2}-\d{2}T'     # a line matching this starts a new event
      max_lines: 500                       # safety cap
      timeout: 5s                          # flush an in-progress event after 5s of silence

  - app: python-app
    path: /var/log/gunicorn.log
    multiline:
      mode: indent                         # any line starting with whitespace is a continuation
      timeout: 5s

  - app: stack-server
    path: /var/log/server.log
    multiline:
      mode: continuation_regex
      pattern: '^\s+at '                   # line matches → continuation of previous
      timeout: 5s
```

Three modes:

- **`start_regex`** — a matching line begins a new event; non-matching lines append to the current event. Idiomatic for timestamp-prefixed logs.
- **`continuation_regex`** — a matching line extends the current event; non-matching lines start a new one. Idiomatic for Java stacks where every frame starts with `<whitespace>at `.
- **`indent`** — any line beginning with whitespace extends the current event. Idiomatic for Python tracebacks, YAML-ish output, etc.

Guardrails on every mode:

- **`max_lines`** (default 500) — cap on stitched event size; hitting the cap flushes what we have and starts a new buffer.
- **`max_bytes`** (default 64 KB) — same idea for runaway output.
- **`timeout`** (default 5s) — if no new line has arrived for the timeout, flush the in-progress event so the shell sees it promptly. A stuck event otherwise hangs in the agent indefinitely.

## Emitted row shape

A stitched event is a single `LogRow` whose `Raw` field is the joined lines with `\n` between them; the coordinator splits it into lines at ingest ([line-storage](line-storage.md)). The timestamp comes from the first line (per the existing schema). `line_num` is the first line's number; `line_num_end` is a new field that carries the last line's number so the shell can tell the user how wide an event was.

This is additive; the coordinator ignores `line_num_end` if schema doesn't declare it, and existing single-line events are unchanged (they just have `line_num == line_num_end`).

## Components to build

1. **`weftd/tailer.go`** — new `multilineBuffer` struct with a flush-on-match/timeout/size loop. Wraps the existing `Lines()` channel; consumers see stitched events.
2. **Config parsing** — `SourceConfig.Multiline` struct with mode, pattern, caps. Compile regex at load time.
3. **`weftd/journal.go`** — apply the same stitching to the journal tailer. systemd journal entries already have a `MESSAGE=` field; multi-line `MESSAGE` values come through as one entry, but multi-line *events spanning multiple entries* (Python tracebacks logged line-by-line to systemd) still need stitching at the agent.
4. **Schema-driven default** — if the per-source `format: json`, multi-line is off by default (one JSON per line is the contract). If `format: text` and no `multiline:` is declared, off. Opt-in only; never surprising.
5. **Dropped-line reporting** — when `max_lines`/`max_bytes` truncates an event, log a weftd-level warning plus an `ingestion_errors` row so customers can see that stitching hit a cap.
6. **Docs** — `docs/manual/weftd.md` gains a "Multi-line events" subsection with one example per mode and the cap recommendations.

## Testing

The golden path is easy to get wrong; three test fixtures go in `cmd/weftd/`:

1. `multiline_start_regex_test.go` — timestamped Python tracebacks; assert 1 event out per traceback in.
2. `multiline_continuation_test.go` — Java stack; assert the `at ` frames stitch into the preceding exception line.
3. `multiline_timeout_test.go` — a feed that stops mid-event; assert the in-progress event flushes after `timeout`.

## Estimated effort

- `multilineBuffer` in `tailer.go`: **1 day**
- Config + compile: **0.25 day**
- Journal tailer wiring: **0.25 day**
- Schema-driven defaults: **0.25 day**
- Cap + drop reporting: **0.5 day**
- Docs + three test fixtures: **0.75 day**

**Total: ~3 days**.

## Non-goals

- **No multi-line on the CloudWatch path.** CloudWatch events already arrive as single items (the CW Agent or Lambda-side logger is responsible for stitching before sending). If a customer's upstream CW events are unstitched, the fix is at the CW Agent, not here.
- **No multi-line on the HTTP ingest path.** Clients posting NDJSON are responsible for stitching client-side. We're a receiver, not a parser-of-raw-streams there.
- **No "auto-detect multi-line format".** We considered heuristics (if >N% of lines start with whitespace, enable indent mode). Heuristics on live data silently get it wrong in awful ways; explicit config wins.
- **No runtime rule reload.** Multi-line config is read at weftd startup; changing it requires a restart. Same as source config today.
