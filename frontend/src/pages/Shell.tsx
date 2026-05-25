import { useState, useEffect, useRef, useCallback } from 'react'
import { useSearchParams, useNavigate } from 'react-router-dom'
import SidebarNav from '../components/SidebarNav'
import {
  getFunctions, deleteFunction, type ShellFunction,
  getWatches, deleteWatch, type CronWatch,
  listSources,
} from '../lib/api'
import { initShellWasm, type ShellBridge, type ChartOpenSpec } from '../wasm/bridge'
import ShellChartModal from '../components/ShellChartModal'
import { useAuth } from '../lib/AuthContext'
import FileTreeSidebar from '../components/FileTreeSidebar'
import '@xterm/xterm/css/xterm.css'
import './Shell.css'

// ── Main Shell ───────────────────────────────────────────────────────────────

export default function Shell() {
  const [searchParams] = useSearchParams()
  const navigate = useNavigate()
  const { user } = useAuth()
  const termContainerRef = useRef<HTMLDivElement>(null)
  const bridgeRef = useRef<ShellBridge | null>(null)
  const [functions, setFunctions] = useState<ShellFunction[]>([])
  const [watches, setWatches] = useState<CronWatch[]>([])
  const [watchLimits, setWatchLimits] = useState({ count: 0, limit: 0, min_interval: '' })
  const [sidebarTab, setSidebarTab] = useState<'functions' | 'watches' | 'tmp' | 'etc' | null>(null)
  const [wasmReady, setWasmReady] = useState(false)
  const [chartSpec, setChartSpec] = useState<ChartOpenSpec | null>(null)

  useEffect(() => {
    getFunctions().then(setFunctions).catch(() => {})
    getWatches().then(resp => {
      setWatches(resp.watches || [])
      setWatchLimits({ count: resp.count, limit: resp.limit, min_interval: resp.min_interval })
    }).catch(() => {})
  }, [])

  // Initialize WASM + xterm.js.
  useEffect(() => {
    if (!termContainerRef.current) return

    let disposed = false
    let bridge: ShellBridge | null = null

    async function init() {
      if (!termContainerRef.current || disposed) return

      // The shell seeds tab-completion / pseudofile listing with the
      // customer's configured services. Pull the live list (deduped,
      // since a service can have both a CloudWatch and a weftd source);
      // an empty list is fine — the shell just starts with nothing to
      // complete and picks up sources as the server reports them.
      const sources = await listSources().catch(() => [])
      const services = Array.from(new Set(sources.map(s => s.service))).sort()

      const identity = user
        ? { user: user.email.split('@')[0], customer: user.customer }
        : undefined
      bridge = await initShellWasm(
        termContainerRef.current,
        services,
        undefined, // range display removed; range is shown in PS1 + accessible via `range`
        identity,
        // `dashboard <name>` at the prompt routes to the dashboards
        // page. Empty name = land on the index.
        (name: string) => {
          navigate(name ? `/dashboards/${encodeURIComponent(name)}` : '/dashboards')
        },
        // `chart <name>` and `... | chart` open a modal with the
        // chart tile rendered in React. The modal owns its own
        // re-fetch for the watch form so it stays live.
        (spec: ChartOpenSpec) => setChartSpec(spec),
      )
      if (disposed) {
        bridge.dispose()
        return
      }
      bridgeRef.current = bridge
      setWasmReady(true)

      // Pre-fill from URL param.
      const q = searchParams.get('q')
      if (q) {
        bridge.setInput(decodeURIComponent(q))
      }

      // Pre-fill from a `clipboard`-shared deep link:
      //   ?bin=/bin/<name>&range=<spec>&args=<scriptArgs>
      // We don't auto-run — user reviews and hits Enter.
      const bin = searchParams.get('bin')
      if (bin) {
        const rng = searchParams.get('range') || ''
        const scriptArgs = searchParams.get('args') || ''
        const cmd = scriptArgs ? `${bin} ${scriptArgs}` : bin
        const line = rng ? `range ${rng}; ${cmd}` : cmd
        bridge.setInput(line)
      }
    }

    init()

    return () => {
      disposed = true
      bridge?.dispose()
      bridgeRef.current = null
    }
  }, []) // eslint-disable-line react-hooks/exhaustive-deps

  // Inject the current lesson's suggested command into the input
  // buffer whenever the onboarding step advances. The user only has
  // to hit enter — they can still edit or clear it. We trigger on
  // step change rather than on every user refresh so we don't fight
  // edits the user has made mid-lesson.
  const step = user?.onboarding_step
  const action = user?.onboarding_action
  useEffect(() => {
    if (!wasmReady || !bridgeRef.current) return
    if (!step || !step.startsWith('shell-') || !action) return
    bridgeRef.current.setInput(action)
    bridgeRef.current.terminal.focus()
  }, [wasmReady, step, action])

  // The tour overlay's re-paste button fires this event; we don't
  // pass the action through the event since the latest value is
  // already in user state.
  useEffect(() => {
    if (!wasmReady) return
    const onPaste = () => {
      const a = user?.onboarding_action
      if (!a || !bridgeRef.current) return
      bridgeRef.current.setInput(a)
      bridgeRef.current.terminal.focus()
    }
    window.addEventListener('tour-action-paste', onPaste)
    return () => window.removeEventListener('tour-action-paste', onPaste)
  }, [wasmReady, user?.onboarding_action])

  const handleFunctionSelect = useCallback((name: string) => {
    bridgeRef.current?.setInput(name)
    setSidebarTab(null)
    bridgeRef.current?.terminal.focus()
  }, [])

  const handleWatchSelect = useCallback((name: string) => {
    bridgeRef.current?.setInput(`cat /cron.d/${name}`)
    setSidebarTab(null)
    bridgeRef.current?.terminal.focus()
  }, [])

  async function handleFunctionDelete(name: string) {
    await deleteFunction(name)
    setFunctions(prev => prev.filter(f => f.Name !== name))
  }

  async function handleWatchDelete(name: string) {
    await deleteWatch(name)
    setWatches(prev => prev.filter(w => w.Name !== name))
  }

  function toggleSidebar(tab: 'functions' | 'watches' | 'tmp' | 'etc') {
    setSidebarTab(prev => prev === tab ? null : tab)
  }

  // handleFSPick is the file-tree leaf callback. Both /tmp and /etc
  // sidebars use it; the distinction was relevant when /etc was a
  // .conf-only world (cat) and /tmp was a scratch world (vim), but
  // both currently want `cat` since the tour is read-only and edits
  // happen via vi from the prompt anyway.
  const handleFSPick = useCallback((absPath: string) => {
    bridgeRef.current?.setInput(`cat ${absPath}`)
    setSidebarTab(null)
    bridgeRef.current?.terminal.focus()
  }, [])

  return (
    <div className="app-layout">
      <SidebarNav />
      <main className="app-main shell-main" data-testid="shell-main">
        <div className="shell-wrap">
          {/* Chrome bar */}
          <div className="shell-chrome" data-testid="shell-chrome">
            <div className="terminal-dots">
              <span className="dot red" />
              <span className="dot yellow" />
              <span className="dot green" />
            </div>
            <div className="shell-chrome-actions">
              <button
                className={`shell-chrome-btn${sidebarTab === 'functions' ? ' shell-chrome-btn-active' : ''}`}
                onClick={() => toggleSidebar('functions')}
                data-testid="shell-functions-toggle-button"
              >
                /bin
              </button>
              <button
                className={`shell-chrome-btn${sidebarTab === 'watches' ? ' shell-chrome-btn-active' : ''}`}
                onClick={() => toggleSidebar('watches')}
                data-testid="shell-watches-toggle-button"
              >
                /cron.d
              </button>
              <button
                className={`shell-chrome-btn${sidebarTab === 'tmp' ? ' shell-chrome-btn-active' : ''}`}
                onClick={() => toggleSidebar('tmp')}
                data-testid="shell-tmp-toggle-button"
              >
                /tmp
              </button>
              <button
                className={`shell-chrome-btn${sidebarTab === 'etc' ? ' shell-chrome-btn-active' : ''}`}
                onClick={() => toggleSidebar('etc')}
                data-testid="shell-etc-toggle-button"
              >
                /etc
              </button>
            </div>
          </div>

          {/* xterm.js terminal container */}
          <div
            className="shell-xterm-container"
            ref={termContainerRef}
            onClick={() => bridgeRef.current?.terminal.focus()}
            data-testid="shell-xterm-container"
          />

          {/* Sidebar panels */}
          <div className={`shell-query-sidebar${sidebarTab === 'functions' ? ' shell-query-sidebar-open' : ''}`}
            data-testid="shell-functions-sidebar">
            <div className="shell-query-sidebar-header">
              <span className="shell-query-sidebar-title">/bin</span>
              <button className="shell-query-sidebar-close" onClick={() => setSidebarTab(null)}
                data-testid="shell-functions-sidebar-close-button">×</button>
            </div>
            {functions.length === 0 ? (
              <div className="shell-query-empty">No functions. Use <code>vi /bin/name</code> to create one.</div>
            ) : (
              <ul className="shell-query-list">
                {functions.map(f => (
                  <li key={f.Name} className="shell-query-item" data-testid={`shell-function-item-${f.Name}`}>
                    <button className="shell-query-name" onClick={() => handleFunctionSelect(f.Name)}
                      data-testid={`shell-function-select-${f.Name}`}>{f.Name}</button>
                    <div className="shell-query-pipeline">{f.Source.split('\n')[0]}</div>
                    <button className="shell-query-delete" onClick={() => handleFunctionDelete(f.Name)}
                      data-testid={`shell-function-delete-${f.Name}`}>✕</button>
                  </li>
                ))}
              </ul>
            )}
          </div>
          <FileTreeSidebar
            root="/tmp"
            open={sidebarTab === 'tmp'}
            onClose={() => setSidebarTab(null)}
            onPickFile={handleFSPick}
            testIdPrefix="shell-tmp"
          />
          <FileTreeSidebar
            root="/etc"
            open={sidebarTab === 'etc'}
            onClose={() => setSidebarTab(null)}
            onPickFile={handleFSPick}
            testIdPrefix="shell-etc"
          />
          <div className={`shell-query-sidebar${sidebarTab === 'watches' ? ' shell-query-sidebar-open' : ''}`}
            data-testid="shell-watches-sidebar">
            <div className="shell-query-sidebar-header">
              <span className="shell-query-sidebar-title">/cron.d</span>
              <span className="shell-query-sidebar-badge">{watchLimits.count}/{watchLimits.limit}</span>
              <button className="shell-query-sidebar-close" onClick={() => setSidebarTab(null)}
                data-testid="shell-watches-sidebar-close-button">×</button>
            </div>
            {watchLimits.limit === 0 ? (
              <div className="shell-query-empty">Crons not available on trial. <a href="/account">Upgrade</a> to enable.</div>
            ) : watches.length === 0 ? (
              <div className="shell-query-empty">
                No crons. Use <code>vi /cron.d/name</code> to create one.
                {watchLimits.min_interval && <><br/>Min interval: {watchLimits.min_interval}</>}
              </div>
            ) : (
              <ul className="shell-query-list">
                {watches.map(w => (
                  <li key={w.Name} className="shell-query-item" data-testid={`shell-watch-item-${w.Name}`}>
                    <button className="shell-query-name" onClick={() => handleWatchSelect(w.Name)}
                      data-testid={`shell-watch-select-${w.Name}`}>{w.Name}</button>
                    <div className="shell-query-pipeline">
                      {w.Schedule} · {w.LastStatus || 'pending'}
                    </div>
                    <button className="shell-query-delete" onClick={() => handleWatchDelete(w.Name)}
                      data-testid={`shell-watch-delete-${w.Name}`}>✕</button>
                  </li>
                ))}
              </ul>
            )}
          </div>
        </div>

      </main>

      {chartSpec && (
        <ShellChartModal spec={chartSpec} onClose={() => setChartSpec(null)} />
      )}
    </div>
  )
}
