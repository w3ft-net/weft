// network_routing.test.ts — what the page itself sends to the API server for
// the shell: tour reports and the trace modal's row fetch. The terminal's own
// queries and follows run in the wasm shell (shell.ExecClient), tested in Go
// and through the real binary in shell_bridge.test.ts.

import { describe, it, expect, beforeEach, vi } from 'vitest'
import { reportCommandCompleted, reportModalClosed, fetchPipelineLines } from '../wasm/network'

// Minimal window stub. In the plain-node vitest pool there is no DOM.
function installWindow(): Record<string, unknown> {
  const w: Record<string, unknown> = {}
  ;(globalThis as unknown as { window: unknown }).window = w
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

describe('reportCommandCompleted', () => {
  const noContent = () => ({ ok: true, status: 204, json: async () => ({}) } as unknown as Response)
  let events: Array<{ type: string; detail: unknown }>

  beforeEach(() => {
    const w = installWindow()
    events = []
    w.dispatchEvent = (e: Event) => { events.push({ type: e.type, detail: (e as CustomEvent).detail }); return true }
  })

  it('posts the full line and its exit code', async () => {
    const fetchMock = vi.fn(async () => noContent())
    vi.stubGlobal('fetch', fetchMock)

    await reportCommandCompleted('history', 0)

    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit]
    expect(url).toContain('/api/shell/builtin')
    expect(JSON.parse(init.body as string)).toEqual({ pipeline: 'history', exit_code: 0 })
  })

  it('reports a dismissed modal as its own event, so the tour can wait for it', async () => {
    const fetchMock = vi.fn(async () => noContent())
    vi.stubGlobal('fetch', fetchMock)

    await reportModalClosed('trace abc')

    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit]
    expect(url).toContain('/api/shell/builtin')
    expect(JSON.parse(init.body as string)).toEqual({ pipeline: 'trace abc', exit_code: 0, event: 'modal_closed' })
    expect(events).toEqual([{ type: 'tour-advance', detail: { step: undefined } }])
  })

  it('tells the tour the new step when this report advanced it', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => ({ ok: true, status: 200, json: async () => ({ step: 'shell-tmp' }) } as unknown as Response)))

    await reportCommandCompleted('history', 0)

    expect(events).toEqual([{ type: 'tour-advance', detail: { step: 'shell-tmp' } }])
  })

  it('still asks the tour to refresh when the report did not advance anything', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => noContent()))

    await reportCommandCompleted('tail -f /log/x', 0)

    expect(events).toEqual([{ type: 'tour-advance', detail: { step: undefined } }])
  })

  it('does not throw when the API is unreachable', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => { throw new Error('Failed to fetch') }))

    await expect(reportCommandCompleted('ls', 0)).resolves.toBeUndefined()
    expect(events).toEqual([])
  })
})

describe('fetchPipelineLines', () => {
  it('posts its pipeline to the exec endpoint as a one-shot query', async () => {
    installWindow()
    const fetchMock = vi.fn(async () => ({ ok: false, status: 500, text: async () => 'stop here' } as unknown as Response))
    vi.stubGlobal('fetch', fetchMock)

    await fetchPipelineLines('trace abc').catch(() => {})

    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit]
    expect(url).toContain('/api/shell/exec')
    expect(JSON.parse(init.body as string)).toMatchObject({ pipeline: 'trace abc', follow: false })
  })
})
