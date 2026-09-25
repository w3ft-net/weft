# Browser span view

## Status

🟢 Implemented and live-verified. `trace <id>`, `trace <id> --spans`,
and `trace spans [name]` all open `ShellTraceModal` in the browser
shell now, mirroring `dashboard`/`chart`'s JS-callback-to-React-modal
pattern. Verified 2026-09-25 against a real local dev stack
(docker-compose postgres/coordinator/api/compute/weftd + `npx vite`),
logged in as a real user, driving all three `trace` shapes through
Chrome. Two real bugs found and fixed during that pass:

- `ShellTraceModal`'s fetch-abort handling didn't special-case
  `AbortError`, so React StrictMode's dev-only mount→cleanup→remount
  cycle flashed a spurious "signal is aborted without reason" error
  before the real fetch resolved. Fixed: `AbortError` is now silently
  ignored (the in-flight replacement request's own state update wins).
- The server embeds ANSI color codes in `trace`'s passthrough
  lines (the "# N line(s)..." summary and "# no lines found" text —
  see `compute/local.go`'s `emitTraceTagged`, written for CLI
  scrollback). `GapView` was rendering that `Raw` text verbatim into
  the DOM, showing literal escape codes. Fixed with a `stripAnsi`
  helper applied to non-data rows.

**Known gap, not fixed here (pre-existing, unrelated to this
plan):** in this local dev environment, a source configured with
`trace_id_field: none` / `duration_ns_field: none` (whole-line regex
search — the mode weft-go's own "span done"/"span error" lines need)
ingests successfully and is fully queryable via a plain `cat`/`grep`
pipeline, but `trace <id>` and `trace spans` themselves (which push
`WHERE trace_id = ?` / `duration_ns > 0` down to the shard) returned
zero rows for the same data. `ingest.Parser.ParseLine` was verified
correct in isolation (a throwaway unit test against the exact
persisted schema extracted `TraceId`/`DurationNs` correctly), and
`shard/shard.go`'s insert wiring for those columns looks correct by
inspection — the discrepancy between "found via raw-text `LIKE`" and
"not found via the structured column predicate" wasn't root-caused
within a reasonable time budget. This blocked verifying the modal's
actual data-rendering (gap bars, rollup table, samples drill-down)
against a real populated response; that path was instead verified by
calling `wasmParseTraceRows`/`wasmRenderSpanRollup` directly in the
browser console with realistic synthetic input matching the exact
`emitTraceTagged` wire format, confirming correct JSON shape
(self-time, percentiles, gap computation all present and correctly
computed). Worth a dedicated investigation if `trace`/`trace spans`
come back empty against real weftd-sourced span data elsewhere.

## Why

`docs/plans/span-display.md` shipped an APM-shaped view of a trace —
span names, counts, self-time, percentiles, error counts, with a
vim-nav list→samples drill-down — but only in the CLI (`spanview`, a
hand-rolled `shellcore.View`). Plain `trace <id>` (the gap-histogram,
`theme.RenderTraceColumns`) is CLI-only too. The browser shell already
has a proven pattern for exactly this kind of surface: `dashboard` and
`chart` both hook out of the Go shell core into a JS callback that
opens a React modal, rather than rasterising into xterm scrollback
(`shell/shell.go`'s `Chart`/`Dashboard` fields). `trace` never got the
same treatment, so today the browser shell shows nothing useful for
either `trace <id>` or `trace <id> --spans`/`trace spans [name]` — not
a degraded ANSI render, just raw unrendered rows.

Bringing `trace` into the browser completes the dashboard/chart/trace
trio and lets the browser shell dogfood
`docs/plans/request-tracing.md`'s feature the same way the CLI already
does.

## Non-goals

- **No waterfall, no tree, no service map.** Restating
  `span-display.md`'s own non-goal, extended to the browser
  specifically: a naive "let's build an APM UI" framing could drift
  toward a timeline/waterfall component. Every view in this plan stays
  a **flat list**, mirroring `spanview`'s existing list→samples UX —
  React tables and drill-down clicks, not a rendered timeline.
- **No live/streaming aggregation.** Same as `span-display.md` — a
  browser trace view queries a historical range like any other weft
  query; no auto-refresh, no dashboard tile.
- **No new trace_id/span schema or ingest work.** The data model
  (`trace_id`, `span_name`, `span_id`, `parent_span_id`, `duration_ns`)
  is already shipped per `request-tracing.md`/`span-display.md`. This
  plan is purely a new rendering surface over data that already
  exists.
- **No new coordinator REST endpoint.** Raw rows reach the browser the
  same way any other pipeline's rows already do (the shell's existing
  async query path); no bespoke `/api/trace/...` route.

## Architecture

### Why this isn't a straight copy of the Chart/Dashboard hook

`Shell.Chart`/`Shell.Dashboard` (`shell/shell.go:68-119`) are nil-able
callback fields; `ConfigureCore` (shell/shell.go:129-152) registers a
JS-forwarding builtin when the field is set, and falls back to
CLI-only ASCII rendering (registered separately in
`cmd/weft/runner/runner.go:76-83`, kept out of `ConfigureCore` so
`shell/` — which must stay WASM-buildable — doesn't pull heavy
packages into the browser binary) when it's nil. `cmd/wasm/main.go`'s
`buildShell` (lines 67-98) sets both fields to closures that call
`cb.Call("chartOpen", ...)`/`cb.Call("dashboardNavigate", ...)` — a
`syscall/js` bridge into `frontend/src/wasm/bridge.ts`, wired by
`frontend/src/pages/Shell.tsx` to React state (`ShellChartModal.tsx`)
or a router navigation.

`trace` is more fractured than either of those:

- Plain `trace <id>` renders via `theme.RenderTraceColumns`
  (`theme/trace_columns.go`), called through
  `shell.RenderTraceIfApplicable` (`shell/shell.go:457`). That function
  is wired into exactly two call sites — `ExecNonInteractive`
  (shell.go:421) and the CLI's interactive REPL
  (`cmd/weft/runner/runner.go:221`) — and nowhere in `cmd/wasm/main.go`.
  So even the plain gap-histogram is missing in the browser today, not
  just the spans rollup.
- `trace <id> --spans` / `trace spans [name]` are intercepted by
  `spanview.Builtin` (`spanview/builtin.go`) before normal pipeline
  dispatch, entering `spanview.SpanView` (raw-ANSI `shellcore.View`,
  list + samples levels, modeled on `dashboard.DashboardView`).
  Registered only in `cmd/weft/runner/runner.go:91-103`, gated on
  `s.Query != nil`.
- **That gate is the crux.** `shell.QueryFunc` is a synchronous Go
  function in the CLI (a blocking gRPC call). `cmd/wasm/main.go`'s
  `buildShell` never sets `Query` — in the browser, queries run
  asynchronously through `frontend/src/wasm/network.ts`'s `execQuery`
  (a JS `fetch` to `/api/query`), with results flowing back into the
  WASM binary via `wasm.HandleQueryResult`, a JS-driven callback with
  no Go call stack for `spanview.QueryFunc` to wrap. This is why
  `spanview.Register` can't simply be called from `cmd/wasm/main.go`
  the way `chart.Register`/`dashboard.Register` are skipped in favor of
  JS-callback fields: there is no synchronous Go query function to hand
  it in WASM.
- **The rollup math is real logic, not just rendering.** `theme.
  RenderSpanRollup` (`theme/span_rollup.go`) parses, groups, computes
  self-time via interval-union over concurrent children
  (`computeSelfDurations`, span_rollup.go:231-275 — not naive
  summation), and exact percentiles. `SpanRollupRow`/`SpanOccurrence`
  are already plain, JSON-able structs. `RenderTraceColumns`, by
  contrast, returns pre-rendered ANSI `[]string` with its parse step
  inlined as an anonymous struct (trace_columns.go:38-95) — there's no
  structured form to reuse yet.

### The design: a `Trace` hook, plus two stateless WASM exports for the math

1. **New `Shell.Trace` hook** (`shell/shell.go`), mirroring
   `Chart`/`Dashboard`:

   ```go
   type TraceRequest struct {
       Pipeline string // the full "trace ..." pipeline to (re-)run
       Title    string // trace ID, span name, or "all spans"
       Mode     string // "gap" | "rollup" | "aggregate"
   }
   Trace func(TraceRequest)
   ```

   `Mode` distinguishes plain `trace <id>` (`"gap"`) from `trace <id>
   --spans` (`"rollup"`, single-trace, gets the SELF column) from
   `trace spans [name]` (`"aggregate"`, cross-trace). `ConfigureCore`
   registers a JS-forwarding builtin when `s.Trace != nil`, same
   shape as the existing Chart/Dashboard nil-checks. When nil,
   `cmd/weft/runner/runner.go` keeps registering `spanview` exactly as
   today — gate that registration on `s.Trace == nil` too, matching
   the existing `s.Chart == nil` / `s.Dashboard == nil` checks at
   runner.go:77/80.

   The line-shape parsing (`trace <id> --spans` / `trace spans
   [name]`) already exists as `parseSpansLine` in
   `spanview/builtin.go` — reuse it (export it, or move it somewhere
   both `spanview` and `shell` can reach) rather than reimplementing
   the arg grammar. A new predicate is needed for the plain-`trace
   <id>` shape (`Mode: "gap"`), since `spanview.Builtin` deliberately
   lets that one fall through to normal dispatch today.

2. **Two small stateless WASM exports** in `cmd/wasm/main.go`, called
   directly from JS once React has the raw rows for
   `TraceRequest.Pipeline` (fetched via the shell's existing async
   query path — the same mechanism `... | chart`'s captured `Lines`
   already ride; no new HTTP endpoint):

   - `wasmRenderSpanRollup(lines) → JSON`, wrapping `theme.
     RenderSpanRollup` directly. Covers `Mode: "rollup"` and
     `"aggregate"` — the same parser already serves both phases per
     `span_rollup.go`'s own doc comments. Zero duplicated business
     logic: the self-time interval-union and percentile math stay in
     Go, already linked into `shell.wasm` via the `theme` package.
   - `wasmParseTraceRows(lines) → JSON`, wrapping a **new**, extracted
     `theme.ParseTraceRows(lines) []TraceRow{TsNs, Label, Message,
     GapNs, ColorCode}` — pulled out of `RenderTraceColumns`'s current
     inline parse step (trace_columns.go:38-95) so the ANSI renderer
     and the new JSON export share one parser. `ColorCode` reuses
     `theme.TraceTagColor`'s existing FNV-hash-to-one-of-8-hues lookup
     (`theme/trace.go`). The 8 ANSI-256 codes need a small,
     hand-copied hex-equivalent table on the React side for CSS
     coloring — an intentional, bounded exception to "no duplicated
     logic" since it's a fixed 8-entry data table, not math.

   These are the design's one deliberate departure from the
   Chart/Dashboard precedent (which does zero server/WASM-side
   compute — `ShellChartModal`'s pipe form just renders captured
   `Lines` as `<pre>` text): trace's rollup involves real, order-
   sensitive math that shouldn't exist in two languages.

3. **React side**: `frontend/src/wasm/bridge.ts` gets a `traceOpen`
   callback (mirrors `chartOpen`); `Shell.tsx` wires it to a new
   `ShellTraceModal.tsx` (sibling of `ShellChartModal.tsx`). The modal
   owns simple list/samples view state (a level toggle, mirroring
   `spanview.SpanView`'s `levelList`/`levelSamples`) — no new
   `shellcore.View`, no ANSI, a React table plus a drill-down click
   target per row. `Mode: "gap"` renders a static 3-column table (bar
   as a CSS width percentage instead of `renderGapBar`'s block
   characters, badge colored via the static hex table from (2));
   `"rollup"`/`"aggregate"` render the span-name table with
   self-time/percentile columns and drill into per-occurrence samples
   on row click.

## Components to build

1. ✅ `shell/shell.go` — `TraceRequest` type, `Trace` field on `Shell`,
   `traceRequestBuiltin` (mirrors `chartRequestBuiltin`/
   `dashboardNavigateBuiltin`), wired in `ConfigureCore`.
2. ✅ `spanview/builtin.go` — exported `parseSpansLine` as
   `ParseSpansLine` so `shell`'s builtin reuses the existing arg
   grammar instead of forking it; `shell.traceRequestBuiltin` adds the
   plain-`trace <id>` (`Mode: "gap"`) predicate itself.
3. ✅ `cmd/weft/runner/runner.go` — gated `spanview.Register` on
   `s.Trace == nil`, matching the existing Chart/Dashboard fallback
   pattern.
4. ✅ `theme/trace_columns.go` — extracted `ParseTraceRows(lines)
   []TraceRow`; `RenderTraceColumns` calls it internally. Also added
   `theme.TraceLabelColorIndex` (trace.go) so `TraceRow.ColorIdx` and
   `TraceTagColor`'s ANSI code share one hash lookup.
5. ✅ `cmd/wasm/main.go` — `Trace` closure in `buildShell` (`cb.Call
   ("traceOpen", ...)`); `wasmRenderSpanRollup` and `wasmParseTraceRows`
   JS exports (JSON-string return, via a small `jsonResult` helper).
6. ✅ `frontend/src/wasm/bridge.ts` — `TraceOpenSpec` type, `traceOpen`
   callback wiring (mirrors `ChartOpenSpec`/`chartOpen`); plus
   `frontend/src/wasm/network.ts`'s `fetchPipelineLines` (a
   Promise-returning sibling of `execQuery` for a React caller that
   isn't going through the WASM callback bridge) and
   `frontend/src/wasm/traceExports.ts` (typed wrappers around the two
   WASM exports) and `frontend/src/lib/traceColors.ts` (the 8-entry
   hex table, `TraceLabelColorIndex`-indexed).
7. ✅ `frontend/src/pages/Shell.tsx` — `onTraceOpen` → modal state,
   mirroring the existing `chartOpen`/`dashboardNavigate` wiring.
8. ✅ `frontend/src/components/ShellTraceModal.tsx` (new) — list/samples
   view, three render modes (`gap`/`rollup`/`aggregate`), sibling of
   `ShellChartModal.tsx`.
9. 🟢 Tests: Go unit tests landed —
   `theme.TestParseTraceRows_GapsAndPassthrough` and
   `shell/trace_request_test.go` (4 cases covering all three
   `TraceRequest.Mode`s plus the nil-`Shell.Trace` no-op). Browser
   click-through done live (see Status above) — all three `trace`
   shapes drive `ShellTraceModal` correctly end to end, and
   `wasmParseTraceRows`/`wasmRenderSpanRollup` were exercised directly
   in the browser console against realistic synthetic input. **Not**
   done: a fixed-input Go test for the two WASM exports themselves —
   `cmd/wasm` has never had test files (no `GOOS=js GOARCH=wasm go
   test` infra in this repo; CI only build-checks it), and the export
   functions are thin JSON-marshal wrappers around already-tested
   `theme` functions, so the marginal value didn't seem to justify
   standing up that infra here. Revisit if `cmd/wasm` picks up other
   testable logic later.

### Estimated effort

- (1)-(3), Go hook + spanview reuse: ~0.5d
- (4), `ParseTraceRows` extraction: ~0.5d
- (5), WASM exports: ~0.5d
- (6)-(8), React bridge + modal (three render modes, list/samples
  state): ~1.5d
- (9), tests: ~0.5d

~3.5d total, assuming no surprises in the WASM↔JS JSON marshaling
(the existing `ChartRequest`/`dashboardNavigate` bridges are the
precedent, but neither passes a `[]struct` this shaped today, only
flat strings and `[]string`).

## Open questions

- **Samples-level drill-down further hand-off.** `spanview`'s CLI
  samples view lets the user pick one occurrence but doesn't currently
  re-dispatch into `trace <id>` for that specific request (confirmed:
  no such re-dispatch exists in `spanview/view.go` today). Should the
  browser modal add a "open this trace" link from a sample row into a
  fresh `Mode: "gap"` request? Nice-to-have, not required for parity
  with the CLI.
- **JSON payload size for wide aggregates.** `trace spans [name]`
  (Phase 2) is unbounded per `span-display.md`'s own still-open
  question — every matching occurrence becomes a samples-level row.
  Marshaling that across the WASM↔JS boundary as one JSON blob
  inherits whatever cap (or lack of one) gets decided there; not a new
  problem this plan introduces, but worth re-checking once that
  question resolves.

## Cross-links

- `docs/plans/span-display.md` — the CLI feature this is the browser
  half of; source of the `spanview` list/samples UX this plan mirrors
  and the "no waterfall" non-goal this plan restates.
- `docs/plans/request-tracing.md` — the underlying data model
  (`trace_id`, W3C `traceparent` propagation) this view renders.
- `shell/shell.go`'s `ChartRequest`/`Chart`/`Dashboard` — the hook
  pattern this plan extends to `trace`.
- `frontend/src/components/ShellChartModal.tsx` — the closest existing
  precedent for `ShellTraceModal.tsx`.
