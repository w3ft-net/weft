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
  }
}

const API_BASE = import.meta.env.VITE_API_URL || ''
const ORG_STORAGE_KEY = 'weft_org'

function authHeaders(): Record<string, string> {
  const headers: Record<string, string> = { 'Content-Type': 'application/json' }
  const token = localStorage.getItem('weft_token')
  if (token) headers['Authorization'] = `Bearer ${token}`
  const org = localStorage.getItem(ORG_STORAGE_KEY)
  if (org) headers['X-Active-Org'] = org
  return headers
}

let abortController: AbortController | null = null

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

export function cancelQuery() {
  abortController?.abort()
  abortController = null
}

export async function execQuery(pipeline: string) {
  cancelQuery()
  abortController = new AbortController()
  let timedOut = false

  const timeout = setTimeout(() => {
    timedOut = true
    abortController?.abort()
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
      window.wasmHandleQueryResult('', msg || `HTTP ${res.status}`)
      return
    }

    const contentType = res.headers.get('content-type') || ''
    if (!contentType.includes('text/event-stream')) {
      window.wasmHandleQueryResult('', `unexpected response (content-type: ${contentType || 'none'})`)
      return
    }

    const reader = res.body?.getReader()
    if (!reader) {
      window.wasmHandleQueryResult('', 'No response body')
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
            window.wasmHandleQueryResult('', evt.text || evt.message || 'unknown error')
            return
          }
        } catch {
          // skip malformed SSE
        }
      }
    }

    window.wasmHandleQueryResult(outputChunks.join('\n') + (outputChunks.length ? '\n' : ''), '')
  } catch (err) {
    clearTimeout(timeout)
    if ((err as Error).name === 'AbortError') {
      if (timedOut) {
        window.wasmHandleQueryResult('', 'request timed out')
      }
      // User-initiated cancel (Ctrl+C): no message needed.
    } else {
      const msg = (err as Error).message
      if (msg === 'Failed to fetch' || msg.includes('NetworkError')) {
        window.wasmHandleQueryResult('', 'cannot reach API server')
      } else {
        window.wasmHandleQueryResult('', msg)
      }
    }
  }
}

export async function execFollow(pipeline: string) {
  cancelQuery()
  abortController = new AbortController()

  try {
    const res = await fetch(`${API_BASE}/api/shell/exec`, {
      method: 'POST',
      headers: authHeaders(),
      body: JSON.stringify({ pipeline, follow: true, range: currentRangeSpec }),
      signal: abortController.signal,
    })

    if (!res.ok) {
      const msg = await res.text().catch(() => `HTTP ${res.status}`)
      window.wasmHandleFollowDone(msg)
      return
    }

    const reader = res.body?.getReader()
    if (!reader) {
      window.wasmHandleFollowDone('No response body')
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
            window.wasmHandleFollowRow(evt.data.raw)
          } else if (evt.type === 'error' && evt.text != null) {
            window.wasmHandleFollowDone(evt.text)
            return
          }
        } catch {
          // skip malformed SSE
        }
      }
    }

    window.wasmHandleFollowDone('')
  } catch (err) {
    if ((err as Error).name !== 'AbortError') {
      const msg = (err as Error).message
      if (msg === 'Failed to fetch' || msg.includes('NetworkError')) {
        window.wasmHandleFollowDone('cannot reach API server')
      } else {
        window.wasmHandleFollowDone(msg)
      }
    } else {
      window.wasmHandleFollowDone('')
    }
  }
}

// wasm* JS-side globals are declared in @warp/shellcore-wasm. This
// file uses wasmHandleQueryResult / wasmHandleFollowRow / wasmHandleFollowDone
// from that surface — no per-package re-declaration needed.
