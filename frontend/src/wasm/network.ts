// network.ts — SSE fetch adapter for WASM shell.
// Called from Go WASM via JS callbacks; parses SSE stream and
// calls back into WASM with results.

const API_BASE = import.meta.env.VITE_API_URL || ''

let abortController: AbortController | null = null

export function cancelQuery() {
  abortController?.abort()
  abortController = null
}

export async function execQuery(pipeline: string) {
  cancelQuery()
  abortController = new AbortController()
  const token = localStorage.getItem('weft_token')
  let timedOut = false

  const timeout = setTimeout(() => {
    timedOut = true
    abortController?.abort()
  }, 10000)

  try {
    const res = await fetch(`${API_BASE}/api/shell/exec`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        ...(token ? { Authorization: `Bearer ${token}` } : {}),
      },
      body: JSON.stringify({ pipeline, follow: false }),
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
          const evt = JSON.parse(raw) as { type: string; text?: string; message?: string; data?: { raw?: string } }
          if (evt.type === 'output' && evt.text != null) {
            outputChunks.push(evt.text)
          } else if (evt.type === 'row' && evt.data?.raw != null) {
            outputChunks.push(evt.data.raw)
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
  const token = localStorage.getItem('weft_token')

  try {
    const res = await fetch(`${API_BASE}/api/shell/exec`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        ...(token ? { Authorization: `Bearer ${token}` } : {}),
      },
      body: JSON.stringify({ pipeline, follow: true }),
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
          const evt = JSON.parse(raw) as { type: string; text?: string }
          if (evt.type === 'output' && evt.text != null) {
            window.wasmHandleFollowRow(evt.text)
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

// Type declarations for WASM global functions.
declare global {
  interface Window {
    wasmInit: (apps: string[], width: number, height: number, callbacks: WasmCallbacks) => void
    wasmHandleInput: (data: string) => void
    wasmHandleQueryResult: (text: string, errMsg: string) => void
    wasmHandleFollowRow: (raw: string) => void
    wasmHandleFollowDone: (errMsg: string) => void
    wasmResize: (cols: number, rows: number) => void
    wasmSpinnerTick: () => void
    wasmSetInput: (text: string) => void
    wasmGetInput: () => string
  }
}

export interface WasmCallbacks {
  write: (data: string) => void
  startQuery: (pipeline: string) => void
  startFollow: (pipeline: string) => void
  cancelQuery: () => void
  notifyQuit: () => void
  fsReadFile: (path: string) => { content?: string; error?: string }
  fsWriteFile: (path: string, content: string) => { error?: string }
  fsReadDir: (path: string) => { entries?: string[]; error?: string }
  fsRemove: (path: string) => { error?: string }
  fsGetWatchResults: (name: string, limit: number) => { results?: { stdout: string; ran_at: string }[]; error?: string }
  onRangeChange: (rangeText: string) => void
}
