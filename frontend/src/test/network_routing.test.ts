// network_routing.test.ts — query/follow results must return to the
// pane that started them.
//
// Browser tmux boots N shellcore panes on one Go runtime. Each pane's
// core owns its own spinner and scrollback. A query fired from pane A
// must have its result delivered to pane A's core (via the instanced
// wasmPane* exports), NOT to the legacy singleton exports (which the Go
// bridge routes to reg.active() — the most-recently-booted pane). The
// original bug: a command run in pane 1 printed its output in pane 2 and
// left pane 1 spinning "querying shards…" forever.

import { describe, it, expect, beforeEach, vi } from 'vitest'
import { execQuery, execFollow } from '../wasm/network'

// Minimal window stub carrying just the wasm* delivery exports the
// network layer calls. In the plain-node vitest pool there is no DOM.
interface WasmWindow {
  wasmHandleQueryResult: ReturnType<typeof vi.fn>
  wasmHandleFollowRow: ReturnType<typeof vi.fn>
  wasmHandleFollowDone: ReturnType<typeof vi.fn>
  wasmPaneQueryResult: ReturnType<typeof vi.fn>
  wasmPaneFollowRow: ReturnType<typeof vi.fn>
  wasmPaneFollowDone: ReturnType<typeof vi.fn>
}

function installWindow(): WasmWindow {
  const w: WasmWindow = {
    wasmHandleQueryResult: vi.fn(),
    wasmHandleFollowRow: vi.fn(),
    wasmHandleFollowDone: vi.fn(),
    wasmPaneQueryResult: vi.fn(),
    wasmPaneFollowRow: vi.fn(),
    wasmPaneFollowDone: vi.fn(),
  }
  ;(globalThis as unknown as { window: WasmWindow }).window = w
  // authHeaders() interpolates the Vite-stamped build SHA into the
  // X-Weft-Client header; supply it (as vite.config.ts does at build).
  ;(globalThis as Record<string, unknown>).__WEFT_WEB_SHA__ = 'testsha'
  // authHeaders() reads the auth token / active org from localStorage;
  // there is no DOM in the node pool, so provide an empty stub.
  ;(globalThis as unknown as { localStorage: Storage }).localStorage = {
    getItem: () => null,
    setItem: () => {},
    removeItem: () => {},
    clear: () => {},
    key: () => null,
    length: 0,
  } as Storage
  return w
}

// sseResponse builds a minimal fetch Response whose body streams the
// given SSE `data:` lines, one Uint8Array chunk per line.
function sseResponse(lines: string[]): Response {
  const enc = new TextEncoder()
  let i = 0
  const reader = {
    read: async () => {
      if (i < lines.length) {
        const chunk = enc.encode(lines[i] + '\n')
        i += 1
        return { done: false, value: chunk }
      }
      return { done: true, value: undefined }
    },
  }
  return {
    ok: true,
    status: 200,
    headers: { get: (k: string) => (k.toLowerCase() === 'content-type' ? 'text/event-stream' : null) },
    body: { getReader: () => reader },
  } as unknown as Response
}

describe('query/follow result routing', () => {
  beforeEach(() => {
    installWindow()
  })

  it('delivers a query result to the initiating pane, not the singleton', async () => {
    const w = (globalThis as unknown as { window: WasmWindow }).window
    vi.stubGlobal('fetch', vi.fn(async () =>
      sseResponse(['data: {"type":"line","data":{"raw":"hello"}}']),
    ))

    await execQuery('range 1d | count', 'pane-1#3')

    expect(w.wasmPaneQueryResult).toHaveBeenCalledWith('pane-1#3', 'hello\n', '')
    expect(w.wasmHandleQueryResult).not.toHaveBeenCalled()
  })

  it('routes a query error to the initiating pane', async () => {
    const w = (globalThis as unknown as { window: WasmWindow }).window
    vi.stubGlobal('fetch', vi.fn(async () =>
      sseResponse(['data: {"type":"error","text":"boom"}']),
    ))

    await execQuery('bogus', 'pane-2#7')

    expect(w.wasmPaneQueryResult).toHaveBeenCalledWith('pane-2#7', '', 'boom')
    expect(w.wasmHandleQueryResult).not.toHaveBeenCalled()
  })

  it('delivers follow rows and completion to the initiating pane', async () => {
    const w = (globalThis as unknown as { window: WasmWindow }).window
    vi.stubGlobal('fetch', vi.fn(async () =>
      sseResponse(['data: {"type":"line","data":{"raw":"tail-row"}}']),
    ))

    await execFollow('range 1d | follow', 'pane-3#2')

    expect(w.wasmPaneFollowRow).toHaveBeenCalledWith('pane-3#2', 'tail-row')
    expect(w.wasmPaneFollowDone).toHaveBeenCalledWith('pane-3#2', '')
    expect(w.wasmHandleFollowRow).not.toHaveBeenCalled()
    expect(w.wasmHandleFollowDone).not.toHaveBeenCalled()
  })

  it('single-pane (no instance) still uses the legacy singleton exports', async () => {
    const w = (globalThis as unknown as { window: WasmWindow }).window
    vi.stubGlobal('fetch', vi.fn(async () =>
      sseResponse(['data: {"type":"line","data":{"raw":"solo"}}']),
    ))

    await execQuery('range 1d | count')

    expect(w.wasmHandleQueryResult).toHaveBeenCalledWith('solo\n', '')
    expect(w.wasmPaneQueryResult).not.toHaveBeenCalled()
  })
})
