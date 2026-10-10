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
  wasmGetDirStack: ReturnType<typeof vi.fn>
  wasmSetDirStack: ReturnType<typeof vi.fn>
  wasmPaneGetDirStack: ReturnType<typeof vi.fn>
  wasmPaneSetDirStack: ReturnType<typeof vi.fn>
}

function installWindow(): WasmWindow {
  const w: WasmWindow = {
    wasmHandleQueryResult: vi.fn(),
    wasmHandleFollowRow: vi.fn(),
    wasmHandleFollowDone: vi.fn(),
    wasmPaneQueryResult: vi.fn(),
    wasmPaneFollowRow: vi.fn(),
    wasmPaneFollowDone: vi.fn(),
    wasmGetDirStack: vi.fn(() => ['/']),
    wasmSetDirStack: vi.fn(),
    wasmPaneGetDirStack: vi.fn(() => ['/']),
    wasmPaneSetDirStack: vi.fn(),
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

  // The server (api/shell.go) emits terminal errors as
  // {"type":"error","message":...} — not "text". A follow stream that
  // dies on e.g. an unreachable coordinator must surface that message,
  // not end as if the tail finished cleanly.
  it('surfaces a server error event (message field) on a query', async () => {
    const w = (globalThis as unknown as { window: WasmWindow }).window
    vi.stubGlobal('fetch', vi.fn(async () =>
      sseResponse(['data: {"type":"error","message":"coordinator unavailable"}']),
    ))

    await execQuery('range 1d | count', 'pane-4#1')

    expect(w.wasmPaneQueryResult).toHaveBeenCalledWith('pane-4#1', '', 'coordinator unavailable')
  })

  it('surfaces a server error event (message field) on a follow', async () => {
    const w = (globalThis as unknown as { window: WasmWindow }).window
    vi.stubGlobal('fetch', vi.fn(async () =>
      sseResponse(['data: {"type":"error","message":"coordinator unavailable"}']),
    ))

    await execFollow('range 1d | follow', 'pane-5#1')

    expect(w.wasmPaneFollowDone).toHaveBeenCalledWith('pane-5#1', 'coordinator unavailable')
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

  // Dirstack courier: the server owns cd, so every request ships the
  // core's cached stack (index 0 == cwd) and the post-run stack returns
  // on a "cwd" SSE event. The CLI runner does both; the browser must too,
  // or `cd` never sticks (the prompt stays at ~ and relative paths break).
  describe('dirstack courier', () => {
    it('sends the pane core\'s dirstack with a query', async () => {
      const w = (globalThis as unknown as { window: WasmWindow }).window
      w.wasmPaneGetDirStack.mockReturnValue(['/log/loom/prod/api', '/tmp'])
      const fetchMock = vi.fn(async () => sseResponse(['data: {"type":"done"}']))
      vi.stubGlobal('fetch', fetchMock)

      await execQuery('pwd', 'pane-6#1')

      const body = JSON.parse((fetchMock.mock.calls[0] as unknown as [string, RequestInit])[1].body as string)
      expect(w.wasmPaneGetDirStack).toHaveBeenCalledWith('pane-6#1')
      expect(body.dirstack).toEqual(['/log/loom/prod/api', '/tmp'])
    })

    it('sends the singleton core\'s dirstack when there is no instance', async () => {
      const w = (globalThis as unknown as { window: WasmWindow }).window
      w.wasmGetDirStack.mockReturnValue(['/log'])
      const fetchMock = vi.fn(async () => sseResponse(['data: {"type":"done"}']))
      vi.stubGlobal('fetch', fetchMock)

      await execQuery('pwd')

      const body = JSON.parse((fetchMock.mock.calls[0] as unknown as [string, RequestInit])[1].body as string)
      expect(body.dirstack).toEqual(['/log'])
    })

    it('sends the dirstack with a follow too', async () => {
      const w = (globalThis as unknown as { window: WasmWindow }).window
      w.wasmPaneGetDirStack.mockReturnValue(['/log/loom/prod/api'])
      const fetchMock = vi.fn(async () => sseResponse(['data: {"type":"line","data":{"raw":"x"}}']))
      vi.stubGlobal('fetch', fetchMock)

      await execFollow('tail -f main.log', 'pane-7#1')

      const body = JSON.parse((fetchMock.mock.calls[0] as unknown as [string, RequestInit])[1].body as string)
      expect(body.dirstack).toEqual(['/log/loom/prod/api'])
    })

    it('applies the returned cwd to the pane BEFORE delivering the result', async () => {
      const w = (globalThis as unknown as { window: WasmWindow }).window
      vi.stubGlobal('fetch', vi.fn(async () =>
        sseResponse([
          'data: {"type":"cwd","cwd":"/tmp","stack":["/tmp","/"]}',
          'data: {"type":"done"}',
        ]),
      ))

      await execQuery('cd /tmp', 'pane-8#1')

      expect(w.wasmPaneSetDirStack).toHaveBeenCalledWith('pane-8#1', ['/tmp', '/'])
      // The redrawn prompt must show the new cwd, so the stack has to be
      // in place before the result event redraws it.
      const setOrder = w.wasmPaneSetDirStack.mock.invocationCallOrder[0]
      const resultOrder = w.wasmPaneQueryResult.mock.invocationCallOrder[0]
      expect(setOrder).toBeLessThan(resultOrder)
    })

    it('applies the returned cwd to the singleton core when there is no instance', async () => {
      const w = (globalThis as unknown as { window: WasmWindow }).window
      vi.stubGlobal('fetch', vi.fn(async () =>
        sseResponse(['data: {"type":"cwd","cwd":"/tmp","stack":["/tmp"]}', 'data: {"type":"done"}']),
      ))

      await execQuery('cd /tmp')

      expect(w.wasmSetDirStack).toHaveBeenCalledWith(['/tmp'])
    })
  })
})
