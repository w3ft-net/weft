// bridge.ts — Weft-specific xterm.js + WASM bridge.
//
// All filesystem operations proxy through /api/fs/ endpoints. The server's
// MountFS handles path routing, serialization, and validation — no per-path
// logic lives here.

import {
  loadShellWasm,
  type WasmCallbacks,
  type ShellBridge as BaseShellBridge,
} from '@warp/shellcore-wasm'
import { execQuery, execFollow, cancelQuery, reportCommandCompleted, setCurrentRangeSpec } from './network'

export type ShellBridge = BaseShellBridge

// TERM_THEME maps xterm's ANSI palette onto the app's tokens (index.css)
// so the terminal and the chrome around it read as one program: same
// indigo-black ground, cyan cursor/selection, xterm-256-style colours.
// Kept in hex because xterm can't read CSS variables.
export const TERM_THEME = {
  background: '#0a0a14',
  foreground: '#c0c0d0',
  cursor: '#5fafd7',
  cursorAccent: '#0a0a14',
  selectionBackground: 'rgba(95, 175, 215, 0.3)',
  black: '#252540',
  red: '#ff5f5f',
  green: '#5fd787',
  yellow: '#ffd75f',
  blue: '#5fafd7',
  magenta: '#af87ff',
  cyan: '#5fd7d7',
  white: '#c0c0d0',
  brightBlack: '#555577',
  brightRed: '#ff8787',
  brightGreen: '#87ffaf',
  brightYellow: '#ffaf00',
  brightBlue: '#87d7ff',
  brightMagenta: '#d7afff',
  brightCyan: '#87ffff',
  brightWhite: '#e0e0f0',
}

export interface ShellIdentity {
  user: string     // email-local-part for the prompt — alice@acme.test → "alice"
  customer: string // customer/team slug — "acme"
}

// ChartOpenSpec is the payload the Go-side `chart` handler hands us
// in place of rendering ASCII. Either `watch` (chart <name>) or
// `lines` (... | chart) is populated.
export interface ChartOpenSpec {
  watch?: string
  lines?: string[]
  kind?: string
  title?: string
  limit?: number
}

// TraceOpenSpec is the payload the Go-side `trace` handler hands us in
// place of rendering ANSI/entering spanview's terminal View. `mode`
// distinguishes plain `trace <id>` ("gap") from `trace <id> --spans`
// ("rollup", Phase 1) from `trace spans [name]` ("aggregate", Phase
// 2) — see shell.TraceRequest (shell/shell.go). Unlike ChartOpenSpec,
// no rows travel with this payload: `trace` is always a pipeline's
// source command, never a pipe-stage destination, so there's no
// captured upstream snapshot to hand over the way `... | chart` does.
// ShellTraceModal re-runs `pipeline` itself via the shell's existing
// one-shot query path.
export interface TraceOpenSpec {
  pipeline: string
  title: string
  mode: 'gap' | 'rollup' | 'aggregate'
}

export async function initShellWasm(
  container: HTMLElement,
  services: string[],
  onRangeChange?: (rangeText: string) => void,
  identity?: ShellIdentity,
  onDashboardNavigate?: (name: string) => void,
  onChartOpen?: (spec: ChartOpenSpec) => void,
  onTraceOpen?: (spec: TraceOpenSpec) => void,
): Promise<ShellBridge> {
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
    // warp's bridge still passes timeStartNs/timeEndNs as 2nd/3rd args
    // for vimgym compat; we ignore them. The server resolves the range
    // spec stored via setCurrentRangeSpec (see onRangeChange below).
    startQuery: (pipeline: string) => { execQuery(pipeline) },
    startFollow: (pipeline: string) => { execFollow(pipeline) },
    cancelQuery: () => cancelQuery(),
    notifyQuit: () => {},

    fsReadFile: (path) => fsRead(path),
    fsWriteFile: (path, content) => fsWrite(path, content),
    fsReadDir: (path) => fsList(path),
    fsRemove: (path) => fsRemove(path),

    loadHistory: () => localStorage.getItem('weft_history') || '',
    saveHistory: (lines) => localStorage.setItem('weft_history', lines),

    getUser: () => identity?.user ?? '',

    getCustomer: () => identity?.customer ?? '',
    getServices: () => services,
    fsGetWatchResults: (name: string, limit: number) => fsGetWatchResults(name, limit),
    // The Go-side wasm bridge calls onRangeChange(rawSpec, label):
    //   rawSpec  — un-resolved spec ("1d..", "30m..", ISO pair, etc).
    //              Stored for the network layer to ship on every fetch.
    //   label    — formatted display text for the chip.
    onRangeChange: (rawSpec: string, label?: string) => {
      setCurrentRangeSpec(rawSpec)
      if (onRangeChange) onRangeChange(label ?? rawSpec)
    },
    // dashboardNavigate is invoked by the `dashboard <name>` builtin
    // inside cmd/wasm. Bare `dashboard` passes "" — we treat that as
    // a request to land on the /dashboards index.
    dashboardNavigate: (name: string) => {
      if (onDashboardNavigate) onDashboardNavigate(name)
    },
    // chartOpen is invoked by both `chart <name>` and `... | chart`
    // in the browser. The Go side hands us the parsed spec; the React
    // side renders the actual tile.
    chartOpen: (spec: ChartOpenSpec) => {
      if (onChartOpen) onChartOpen(spec)
    },
    // traceOpen is invoked by the `trace` builtin inside cmd/wasm for
    // all three shapes (see shell.traceRequestBuiltin). The React side
    // renders list/samples in a modal, mirroring spanview's terminal
    // UX rather than ANSI scrollback.
    traceOpen: (spec: TraceOpenSpec) => {
      if (onTraceOpen) onTraceOpen(spec)
    },
    commandCompleted: (line: string, exitCode: number) => { void reportCommandCompleted(line, exitCode) },
  } as WasmCallbacks & {
    dashboardNavigate: (name: string) => void
    chartOpen: (spec: ChartOpenSpec) => void
    traceOpen: (spec: TraceOpenSpec) => void
  }

  const bridge = await loadShellWasm(container, {
    wasmUrl: '/shell.wasm',
    callbacks,
    themeOverrides: TERM_THEME,
  })

  terminalRef = bridge.terminal
  for (const data of earlyWrites) {
    bridge.terminal.write(data)
  }

  // OSC-52 handler — the `clipboard` builtin emits ESC ] 52 ; c ; <b64> BEL.
  // We decode and write to the system clipboard; returning true keeps the
  // sequence out of scrollback. The plain confirmation line the builtin
  // also prints stays visible as a fallback in case the clipboard API
  // is unavailable.
  bridge.terminal.parser.registerOscHandler(52, (data) => {
    // Payload looks like "c;<base64>" or "p;<base64>" — we accept either,
    // since the underlying selection is the user's clipboard either way.
    const semi = data.indexOf(';')
    if (semi < 0) return false
    const b64 = data.slice(semi + 1)
    let text: string
    try {
      text = atob(b64)
    } catch {
      return false
    }
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).catch(() => {})
    }
    return true
  })

  return bridge
}

// initShellPane boots one pane of a browser tmux. Unlike initShellWasm
// it registers the core under `instance` (so multiple panes coexist on
// the one Go runtime, all sharing the server-side FS) and installs an
// `interceptData` hook that the pane grid uses for the Ctrl-B prefix
// FSM. The FS / network / history callbacks are identical to the
// single-pane shell — they're stateless XHRs against /api, so every
// pane reading them sees the same tenant filesystem.
export async function initShellPane(
  container: HTMLElement,
  instance: string,
  services: string[],
  identity: ShellIdentity | undefined,
  interceptData: (data: string, forward: (d: string) => void) => void,
): Promise<ShellBridge> {
  let terminalRef: BaseShellBridge['terminal'] | null = null
  const earlyWrites: string[] = []

  const callbacks: WasmCallbacks = {
    write: (data) => {
      if (terminalRef) terminalRef.write(data)
      else earlyWrites.push(data)
    },
    // Scope query/follow to this pane so results return to the core
    // that started them (not reg.active() — the last-booted pane).
    startQuery: (pipeline: string) => { execQuery(pipeline, instance) },
    startFollow: (pipeline: string) => { execFollow(pipeline, instance) },
    cancelQuery: () => cancelQuery(instance),
    notifyQuit: () => {},

    fsReadFile: (path) => fsRead(path),
    fsWriteFile: (path, content) => fsWrite(path, content),
    fsReadDir: (path) => fsList(path),
    fsRemove: (path) => fsRemove(path),

    loadHistory: () => localStorage.getItem('weft_history') || '',
    saveHistory: (lines) => localStorage.setItem('weft_history', lines),

    getUser: () => identity?.user ?? '',
    getCustomer: () => identity?.customer ?? '',
    getServices: () => services,
    fsGetWatchResults: (name: string, limit: number) => fsGetWatchResults(name, limit),

    // Panes don't own the header range chip; still record the spec so
    // the network layer ships a sane window on each fetch.
    onRangeChange: (rawSpec: string) => { setCurrentRangeSpec(rawSpec) },
    // Dashboard/chart/trace navigation is a whole-page concern; no-op inside a pane.
    dashboardNavigate: () => {},
    chartOpen: () => {},
    traceOpen: () => {},
    commandCompleted: (line: string, exitCode: number) => { void reportCommandCompleted(line, exitCode) },
  } as WasmCallbacks

  const bridge = await loadShellWasm(container, {
    wasmUrl: '/shell.wasm',
    callbacks,
    themeOverrides: TERM_THEME,
    instance,
    interceptData,
  })

  terminalRef = bridge.terminal
  for (const data of earlyWrites) {
    bridge.terminal.write(data)
  }

  bridge.terminal.parser.registerOscHandler(52, (data) => {
    const semi = data.indexOf(';')
    if (semi < 0) return false
    const b64 = data.slice(semi + 1)
    let text: string
    try {
      text = atob(b64)
    } catch {
      return false
    }
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).catch(() => {})
    }
    return true
  })

  return bridge
}

// ── Synchronous FS proxy ───────────────────────────────────────
// All FS callbacks use synchronous XHR because Go WASM's
// js.Value.Call() blocks until the JS function returns.

const API_BASE = import.meta.env.VITE_API_URL || ''

function authHeaders(): Record<string, string> {
  const headers: Record<string, string> = { 'Content-Type': 'application/json' }
  const token = localStorage.getItem('weft_token')
  if (token) headers['Authorization'] = `Bearer ${token}`
  const org = localStorage.getItem('weft_org')
  if (org) headers['X-Active-Org'] = org
  return headers
}

function fsList(path: string): { entries?: string[]; error?: string } {
  const xhr = new XMLHttpRequest()
  xhr.open('GET', `${API_BASE}/api/fs/list?path=${encodeURIComponent(path)}`, false)
  for (const [k, v] of Object.entries(authHeaders())) xhr.setRequestHeader(k, v)
  try { xhr.send() } catch { return { error: 'network error' } }
  if (xhr.status >= 400) return { error: xhr.responseText || `HTTP ${xhr.status}` }
  try {
    const data = JSON.parse(xhr.responseText) as { entries: { name: string; is_dir: boolean }[] }
    return { entries: (data.entries || []).map(e => e.is_dir ? e.name + '/' : e.name) }
  } catch {
    return { entries: [] }
  }
}

function fsRead(path: string): { content?: string; error?: string } {
  const xhr = new XMLHttpRequest()
  xhr.open('GET', `${API_BASE}/api/fs/read?path=${encodeURIComponent(path)}`, false)
  for (const [k, v] of Object.entries(authHeaders())) xhr.setRequestHeader(k, v)
  try { xhr.send() } catch { return { error: 'network error' } }
  if (xhr.status >= 400) return { error: xhr.responseText || `HTTP ${xhr.status}` }
  return { content: xhr.responseText }
}

function fsWrite(path: string, content: string): { error?: string } {
  const xhr = new XMLHttpRequest()
  xhr.open('PUT', `${API_BASE}/api/fs/write`, false)
  for (const [k, v] of Object.entries(authHeaders())) xhr.setRequestHeader(k, v)
  try { xhr.send(JSON.stringify({ path, content })) } catch { return { error: 'network error' } }
  if (xhr.status >= 400) return { error: xhr.responseText || `HTTP ${xhr.status}` }
  return {}
}

function fsRemove(path: string): { error?: string } {
  const xhr = new XMLHttpRequest()
  xhr.open('DELETE', `${API_BASE}/api/fs/remove?path=${encodeURIComponent(path)}`, false)
  for (const [k, v] of Object.entries(authHeaders())) xhr.setRequestHeader(k, v)
  try { xhr.send() } catch { return { error: 'network error' } }
  if (xhr.status >= 400) return { error: xhr.responseText || `HTTP ${xhr.status}` }
  return {}
}

function fsGetWatchResults(name: string, limit: number): { results?: { stdout: string; ran_at: string }[]; error?: string } {
  const xhr = new XMLHttpRequest()
  xhr.open('GET', `${API_BASE}/api/watches/${encodeURIComponent(name)}/results`, false)
  for (const [k, v] of Object.entries(authHeaders())) xhr.setRequestHeader(k, v)
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
