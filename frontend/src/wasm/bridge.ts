// bridge.ts — Loads the Go WASM binary and wires it to xterm.js.

import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { execQuery, execFollow, cancelQuery, type WasmCallbacks } from './network'

// Go WASM runtime support — loaded via <script> in index.html.
declare class Go {
  importObject: WebAssembly.Imports
  run(instance: WebAssembly.Instance): Promise<void>
}

export interface ShellBridge {
  terminal: Terminal
  dispose: () => void
  setInput: (text: string) => void
  getInput: () => string
}

export interface ShellIdentity {
  user: string     // email-local-part for the prompt — alice@acme.test → "alice"
  customer: string // customer/team slug — "acme"
}

export async function initShellWasm(
  container: HTMLElement,
  apps: string[],
  onRangeChange?: (rangeText: string) => void,
  identity?: ShellIdentity,
): Promise<ShellBridge> {
  // Create xterm.js terminal.
  const terminal = new Terminal({
    fontFamily: '"JetBrains Mono", "SF Mono", "Fira Code", "Cascadia Code", monospace',
    fontSize: 13,
    lineHeight: 1.2,
    theme: {
      background: '#0f1117',
      foreground: '#c8ccd4',
      cursor: '#ff8700',
      cursorAccent: '#0f1117',
      selectionBackground: 'rgba(100, 120, 160, 0.4)',
      black: '#1a1e2a',
      red: '#e06c75',
      green: '#98c379',
      yellow: '#e5c07b',
      blue: '#61afef',
      magenta: '#c678dd',
      cyan: '#56b6c2',
      white: '#c8ccd4',
      brightBlack: '#5c6370',
      brightRed: '#e06c75',
      brightGreen: '#98c379',
      brightYellow: '#e5c07b',
      brightBlue: '#61afef',
      brightMagenta: '#c678dd',
      brightCyan: '#56b6c2',
      brightWhite: '#ffffff',
    },
    allowProposedApi: true,
    scrollback: 10000,
    cursorBlink: true,
    cursorStyle: 'block',
  })

  const fitAddon = new FitAddon()
  terminal.loadAddon(fitAddon)
  terminal.open(container)
  fitAddon.fit()

  // Load Go WASM.
  const go = new Go()
  const result = await WebAssembly.instantiateStreaming(
    fetch('/shell.wasm'),
    go.importObject,
  )
  // Don't await go.run — it blocks until the Go program exits (which is never).
  go.run(result.instance)

  // Callbacks from WASM to JS.
  const callbacks: WasmCallbacks = {
    write: (data: string) => {
      terminal.write(data)
    },
    startQuery: (pipeline: string) => {
      execQuery(pipeline)
    },
    startFollow: (pipeline: string) => {
      execFollow(pipeline)
    },
    cancelQuery: () => {
      cancelQuery()
    },
    notifyQuit: () => {},

    // Virtual filesystem operations — proxy to the API via synchronous XHR.
    // Must be synchronous because Go WASM calls these from js.Value.Call().
    fsReadFile: (path: string) => fsApiSync('GET', path),
    fsWriteFile: (path: string, content: string) => fsApiSync('PUT', path, content),
    fsReadDir: (path: string) => fsApiSyncDir(path),
    fsRemove: (path: string) => fsApiSync('DELETE', path),
    fsGetWatchResults: (name: string, limit: number) => fsGetWatchResults(name, limit),
    onRangeChange: (rangeText: string) => {
      if (onRangeChange) onRangeChange(rangeText)
    },

    // History persistence via localStorage.
    loadHistory: () => localStorage.getItem('weft_history') || '',
    saveHistory: (lines: string) => localStorage.setItem('weft_history', lines),

    // Identity for the prompt prefix. Caller supplies this from
    // AuthContext; absent identity falls back to "" which the prompt
    // renders as `<empty>` (no user@host prefix).
    getUser: () => identity?.user ?? '',
    getCustomer: () => identity?.customer ?? '',
  }

  // Initialize WASM shell.
  window.wasmInit(apps, terminal.cols, terminal.rows, callbacks)

  // Auto-focus terminal.
  terminal.focus()

  // Wire xterm.js input to WASM.
  terminal.onData((data: string) => {
    window.wasmHandleInput(data)
  })

  // Handle resize.
  const resizeObserver = new ResizeObserver(() => {
    fitAddon.fit()
    window.wasmResize(terminal.cols, terminal.rows)
  })
  resizeObserver.observe(container)

  // Spinner tick interval.
  const spinnerInterval = setInterval(() => {
    window.wasmSpinnerTick()
  }, 80)

  return {
    terminal,
    dispose: () => {
      clearInterval(spinnerInterval)
      resizeObserver.disconnect()
      terminal.dispose()
    },
    setInput: (text: string) => {
      window.wasmSetInput(text)
    },
    getInput: () => {
      return window.wasmGetInput()
    },
  }
}

// ── Synchronous FS API helpers ──────────────────────────────
// These use XMLHttpRequest (synchronous mode) because Go WASM's
// js.Value.Call() blocks until the JS function returns.

const API_BASE = import.meta.env.VITE_API_URL || ''

function fsApiPath(fsPath: string): { endpoint: string; name: string } | null {
  // Map virtual FS paths to API endpoints.
  // /bin/check_errors → PUT /api/functions/check_errors
  // /cron.d/api-errors → PUT /api/watches/api-errors
  // /etc/notify.conf → PUT /api/notify
  if (fsPath === '/etc/notify.conf') {
    return { endpoint: '/api/notify', name: 'notify.conf' }
  }
  const m = fsPath.match(/^\/(bin|cron\.d)\/(.+)$/)
  if (m) {
    const dir = m[1]
    const name = m[2]
    if (dir === 'bin') return { endpoint: `/api/functions/${encodeURIComponent(name)}`, name }
    if (dir === 'cron.d') return { endpoint: `/api/watches/${encodeURIComponent(name)}`, name }
  }
  return null
}

function fsApiSync(method: string, path: string, content?: string): { content?: string; error?: string } {
  const mapped = fsApiPath(path)
  if (!mapped) {
    return { error: `${path}: unsupported path` }
  }

  const token = localStorage.getItem('weft_token')
  const xhr = new XMLHttpRequest()
  xhr.open(method, `${API_BASE}${mapped.endpoint}`, false) // synchronous
  xhr.setRequestHeader('Content-Type', 'application/json')
  if (token) xhr.setRequestHeader('Authorization', `Bearer ${token}`)

  try {
    if (method === 'PUT' && content !== undefined) {
      // For functions: send { source: content }
      // For watches: send raw content (the cron file) — server parses it
      // For notify: send { aliases_yaml: content }
      if (path.startsWith('/bin/')) {
        xhr.send(JSON.stringify({ source: content }))
      } else if (path.startsWith('/cron.d/')) {
        // Parse the cron file format client-side to match the API contract.
        const parsed = parseCronFile(content)
        xhr.send(JSON.stringify(parsed))
      } else if (path === '/etc/notify.conf') {
        xhr.send(JSON.stringify({ aliases_yaml: content }))
      }
    } else if (method === 'GET') {
      xhr.send()
    } else if (method === 'DELETE') {
      xhr.send()
    }
  } catch {
    return { error: 'network error' }
  }

  if (xhr.status === 404 || xhr.status === 204) {
    if (method === 'DELETE') return {}
    return { error: `${path}: not found` }
  }
  if (xhr.status >= 400) {
    return { error: xhr.responseText || `HTTP ${xhr.status}` }
  }

  if (method === 'GET') {
    try {
      const data = JSON.parse(xhr.responseText)
      // Functions return { Source: "..." }, watches return { Pipeline: "...", Schedule: "..." }
      // Notify returns { aliases_yaml: "..." }
      if (data.Source !== undefined) {
        return { content: data.Source }
      }
      if (data.Pipeline !== undefined) {
        // Serialize back to cron file format.
        return { content: serializeCronFile(data) }
      }
      if (data.aliases_yaml !== undefined) {
        return { content: data.aliases_yaml }
      }
      return { content: xhr.responseText }
    } catch {
      return { content: xhr.responseText }
    }
  }

  return {}
}

function fetchApps(): string[] | null {
  const token = localStorage.getItem('weft_token')
  const xhr = new XMLHttpRequest()
  xhr.open('GET', `${API_BASE}/api/apps`, false)
  xhr.setRequestHeader('Content-Type', 'application/json')
  if (token) xhr.setRequestHeader('Authorization', `Bearer ${token}`)
  try { xhr.send() } catch { return null }
  if (xhr.status >= 400) return null
  try {
    return JSON.parse(xhr.responseText) as string[]
  } catch {
    return null
  }
}

function fsApiSyncDir(path: string): { entries?: string[]; error?: string } {
  const token = localStorage.getItem('weft_token')
  let endpoint: string

  if (path === '/' || path === '') {
    return { entries: ['bin/', 'cron.d/', 'log/', 'etc/'] }
  } else if (path === '/bin' || path === '/bin/') {
    endpoint = '/api/functions'
  } else if (path === '/cron.d' || path === '/cron.d/') {
    endpoint = '/api/watches'
  } else if (path === '/log' || path === '/log/' || path.startsWith('/log/')) {
    const apps = fetchApps()
    if (apps === null) return { entries: [] }

    const norm = path.replace(/\/+$/, '')
    if (norm === '/log') {
      // Group by namespace: apps with "/" show as dirs, flat apps as .log
      const seen = new Set<string>()
      const entries: string[] = []
      for (const app of apps) {
        const slash = app.indexOf('/')
        if (slash >= 0) {
          const ns = app.substring(0, slash)
          if (!seen.has(ns)) {
            seen.add(ns)
            entries.push(ns + '/')
          }
        } else {
          entries.push(app + '.log')
        }
      }
      return { entries }
    }

    // /log/<namespace> — filter apps under this prefix
    const ns = norm.substring('/log/'.length)
    const prefix = ns + '/'
    const entries = apps
      .filter(a => a.startsWith(prefix))
      .map(a => a.substring(prefix.length) + '.log')
    if (entries.length > 0) return { entries }
    return { error: `${path}: no such directory` }
  } else if (path === '/etc' || path === '/etc/') {
    return { entries: ['notify.conf', 'usage'] }
  } else {
    return { error: `${path}: not a directory` }
  }

  const xhr = new XMLHttpRequest()
  xhr.open('GET', `${API_BASE}${endpoint}`, false)
  xhr.setRequestHeader('Content-Type', 'application/json')
  if (token) xhr.setRequestHeader('Authorization', `Bearer ${token}`)

  try { xhr.send() } catch { return { error: 'network error' } }

  if (xhr.status >= 400) {
    return { error: xhr.responseText || `HTTP ${xhr.status}` }
  }

  try {
    const parsed = JSON.parse(xhr.responseText)
    // Both endpoints return envelopes: { functions: [...] } or { watches: [...] }
    const items: { Name: string }[] = parsed.functions || parsed.watches || []
    return { entries: (items || []).map(i => i.Name) }
  } catch {
    return { entries: [] }
  }
}

function fsGetWatchResults(name: string, limit: number): { results?: { stdout: string; ran_at: string }[]; error?: string } {
  const token = localStorage.getItem('weft_token')
  const xhr = new XMLHttpRequest()
  xhr.open('GET', `${API_BASE}/api/watches/${encodeURIComponent(name)}/results`, false)
  xhr.setRequestHeader('Content-Type', 'application/json')
  if (token) xhr.setRequestHeader('Authorization', `Bearer ${token}`)
  try { xhr.send() } catch { return { error: 'network error' } }
  if (xhr.status >= 400) return { error: xhr.responseText || `HTTP ${xhr.status}` }
  try {
    const items = JSON.parse(xhr.responseText) as { Stdout: string; RanAt: string }[]
    return {
      results: (items || []).slice(0, limit || 20).map(r => ({
        stdout: r.Stdout,
        ran_at: r.RanAt,
      }))
    }
  } catch {
    return { results: [] }
  }
}

function parseCronFile(content: string): { schedule: string; pipeline: string } {
  let schedule = ''
  const pipelineLines: string[] = []
  for (const line of content.split('\n')) {
    const trimmed = line.trim()
    if (trimmed.startsWith('# schedule:')) {
      schedule = trimmed.replace('# schedule:', '').trim()
    } else if (trimmed && !trimmed.startsWith('#')) {
      pipelineLines.push(line)
    }
  }
  return { schedule, pipeline: pipelineLines.join('\n') }
}

function serializeCronFile(data: { Schedule: string; Pipeline: string; Enabled?: boolean }): string {
  let out = `# schedule: ${data.Schedule}\n`
  if (data.Enabled === false) out += `# enabled: false\n`
  out += data.Pipeline
  if (!data.Pipeline.endsWith('\n')) out += '\n'
  return out
}
