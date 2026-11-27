import { extraRequestHeaders } from '../lib/headers'

// network.ts — what the page itself sends to the API server for the shell: the
// auth headers the Go shell asks for on every query (callbacks.authHeaders),
// the tour reports, and the one-shot row fetch the trace modal uses. The
// terminal's own queries and follows run in the wasm shell (shell.ExecClient).

declare global {
  // __WEFT_WEB_SHA__ is injected by Vite at build time (see vite.config.ts).
  // The string is the short git SHA the bundle was built from, or "dev"
  // when no git repo was present. Used to identify the frontend on every
  // server-bound fetch as X-Weft-Client: weft-web/<sha>.
  const __WEFT_WEB_SHA__: string
}

export const API_BASE = import.meta.env.VITE_API_URL || ''

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
  Object.assign(headers, extraRequestHeaders())
  // The API server's ClientVersionMiddleware only enforces a min on
  // `weft/<ver>` (CLI). `weft-web/<sha>` ships in lockstep so it's
  // exempt from the gate — but the header still flows so admin
  // dashboards can see which browser build hit which endpoint.
  headers['X-Weft-Client'] = `weft-web/${__WEFT_WEB_SHA__}`
  return headers
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

// reportCommandCompleted tells the server a shell line finished, with the
// exit code the shell core saw. The tour advances from this — the browser
// knows exactly what the user ran and whether it worked, whereas the
// server's exec path only sees the pipeline text the client chose to send
// (pipe stages like `| chart ...` are handled locally and inline builtins
// like `history` never leave the browser). A non-zero exit never advances
// a lesson (enforced server-side).
//
// 'tour-advance' is dispatched whether or not this report advanced the
// step: the server's exec-path hook may have advanced it already, and the
// card still needs to refresh. Best effort — a failed report must never
// disturb the shell.
export function reportCommandCompleted(pipeline: string, exitCode: number): Promise<void> {
  return postBuiltinReport({ pipeline, exit_code: exitCode })
}

// reportModalClosed tells the server the user dismissed the modal the given
// line opened (`trace <id>`). A lesson whose command opens a modal completes
// on this rather than on the command finishing, so the next lesson's prefill
// never lands underneath an open modal.
export function reportModalClosed(pipeline: string): Promise<void> {
  return postBuiltinReport({ pipeline, exit_code: 0, event: 'modal_closed' })
}

// announceTourAdvance tells the page's tour card the tour moved to step, so it
// can refresh.
export function announceTourAdvance(step?: string) {
  window.dispatchEvent(new CustomEvent('tour-advance', { detail: { step } }))
}

async function postBuiltinReport(body: { pipeline: string; exit_code: number; event?: string }): Promise<void> {
  try {
    const res = await fetch(`${API_BASE}/api/shell/builtin`, {
      method: 'POST',
      headers: authHeaders(),
      body: JSON.stringify(body),
    })
    if (!res.ok) return
    let step: string | undefined
    if (res.status === 200) {
      try {
        step = ((await res.json()) as { step?: string }).step
      } catch { /* tolerate no-body */ }
    }
    announceTourAdvance(step)
  } catch { /* best effort */ }
}

// fetchPipelineLines runs pipeline as a one-shot query and resolves to its raw
// output lines. It is for a React component (ShellTraceModal) that needs the
// rows directly to hand to the wasmParseTraceRows/wasmRenderSpanRollup exports
// itself, so it does not go through the shell. The caller passes its own
// AbortSignal (e.g. from a cleanup effect).
export async function fetchPipelineLines(pipeline: string, signal?: AbortSignal): Promise<string[]> {
  const res = await fetch(`${API_BASE}/api/shell/exec`, {
    method: 'POST',
    headers: authHeaders(),
    body: JSON.stringify({ pipeline, follow: false, range: currentRangeSpec }),
    signal,
  })

  if (!res.ok) {
    const msg = await res.text().catch(() => `HTTP ${res.status}`)
    throw new Error(msg || `HTTP ${res.status}`)
  }

  const contentType = res.headers.get('content-type') || ''
  if (!contentType.includes('text/event-stream')) {
    throw new Error(`unexpected response (content-type: ${contentType || 'none'})`)
  }

  const reader = res.body?.getReader()
  if (!reader) {
    throw new Error('No response body')
  }

  const decoder = new TextDecoder()
  let buf = ''
  const lines: string[] = []

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
      let evt: { type: string; text?: string; message?: string; data?: { raw?: string } }
      try {
        evt = JSON.parse(raw)
      } catch {
        continue // skip malformed SSE
      }
      if (evt.type === 'line' && evt.data?.raw != null) {
        lines.push(evt.data.raw)
      } else if (evt.type === 'error') {
        throw new Error(evt.text || evt.message || 'unknown error')
      }
      // 'stderr' events are ignored here — this path
      // is for structured row data (trace/spans queries), not scrollback.
    }
  }

  return lines
}
