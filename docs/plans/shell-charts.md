# Plan: Charts in the shell

In-terminal line / sparkline / bar charts so an operator can `chart
<watch>` from the shell instead of leaving for the web UI's Watch
page. Live mode redraws in place at 1 Hz over a stable cursor anchor
— same ergonomic as `less` (press `q` to exit), no new transport.

## Why add this

Watches are first-class shell objects today: `vi /cron.d/<name>`
edits one, `cat /cron.d/<name>` shows its source, `weft.latest`
reads the most recent value. The one thing you can't do in the
shell is **see** the result series — for that the operator has to
flip to a browser tab and navigate to `/watch/<name>`. The web tab
is fine for a deep look, but for the dozen times a day a customer
just wants to glance at a number's trend while triaging, it's
friction we don't need.

The shell already has all the data: `WatchResultsProvider.GetWatchResults`
is plumbed through both renderers (CLI → gRPC → coordinator;
WASM → XHR → API). The piece we don't have is a way to turn a
slice of `(timestamp, value)` into ANSI suitable for the shell's
output stream.

## Approach choice (and the killed alternatives)

We have three real ways to get terminal charts into shellcore:

**A. Port the rendering primitives we need from terminal-chart
(Rust) to Go.** ~1000–1500 LOC under `shellcore/chart/`. Compiles
into the same binary as today, runs identically under CLI and
GOOS=js wasm. Translation, not invention — termichart's
`braille.rs`, `block.rs`, `axis.rs`, `cell.rs`, `sparkline.rs`,
and the line/bar builders are tightly-scoped pure code that maps
to Go cleanly.

**B. Embed `termichart-wasm` via a Go wasm host (wazero) in the
CLI, and load a second wasm module alongside `shell.wasm` in the
browser.** Zero porting, full feature parity with termichart. But:
the CLI binary grows ~5–10 MB and gains a wasm runtime
(currently it has none); the browser path needs JS glue between
two wasm modules; we inherit termichart's API surface (themes,
candlestick, exchange clients) for features we'll never use.

**C. Server-side render to ANSI on the coordinator; the shell
just paints the bytes.** One copy of the chart code, no
client-side build for it. Loses fidelity to terminal width
(the server can't infer truecolor support, exact column count
under resize), and a 1 Hz live mode would now be 1 Hz of WS
chatter to the coordinator for what is locally-derivable from
data the client already has.

**Going with A.** Two reasons: it's consistent with shellcore's
"write ANSI directly" rule (`feedback_charm_tui.md` —
shellcore is renderer-agnostic, doesn't pull in TUI runtimes),
and it avoids paying the one-time integration cost of a
runtime-embedded wasm just to use ~10% of termichart's surface.
The Rust library stays as reference — we read its render
algorithms when porting, we don't depend on its build.

If A turns out to be more work than the budget below allows,
the fallback isn't B or C; it's "ship sparkline-only" — that
alone covers `chart <watch>` and the `ls /cron.d/`
side-by-side glyph and is ~150 lines.

## Architecture

Two halves: a pure rendering library, and a small live-redraw
mode that drives it.

### Rendering library (pure Go, no I/O)

```
shellcore/chart/
  cell.go         // Cell grid: (col,row) → rune+ANSI style
  block.go        // ▁▂▃▄▅▆▇█ ramps for bar / 2x1 line
  braille.go      // ⠁..⡿ 2x4 dots for high-density line
  axis.go         // y-tick labels, x-axis framing, padding math
  line.go         // line/area chart builder, braille-backed
  sparkline.go    // one-row Unicode block sparkline
  bar.go          // vertical & horizontal bar
  builder.go      // small fluent surface (mirrors termichart's ChartBuilder shape)
```

Public surface kept narrow:

```go
out, _ := chart.Line(80, 18).
    Title("error rate / hour").
    Points(pts).
    YThreshold(50, chart.SeverityWarn).
    Render()      // returns ANSI string with embedded newlines
```

Output is **one string with embedded newlines**, same as
`HelpBuiltins`/`docs_gen.go`/`less` content. That means chart
output composes with `less`, gets paged when long, lands in
scrollback identically to any other command's stdout. No new
output channel.

The CLI and the wasm renderer both write ANSI through the same
single `write([]byte)` callback already on `ShellCore` — neither
needs to know a chart was rendered.

### Live mode (`ModeChart`)

Modeled directly after `shellcore/less.go` (same shape: a mode,
a state struct, a key handler, a draw routine). When the user
runs `chart <watch> --follow`:

1. Core enters `ModeChart`.
2. First fetch + draw paints a `height`-line region; the cursor
   is left at a known anchor (column 0, row = anchor).
3. The renderer (CLI loop or JS `setInterval`) calls
   `core.ChartTick()` once per second.
4. `ChartTick` re-fetches via `WatchResultsProvider.GetWatchResults`,
   re-renders, repositions cursor to anchor, emits `\x1b[?2026h…
   \x1b[?2026l` (DEC sync update) to avoid tearing, and rewrites
   the region.
5. `q` / `Esc` / `Ctrl+C` exits the mode, leaves the last frame
   in scrollback, redraws the prompt.

Polling cadence is 1 Hz (configurable via `--interval`); watches
mostly fire at minute boundaries so 1 Hz is wasteful in absolute
terms but cheap in actual cost (the API call is a single
`SELECT … LIMIT N` server-side, already cached at the coordinator
for dashboard rendering). The 1 Hz tick lets a manual run of a
watch (which the operator might trigger from another tab) appear
within a second.

Why poll, not WebSocket push:

- The endpoint already exists (`GET /api/watches/{name}/results`)
  and is already used by the dashboard.
- A WS subscription means a new server route, a new client
  subscription state machine, fan-out at the coordinator, and
  reconnect/backfill semantics. None of that buys us anything
  visible at 1 Hz.
- If polling load shows up in metrics, the next step is an
  `If-Modified-Since` short-circuit on `/results` (304 unless
  a new result landed) — cheaper than a transport rewrite.

### Tick driver: reuse the spinner heartbeat

The CLI renderer and `cmd/wasm/main.go` both already drive a
heartbeat into shellcore for spinner animation
(`SpinnerTick()`, called every 80 ms in CLI, every 80 ms via JS
`setInterval` in the browser). We piggyback on that:

```go
// shellcore/core.go (sketch)
func (c *ShellCore) SpinnerTick() {
    c.spinnerFrame++
    if c.mode == ModeChart {
        c.chartMaybeTick()  // throttles to ~1 Hz internally
    }
}
```

No new tick API on the renderer side, no new JS interval. The
chart mode owns its own throttling.

### Shell surface

One new builtin in `shellcore/core.go` (parsed alongside
`history`, `range`):

```
chart <name> [-t line|sparkline|bar] [-n LIMIT] [-i INTERVAL] [-f|--follow]

  -t line       chart type (default: line for >1 point, sparkline for 1-row)
  -n 50         number of recent results to plot (default: 50)
  -i 1s         poll interval in --follow mode (default: 1s)
  -f, --follow  redraw on tick instead of one-shot
```

Path form is also accepted so it sits naturally next to `cat`/`vi`:

```
chart /cron.d/error-rate
chart /cron.d/error-rate -f
```

Multi-row watches (the `-k` / `-t` alert mode) auto-pick a bar
chart with one bar per tag for the most-recent tick, and a
small-multiples line chart for the historical view.

A nice secondary use: `ls -l /cron.d/` adds a single-line
sparkline beside each entry. That's an extra line of code per
entry once the sparkline primitive exists.

## Components to build

1. **`shellcore/chart/` rendering primitives.**  Translate
   termichart-render's `cell.rs`, `block.rs`, `braille.rs`,
   `axis.rs` and termichart-charts' `line.rs`, `sparkline.rs`,
   `bar.rs`, `builder.rs` to Go. Skip everything else
   (candlestick, dashboard, interactive, tui, portfolio,
   histogram). **~3 days.**
2. **`shellcore/chart/golden_test.go`** — pin the ANSI output for
   each chart type at small (40×8) and medium (80×20) sizes.
   Golden files under `shellcore/chart/testdata/`. Future
   refactors can't quietly drift the rendering. **~0.5 day.**
3. **`shellcore/mode_chart.go`** — `ModeChart` state, key handler,
   `chartMaybeTick`, draw routine. Direct analog of `less.go`.
   **~1 day.**
4. **`shellcore/core.go` builtin wiring.** Parse `chart …`,
   resolve `<name>` vs. `/cron.d/<name>`, fetch initial results
   via `c.shellFS.(WatchResultsProvider)`, render once or enter
   `ModeChart`. **~0.5 day.**
5. **WASM bridge: enter/exit ModeChart hooks** in
   `frontend/src/wasm/bridge.ts`. The existing `wasmSpinnerTick`
   interval already runs at 80 ms — `chartMaybeTick` throttles
   internally, so no JS-side change is needed beyond letting the
   shell's `write` callback paint the redrawn region. **~0.25 day.**
6. **`ls -l /cron.d/` sparkline column.** Tiny addition to the
   `ls` builtin in `shellcore/ls_test.go`-tested code. Reads the
   last 30 results per watch, renders a 12-cell sparkline,
   inlines into the long-form ls row. **~0.5 day.**
7. **`docs/manual/shell-charts.md` + man-page entry** for the
   `chart` builtin (`docs_gen.go` regen). Cross-link from
   `help builtins` and `help cron`. **~0.5 day.**
8. **Smoke test in `cmd/weft/main.go` non-interactive path** so
   `weft sh -c "chart error-rate"` prints a chart and exits.
   **~0.25 day.**

**Total: ~6.5 days.**

## What we're explicitly not porting from terminal-chart

- **Candlestick / Heikin-Ashi.** Crypto-OHLCV oriented; weft has
  no use case.
- **Exchange clients (Binance, CoinGecko).** Live data path
  belongs to weft's existing watch results endpoint, not a
  third-party fetcher embedded in the chart library.
- **`crossterm`-driven `WatchMode` / `TuiApp` / `InteractiveChart`.**
  These take over the terminal with raw mode and own the event
  loop. That contradicts shellcore's design: the renderer owns
  the terminal, the core writes ANSI through a callback. Live
  mode here is implemented as a shellcore mode, not a separate
  app.
- **Themes (Dracula, Nord, Catppuccin, …).** The shell has a
  single palette today. If themes become a thing, they're a
  shell-wide concern, not a chart-only one.
- **Python / Node SDKs, full-WASM bindings.** We're not
  redistributing chart rendering; consumers are the CLI and the
  GOOS=js wasm shell, both of which speak Go.
- **`tungstenite` WebSocket streaming, JSON / CSV / streaming
  data adapters.** Replaced by the existing
  `WatchResultsProvider`.

## Non-goals

- **Charting arbitrary pipelines.** v1 source = a watch. A
  future `chart 'cat /log/api.log | grep ERROR | wc -l'` form
  would re-execute the pipeline on every tick, which is the
  computational shape of `dashboard-builder.md`'s tile system —
  out of scope here.
- **Mouse / scroll / zoom.** This isn't a TUI app.
- **Dashboards in the shell.** That's `docs/plans/dashboard-builder.md`'s
  territory; this plan stays at one chart per command.
- **New transport.** No WebSocket subscriptions, no SSE, no gRPC
  streaming. Just polling the endpoint that already exists.
- **Pager integration.** Charts always fit the terminal (see
  "Sizing rule" above). `less` is for text.
- **A Rust dependency.** No FFI, no second wasm module, no
  termichart binary in the build pipeline. The library is read
  for inspiration; the artifact is Go code in our tree.

## Cross-links

- `docs/plans/dashboard-builder.md` — once that ships, its
  `kind: sparkline` tile and the shell's `chart <watch>` should
  use visually-consistent rendering. The web side stays SVG; the
  shell side is ANSI; the *aesthetic* (color ramp for severity,
  threshold line style) wants to match. Worth a one-page style
  note when both have shipped.
- `docs/plans/materialized-views.md` — a materialized view that
  emits one number per bucket is structurally a watch result
  series. Once shipped, it's a natural follow-on `chart` source.

## Sizing rule: charts always fit the terminal

Charts are visual, not textual — paging a chart through `less`
fragments the visual and is the wrong shape. The rule:

- The chart's render dimensions default to **the terminal's
  current `(width, height)` minus a small reserve** (1 row for
  the prompt that returns afterward, 1 row for an optional
  status line in `--follow`).
- Explicit `--width` / `--height` flags clamp to the terminal
  if the user asks for something bigger.
- On resize (CLI: SIGWINCH; WASM: `wasmResize`), follow mode
  re-renders into the new dimensions on the next tick. One-shot
  output is already painted and stays as-is — the user resized
  *after* the fact, that's their problem.

This means the chart is *always* a single visible frame, never
paged. `less` integration is explicitly not a feature here.
