# Browser tmux (tiled shell panes) — plan

Status: **in progress** (kicked off 2026-07-22). Lead app: **weft**.

## Goal

Basic tmux-style multiplexing in the **browser** shell: split the terminal
into tiled panes, each an independent shell over a **shared filesystem**, with
a prefix key (`C-b`) for split/navigate/close/zoom and a status bar. Native CLI
tiling is explicitly **out of scope** for now — `weft` CLI users run real tmux.
The vimgym terminal (bubbletea) host is also out of scope.

## Why this is mostly cheap

A `shellcore.ShellCore` is already a self-contained pane: its own dims, its own
`write([]byte)` paint callback, its own `HandleEvent` input, its own
history/cwd/mode. In the browser, `write()` is piped straight into a
**full-screen xterm.js**, which is itself a VT parser + cell grid. Therefore:

- **The DOM is the compositor.** Tiling = N xterm instances in a CSS grid. No
  Go-side VT parser / cell-grid compositor is needed (that would only be needed
  for native-CLI tiling, which we are not doing).
- **Alt-screen is free per pane.** vim / less / `chart --follow` each use
  `\x1b[?1049h`; because every pane owns its own xterm, they "just work" side by
  side with no shellcore changes.

So the multiplexer is *mostly a frontend project*. The one non-frontend
requirement is that the WASM bridge must support **N cores over one shared
`MountFS`, addressable per pane** — today it is hard-wired single-instance.

## Where each piece lives

| Concern | Home |
|---|---|
| Pane geometry / CSS grid / focus ring / status bar | weft frontend (React) |
| Prefix-key FSM (`C-b` then split/nav/close/zoom) | weft frontend (React) |
| N cores keyed by pane id, shared `MountFS` | warp `pkg/shellcore/wasm` bridge + weft `cmd/wasm` |
| The cores themselves (resize/reflow, modes) | unchanged — free |

## The single warp change

`pkg/shellcore/wasm/boot.go` holds `var active *shellcore.ShellCore` and every
`wasm*` export operates on that one global (comment: single-instance is "a
deliberate design choice"). We replace the global with an **id-keyed registry**:

- New untagged `registry.go` (host-testable — no `syscall/js`): `map[id]*ShellCore`
  with `set/get/remove` and a notion of the "default" instance.
- `BootOpts` gains `Instance string` (empty = default; preserves today's behavior).
- Legacy exports keep operating on the default instance → **no ABI change, weft
  keeps building and the current single pane is byte-for-byte identical.**
- A later increment adds instance-scoped exports (`wasmPaneInput(id,data)`,
  `wasmPaneResize(id,cols,rows)`, `wasmPaneSpinnerTick(id)`, …) for multi-pane.

warp changes ship to weft via a version tag + pin bump (per repo convention).
Must keep `../weft` building at every step.

## weft `cmd/wasm` change

Today: one `wasm.Boot(...)`, one `NewMountFS()`, one `shell.Shell`. Multi-pane =
`Boot` once per pane with a distinct `Instance` and its own `write` callback, all
`ConfigureCore`'d against **one shared `MountFS`** (built once, reused) and one
HTTP coordinator/token/org. Shared FS gives real-tmux semantics: `echo hi > f`
in pane A is `cat f`-able in pane B.

## weft frontend: `<PaneGrid>` (the mux)

- Binary split-tree → flex/CSS grid; each leaf is an xterm bound to a `paneId`
  (`data-testid={`pane-${id}`}`).
- Prefix-key interceptor: capture `C-b`, next key routes — `%`/`"` split,
  arrows/`hjkl` nav, `x` close, `z` zoom; everything else forwards to the focused
  pane via the instanced input export.
- On grid geometry change, call the instanced resize export per pane; cores
  reflow for free.
- Status bar (pane list, focus, range chip).

## Sequencing

1. **warp**: id-keyed registry (host-tested), refactor bridge onto it, no ABI
   change. ✅ *done — branch `tmux-multicore-bridge`*
2. **warp**: instance-scoped `wasmPane*` exports (input/resize/spinner/setInput/
   getInput/queryResult/followRow/followDone/close). ✅ *done (folded into 1)*
   — still needs a version tag + weft pin bump to ship.
3. **weft `cmd/wasm`**: Boot-N-times over shared MountFS/coordinator. ✅ *done*
4. **weft frontend**: `<PaneGrid>` + prefix FSM + status bar. ✅ *done — route `/panes`*

### How to test in the browser

`./start-dev.sh` (Docker backend re-points the warp replace to `/src/warp`
internally, so the host `replace` is fine), then visit
`http://localhost:5173/panes`. Two panes open side by side over one shared FS.
`Ctrl-B` then: `%`/`"` split · `o` next · `1`-`9` jump · `x` close · `z` zoom.
Prove shared FS: `echo hi > /tmp/f` in pane A, `cat /tmp/f` in pane B.

**Ship gate:** ✅ *done.* warp `tmux-multicore-bridge` fast-forwarded onto
`main` and tagged **v0.2.35**; `weft/go.mod` bumped `v0.2.9 → v0.2.35` and the
dev `replace github.com/graybeardcorp/warp => ../warp` removed. Also fixed a
pane result-routing bug found in browser testing: query/follow results went
through the singleton `wasmHandle*` exports (→ `reg.active()`, the last-booted
pane), so output crossed panes and the initiating pane spun forever — now
threaded per-instance through `wasmPane*` (weft `frontend/src/wasm/network.ts`
+ `bridge.ts`, test `network_routing.test.ts`).

### Progress notes

- `pkg/shellcore/wasm/registry.go` + `registry_test.go` (host-run, no build tag):
  `map[instance]*ShellCore`, `active()` = most-recently-booted (legacy default),
  `remove()` for pane close.
- `boot.go`: `var active` → `var reg`; `BootOpts.Instance` (default `""`); legacy
  exports read `reg.active()` (byte-for-byte unchanged); new `wasmPane*` exports
  address by id via `reg.get(id)`. Per-core primitives (`coreInput`, `coreResize`,
  …) shared by both families.
- Verified: warp host build + `./pkg/shellcore/...` tests green; wasm-target build
  green; weft host + wasm build green against a temporary local `replace` (reverted
  — ship via tag + pin bump).

## vimgym later — now unblocked

The browser feature has landed in weft (ship gate done above), so this is the
next phase. Same frontend component reused, but vimgym's adventure is a *guided
single shell*; tiling fights it as a default. The fit there is **teaching tmux
as curriculum** (a room/course), which needs the same multi-core-shared-FS
enablement warp v0.2.35 now provides.
