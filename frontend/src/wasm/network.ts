// network.ts — SSE fetch adapter for WASM shell.
// Called from Go WASM via JS callbacks; parses SSE stream and
// calls back into WASM with results.

// Local declaration of the wasm* exports we call here. The full
// surface is declared in @warp/shellcore-wasm; under
// verbatimModuleSyntax + bundler-mode we can't rely on a bare
// side-effect import to merge those globals into this file's
// scope, so we re-declare the subset network.ts uses directly.
declare global {
  interface Window {
    wasmHandleQueryResult: (text: string, errMsg: string) => void
    wasmHandleFollowRow: (raw: string) => void
    wasmHandleFollowDone: (errMsg: string) => void

    // Instanced (browser-tmux) result delivery. Present only in
    // multi-pane WASM builds (weft's cmd/wasm registers them via
    // warp's bridge). They address the pane that started the query by
    // id so its result lands in that pane's core — the singleton
    // wasm* exports above route to reg.active() (the most-recently
    // booted pane), which crosses output between panes.
    wasmPaneQueryResult?: (instance: string, text: string, errMsg: string) => void
    wasmPaneFollowRow?: (instance: string, raw: string) => void
    wasmPaneFollowDone?: (instance: string, errMsg: string) => void
  }
  // __WEFT_WEB_SHA__ is injected by Vite at build time (see vite.config.ts).
  // The string is the short git SHA the bundle was built from, or "dev"
  // when no git repo was present. Used to identify the frontend on every
  // server-bound fetch as X-Weft-Client: weft-web/<sha>.
  const __WEFT_WEB_SHA__: string
}

const API_BASE = import.meta.env.VITE_API_URL || ''
const ORG_STORAGE_KEY = 'weft_org'

// Exported for cors_contract.test.ts, which pins the set of header
// names this returns against the server's allow-list source of truth
// (Go: internal/clientwire/headers.go). A header added here that the
// api server's CORSMiddleware does not allow makes the browser shell
// die at preflight while the CLI keeps working — that exact gap shipped
// twice (X-Active-Org, X-Weft-Client). Keep the two lists in sync.
export function authHeaders(): Record<string, string> {
  const headers: Record<string, string> = { 'Content-Type': 'application/json' }
  const token = localStorage.getItem('weft_token')
  if (token) headers['Authorization'] = `Bearer ${token}`
  const org = localStorage.getItem(ORG_STORAGE_KEY)
  if (org) headers['X-Active-Org'] = org
  // The API server's ClientVersionMiddleware only enforces a min on
  // `weft/<ver>` (CLI). `weft-web/<sha>` ships in lockstep so it's
  // exempt from the gate — but the header still flows so admin
  // dashboards can see which browser build hit which endpoint.
  headers['X-Weft-Client'] = `weft-web/${__WEFT_WEB_SHA__}`
  return headers
}

// A query/follow is keyed by the pane instance that started it (or ""
// for the single-pane shell). Each pane runs an independent request, so
// its in-flight AbortController is tracked per key — otherwise starting
// a query in one pane would cancel a query still streaming in another.
const controllers = new Map<string, AbortController>()

function keyFor(instance?: string): string {
  return instance ?? ''
}

// deliver returns the result callbacks for a given pane. With an
// instance, results route through the instanced wasmPane* exports so
// they reach the pane that started the query; without one, the legacy
// singleton exports (single-pane shell) are used unchanged.
interface Deliver {
  queryResult: (text: string, errMsg: string) => void
  followRow: (raw: string) => void
  followDone: (errMsg: string) => void
}

function deliver(instance?: string): Deliver {
  if (instance) {
    return {
      queryResult: (t, e) => window.wasmPaneQueryResult?.(instance, t, e),
      followRow: (r) => window.wasmPaneFollowRow?.(instance, r),
      followDone: (e) => window.wasmPaneFollowDone?.(instance, e),
    }
  }
  return {
    queryResult: (t, e) => window.wasmHandleQueryResult(t, e),
    followRow: (r) => window.wasmHandleFollowRow(r),
    followDone: (e) => window.wasmHandleFollowDone(e),
  }
}

// currentRangeSpec holds the un-resolved range spec ("1d..", "30m..10m",
// "2026-04-01..2026-04-12"). The Go-side `range` builtin pushes updates via
// the onRangeChange JS callback (see bridge.ts → setCurrentRangeSpec). The
// server resolves it on every request, so even a tab left open for hours
// always queries against a fresh window.
let currentRangeSpec = '1d..'

export function setCurrentRangeSpec(spec: string) {
  if (spec) currentRangeSpec = spec
}

export function getCurrentRangeSpec(): string {
  return currentRangeSpec
}

export function notifyBuiltinExecuted(pipeline: string) {
  fetch(`${API_BASE}/api/shell/builtin`, {
    method: 'POST',
    headers: authHeaders(),
    body: JSON.stringify({ pipeline }),
  }).then(async res => {
    if (!res.ok || res.status !== 200) return
    let step: string | undefined
    try {
      const body = await res.json() as { step?: string }
      step = body.step
    } catch { /* tolerate no-body */ }
    window.dispatchEvent(new CustomEvent('tour-advance', { detail: { step } }))
  }).catch(() => {})
}

export function cancelQuery(instance?: string) {
  const k = keyFor(instance)
  controllers.get(k)?.abort()
  controllers.delete(k)
}

export async function execQuery(pipeline: string, instance?: string) {
  cancelQuery(instance)
  const abortController = new AbortController()
  controllers.set(keyFor(instance), abortController)
  const out = deliver(instance)
  let timedOut = false

  const timeout = setTimeout(() => {
    timedOut = true
    abortController.abort()
  }, 10000)

  try {
    const res = await fetch(`${API_BASE}/api/shell/exec`, {
      method: 'POST',
      headers: authHeaders(),
      body: JSON.stringify({ pipeline, follow: false, range: currentRangeSpec }),
      signal: abortController.signal,
    })
    clearTimeout(timeout)

    if (!res.ok) {
      const msg = await res.text().catch(() => `HTTP ${res.status}`)
      out.queryResult('', msg || `HTTP ${res.status}`)
      return
    }

    const contentType = res.headers.get('content-type') || ''
    if (!contentType.includes('text/event-stream')) {
      out.queryResult('', `unexpected response (content-type: ${contentType || 'none'})`)
      return
    }

    const reader = res.body?.getReader()
    if (!reader) {
      out.queryResult('', 'No response body')
      return
    }

    const decoder = new TextDecoder()
    let buf = ''
    const outputChunks: string[] = []

    while (true) {
      const { done, value } = await reader.read()
      if (done) break

      buf += decoder.decode(value, { stream: true })
      const parts = buf.split('\n')
      buf = parts.pop() ?? ''

      for (const part of parts) {
        const line = part.trim()
        if (!line.startsWith('data:')) continue
        const raw = line.slice(5).trim()
        if (!raw || raw === '[DONE]') continue
        try {
          const evt = JSON.parse(raw) as { type: string; step?: string; text?: string; message?: string; data?: { raw?: string } }
          if (evt.type === 'line' && evt.data?.raw != null) {
            outputChunks.push(evt.data.raw)
          } else if (evt.type === 'stderr' && evt.text != null) {
            // Non-fatal diagnostics (alert messages, "non-numeric
            // value", etc.) emitted alongside stdout. Render red so
            // they're distinguishable in the same scrollback; the
            // stream keeps going. `error` events are terminal, this
            // one isn't.
            outputChunks.push(`\x1b[91m${evt.text.replace(/\n$/, '')}\x1b[0m`)
          } else if (evt.type === 'tour_advance') {
            window.dispatchEvent(new CustomEvent('tour-advance', { detail: { step: evt.step } }))
          } else if (evt.type === 'error') {
            out.queryResult('', evt.text || evt.message || 'unknown error')
            return
          }
        } catch {
          // skip malformed SSE
        }
      }
    }

    out.queryResult(outputChunks.join('\n') + (outputChunks.length ? '\n' : ''), '')
  } catch (err) {
    clearTimeout(timeout)
    if ((err as Error).name === 'AbortError') {
      if (timedOut) {
        out.queryResult('', 'request timed out')
      }
      // User-initiated cancel (Ctrl+C): no message needed.
    } else {
      const msg = (err as Error).message
      if (msg === 'Failed to fetch' || msg.includes('NetworkError')) {
        out.queryResult('', 'cannot reach API server')
      } else {
        out.queryResult('', msg)
      }
    }
  } finally {
    // Drop this request's controller unless a newer one has already
    // replaced it (a rapid re-run for the same pane).
    if (controllers.get(keyFor(instance)) === abortController) {
      controllers.delete(keyFor(instance))
    }
  }
}

export async function execFollow(pipeline: string, instance?: string) {
  cancelQuery(instance)
  const abortController = new AbortController()
  controllers.set(keyFor(instance), abortController)
  const out = deliver(instance)

  try {
    const res = await fetch(`${API_BASE}/api/shell/exec`, {
      method: 'POST',
      headers: authHeaders(),
      body: JSON.stringify({ pipeline, follow: true, range: currentRangeSpec }),
      signal: abortController.signal,
    })

    if (!res.ok) {
      const msg = await res.text().catch(() => `HTTP ${res.status}`)
      out.followDone(msg)
      return
    }

    const reader = res.body?.getReader()
    if (!reader) {
      out.followDone('No response body')
      return
    }

    const decoder = new TextDecoder()
    let buf = ''

    while (true) {
      const { done, value } = await reader.read()
      if (done) break

      buf += decoder.decode(value, { stream: true })
      const parts = buf.split('\n')
      buf = parts.pop() ?? ''

      for (const part of parts) {
        const line = part.trim()
        if (!line.startsWith('data:')) continue
        const raw = line.slice(5).trim()
        if (!raw || raw === '[DONE]') continue
        try {
          const evt = JSON.parse(raw) as { type: string; text?: string; data?: { raw?: string } }
          if (evt.type === 'line' && evt.data?.raw != null) {
            out.followRow(evt.data.raw)
          } else if (evt.type === 'error' && evt.text != null) {
            out.followDone(evt.text)
            return
          }
        } catch {
          // skip malformed SSE
        }
      }
    }

    out.followDone('')
  } catch (err) {
    if ((err as Error).name !== 'AbortError') {
      const msg = (err as Error).message
      if (msg === 'Failed to fetch' || msg.includes('NetworkError')) {
        out.followDone('cannot reach API server')
      } else {
        out.followDone(msg)
      }
    } else {
      out.followDone('')
    }
  } finally {
    if (controllers.get(keyFor(instance)) === abortController) {
      controllers.delete(keyFor(instance))
    }
  }
}

// wasm* JS-side globals are declared in @warp/shellcore-wasm. This
// file uses wasmHandleQueryResult / wasmHandleFollowRow / wasmHandleFollowDone
// from that surface — no per-package re-declaration needed.
