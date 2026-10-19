# SDK line header: structured fields on file lines, no wizard

## Status

🟡 Designed, not started. Supersedes the "socket envelope" part of
[request-tracing](request-tracing.md) (the `\x1e<trace_id>\x1e` frame on the
records socket); that plan's `trace <id>` query and reserved column are
unchanged.

## Problem

An app that uses the weft SDK already knows each line's timestamp, level,
trace ID and span, yet today:

- the SDK's logging helpers stamp `trace_id=...` into the line **text**
  (`weft-go` `SlogHandler`, `weft-py` `TraceIDFilter`), so the schema wizard
  must then re-extract it from the text it was just written into;
- the wizard asks about timestamp, level and trace ID (and spans) for every
  new file source (`cmd/weft/setup/tree.go`), and weftd **blocks** the source
  in `onboardAndRun` (`WaitForConfiguration`) until someone finishes it;
- the only structural path, the socket envelope, is best-effort (no replay),
  so choosing it gives up the file source's at-least-once delivery. Weft's
  own hosts hit this: `startup.sh` enables both `socket:` and a
  `/opt/app/logs/*.log` file source, and resampling the coordinator still
  walks through trace-ID selection.

The result is an ambiguous middle: the work of extraction is done without the
benefit of the SDK knowing the answer.

## Design

The SDK writes each record to its log file with a **line header** carrying
the fields it knows. weftd, which already tails the file, strips the header
and fills `LogRow`'s own fields before the row goes on the wire. Nothing
about this involves the records socket.

### Format

```
\x1e v1 \x1f ts \x1f lvl \x1f trace \x1f span_name \x1f span_id \x1f parent_id \x1f dur_ns \x1f fmt \x1e body \n
```

- The outer `\x1e` pair brackets the header. Inside, `\x1f` separates
  values. The first value is the version, and **the version fixes the order
  and meaning of the rest**.
- An empty value means "not supplied": empty `ts` is ingest time, empty
  `lvl` is no level.
- Encodings: `ts` is unix nanoseconds (decimal), `dur_ns` a decimal integer,
  IDs are hex, `lvl` is the SDK's level word (upper-cased by weftd, as
  `ingest.Parser` does), `fmt` is `json` or `text`.
- The SDK refuses any value containing `\x1e`, `\x1f` or `\n` (the Go and
  Python SDKs already reject `\x1e` and `\n` in trace IDs).
- The header is recognized **only at the start of a line**.
- v1's values map one-to-one onto `LogRow` and onto the wizard's probes:

| v1 value | `LogRow` field | Wizard probe it replaces |
|---|---|---|
| `ts` | `ts_unix_ns` | `ProbeTimestamp` |
| `lvl` | `level` | `ProbeLevel` |
| `trace` | `trace_id` | `ProbeTraceID` |
| `span_name` | `span_name` | `ProbeSpanName` |
| `span_id` | `span_id` | `ProbeSpanID` |
| `parent_id` | `parent_span_id` | `ProbeParentSpanID` |
| `dur_ns` | `duration_ns` | `ProbeDurationNs` |
| `fmt` | selects `payload_json` handling | the "are these JSON?" question |

The proto needs no new fields; every column above already exists.

### Compatibility and versioning

SDK and weftd versions will skew (weftd auto-updates, SDKs are pinned by
customers), so the rules are explicit:

1. **Versions are append-only.** v2 may add values after v1's; it may not
   reorder or redefine any v1 value.
2. **Unknown higher version.** A weftd that knows versions up to K reads the
   first K-worth of values from a higher-version header, ignores the rest,
   and keeps the body. Because the header is closed by the second `\x1e`,
   the body is always findable. The source's status reports "SDK is newer
   than this weftd; upgrade".
3. **Breaking change.** A reorder or redefinition needs a new leading
   sentinel, not a version bump. Expected to be never.
4. **Legacy envelope.** `\x1eTRACE\x1eline` has no `\x1f` and no `v<digits>`
   first value. weftd keeps reading that shape as "trace ID only". (This
   assumes no customer depends on the socket envelope beyond that; `w3ft`
   0.2.0 is released, but the tracing SDK surface is marked not started in
   request-tracing.md.)
5. **Old weftd, new SDK.** A weftd that predates header support reads the
   control bytes into `raw`, and its wizard samples them. Mitigation is
   rollout order: ship weftd with header support first, then the SDKs, and
   raise `WeftdHardFloor` (`coordinator/service/server.go`) to that version.
   The SDK docs state the minimum weftd.
6. **An SDK file is SDK-only.** The handler writes to its own file and
   nothing else does, so every line has a header. A headerless line in an
   SDK-fed source is a fault, not a design case: weftd ships it with ingest
   time and no level, and nothing prompts. The expected causes are a torn
   line after rotation or a checkpoint resume, concurrent writers
   interleaving long lines, and a message containing a raw newline (the
   handler must escape newlines; a JSON body does so already, and `text`
   bodies must too).

### weftd

`buildLogRow` (`cmd/weftd/main.go`) is the one place a line becomes a row.
It:

1. strips the header and sets `TsUnixNs`, `Level`, `TraceId`, `SpanName`,
   `SpanId`, `ParentSpanId` and `DurationNs` from it;
2. for `fmt=json`, validates the body and sets `payload_json`; an invalid
   body falls back to `raw` only;
3. runs the schema parser only for fields the header left empty.

`raw` is the body only. The header never reaches the coordinator or shards,
which keep stamping namespace, customer and env from the token.

### Onboarding without the wizard

weftd marks a source **SDK-fed** the first time it sees a header. For such a
source:

- the sample it submits has headers stripped and carries a flag naming the
  derived fields (a new field on `SubmitSampleRequest`);
- the coordinator configures the source immediately with a synthesized
  schema (`json` or `text` from `fmt`, nothing to extract), so
  `onboardAndRun` does not call `WaitForConfiguration` and the tailer starts
  at once;
- the wizard never opens. `weft source configure` shows "timestamp, level,
  trace ID and spans come from the weft SDK (header v1)";
- a later schema edit (promoting an attribute) is an ordinary edit.

A source with no headered lines behaves exactly as today.

### SDKs

- **weft-go:** a `slog.Handler` that writes headered lines to a file (or an
  `io.Writer` the caller supplies), reading the trace and span from `ctx`.
  `slog.SetDefault` also routes the stdlib `log` package through it.
- **weft-py:** a `logging.Handler` doing the same, reading the contextvar.
- The text-stamping helpers (`SlogHandler`'s `trace_id` attr,
  `TraceIDFilter`) are removed.
- The records socket stays for `Send`, heartbeats and runtime stats; it is
  not part of this design.
- Body format: JSON (`{"msg": "...", ...attrs}`) so `jq` works and attributes
  are queryable; `fmt=text` remains for `SendLine`-style raw strings.

### File ownership and raw output

The handler writes to its own file (default: next to the app's other logs;
configurable), never to stdout. Raw stdout and stderr (panics, stdlib
prints, runtime noise) go to a separate file or the journal and are an
ordinary source with the wizard, under their own kind (for example
`stdio`). The two sources sit side by side under the same service, so there
is no mixed file and no ambiguous middle.

### Weft's own hosts

Today `weft-coordinator.service` (and the other roles) send stdout and
stderr to `/opt/app/logs/<role>.log`, which `startup.sh` tails as one source.
After this change the structured logs go to a new SDK-written file under the
same service, which is SDK-fed, so resampling no longer asks for a trace ID.
`<role>.log` keeps holding raw stdout/stderr and stays wizard-driven. The
`startup.sh` source list and the glob/explicit deconfliction need to name
both files.

## Out of scope

- The records socket's protocol and best-effort delivery.
- Capturing arbitrary stdout/stderr in-process (redirecting `sys.stdout` or
  fd 1/2); considered and not needed here because headered lines reach the
  file through the logging handler.
- Languages other than Go and Python; their files remain wizard-driven.

## Open questions

- Exact `WeftdHardFloor` value and whether to enforce it from the SDK side.
- The default path and kind name for the SDK file and for the raw stdio file.
- Release sequencing across `weft`, `weft-go` and `weft-py`.

## Work

1. weftd: header parser and `buildLogRow` changes, with tests for every
   compatibility rule above.
2. Proto and coordinator: derived-fields flag on `SubmitSampleRequest`,
   synthesized schema, source status line.
3. `onboardAndRun`: skip the wait for SDK-fed sources.
4. `weft-go` and `weft-py`: file handlers; remove the stamping helpers.
5. Wizard and `weft source configure`: derived-fields display.
6. Docs: `connect-source.md` ("weftd forces a schema" no longer true for
   SDK-fed sources), `weftd.md`.
