import { useState, useEffect, useRef, useCallback } from 'react'
import { useSearchParams } from 'react-router-dom'
import SidebarNav from '../components/SidebarNav'
import {
  getFunctions, deleteFunction, type ShellFunction,
  getWatches, deleteWatch, type CronWatch,
} from '../lib/api'
import { initShellWasm, type ShellBridge } from '../wasm/bridge'
import { useAuth } from '../lib/AuthContext'
import '@xterm/xterm/css/xterm.css'
import './Shell.css'

// ── Main Shell ───────────────────────────────────────────────────────────────

export default function Shell() {
  const [searchParams] = useSearchParams()
  const { user } = useAuth()
  const termContainerRef = useRef<HTMLDivElement>(null)
  const bridgeRef = useRef<ShellBridge | null>(null)
  const [functions, setFunctions] = useState<ShellFunction[]>([])
  const [watches, setWatches] = useState<CronWatch[]>([])
  const [watchLimits, setWatchLimits] = useState({ count: 0, limit: 0, min_interval: '' })
  const [sidebarTab, setSidebarTab] = useState<'functions' | 'watches' | null>(null)
  const [, setWasmReady] = useState(false)
  const [rangeText, setRangeText] = useState('')

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

      // TODO: fetch real app list from API.
      const apps = ['api', 'web']

      const identity = user
        ? { user: user.email.split('@')[0], customer: user.customer }
        : undefined
      bridge = await initShellWasm(
        termContainerRef.current,
        apps,
        (text) => setRangeText(text.trim()),
        identity,
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
    }

    init()

    return () => {
      disposed = true
      bridge?.dispose()
      bridgeRef.current = null
    }
  }, []) // eslint-disable-line react-hooks/exhaustive-deps

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

  function toggleSidebar(tab: 'functions' | 'watches') {
    setSidebarTab(prev => prev === tab ? null : tab)
  }

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
            <span className="terminal-title" data-testid="shell-range-display">
              {rangeText || 'weft shell'}
            </span>
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
          <div className={`shell-query-sidebar${sidebarTab === 'watches' ? ' shell-query-sidebar-open' : ''}`}
            data-testid="shell-watches-sidebar">
            <div className="shell-query-sidebar-header">
              <span className="shell-query-sidebar-title">/cron.d</span>
              <span className="shell-query-sidebar-badge">{watchLimits.count}/{watchLimits.limit}</span>
              <button className="shell-query-sidebar-close" onClick={() => setSidebarTab(null)}
                data-testid="shell-watches-sidebar-close-button">×</button>
            </div>
            {watchLimits.limit === 0 ? (
              <div className="shell-query-empty">Watches not available on trial. <a href="/account">Upgrade</a> to enable.</div>
            ) : watches.length === 0 ? (
              <div className="shell-query-empty">
                No watches. Use <code>vi /cron.d/name</code> to create one.
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
    </div>
  )
}
