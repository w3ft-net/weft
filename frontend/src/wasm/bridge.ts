// bridge.ts — Weft-specific xterm.js + WASM bridge.
//
// Wraps @warp/shellcore-wasm's loadShellWasm with the weft-specific
// callbacks: API-backed virtual filesystem (/bin → /api/functions,
// /cron.d → /api/watches, /etc → /api/notify), watch-results lookup,
// time-range display callback, and identity (user@customer).

import {
  loadShellWasm,
  type WasmCallbacks,
  type ShellBridge as BaseShellBridge,
} from '@warp/shellcore-wasm'
import { execQuery, execFollow, cancelQuery } from './network'

export type ShellBridge = BaseShellBridge

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
  // The shared package owns the Terminal; we route writes into it
  // after loadShellWasm returns. Until then, buffer any writes that
  // arrive synchronously during wasmInit.
  let terminalRef: BaseShellBridge['terminal'] | null = null
  const earlyWrites: string[] = []

  const callbacks: WasmCallbacks = {
    write: (data) => {
      if (terminalRef) {
        terminalRef.write(data)
      } else {
        earlyWrites.push(data)
      }
    },
    startQuery: (pipeline) => execQuery(pipeline),
    startFollow: (pipeline) => execFollow(pipeline),
    cancelQuery: () => cancelQuery(),
    notifyQuit: () => {},

    fsReadFile: (path) => fsApiSync('GET', path),
    fsWriteFile: (path, content) => fsApiSync('PUT', path, content),
    fsReadDir: (path) => fsApiSyncDir(path),
    fsRemove: (path) => fsApiSync('DELETE', path),

    loadHistory: () => localStorage.getItem('weft_history') || '',
    saveHistory: (lines) => localStorage.setItem('weft_history', lines),

    getUser: () => identity?.user ?? '',

    // Weft-specific extras. The shared package treats them as
    // opaque fields and forwards them as-is to the WASM side.
    getCustomer: () => identity?.customer ?? '',
    getApps: () => apps,
    fsGetWatchResults: (name: string, limit: number) => fsGetWatchResults(name, limit),
    onRangeChange: (rangeText: string) => {
      if (onRangeChange) onRangeChange(rangeText)
    },
  }

  const bridge = await loadShellWasm(container, {
    wasmUrl: '/shell.wasm',
    callbacks,
  })

  terminalRef = bridge.terminal
  for (const data of earlyWrites) {
    bridge.terminal.write(data)
  }

  return bridge
}

// ── Synchronous FS API helpers ──────────────────────────────
// These use XMLHttpRequest (synchronous mode) because Go WASM's
// js.Value.Call() blocks until the JS function returns.

const API_BASE = import.meta.env.VITE_API_URL || ''

function fsApiPath(fsPath: string): { endpoint: string; name: string } | null {
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
  xhr.open(method, `${API_BASE}${mapped.endpoint}`, false)
  xhr.setRequestHeader('Content-Type', 'application/json')
  if (token) xhr.setRequestHeader('Authorization', `Bearer ${token}`)

  try {
    if (method === 'PUT' && content !== undefined) {
      if (path.startsWith('/bin/')) {
        xhr.send(JSON.stringify({ source: content }))
      } else if (path.startsWith('/cron.d/')) {
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
      if (data.Source !== undefined) {
        return { content: data.Source }
      }
      if (data.Pipeline !== undefined) {
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
