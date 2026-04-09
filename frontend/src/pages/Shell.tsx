import { useState, useEffect, useRef, type KeyboardEvent, useCallback } from 'react'
import { useSearchParams, useNavigate } from 'react-router-dom'
import SidebarNav from '../components/SidebarNav'
import { getQueries, createQuery, deleteQuery, type SavedQuery } from '../lib/api'
import './Shell.css'

const API_BASE = import.meta.env.VITE_API_URL || ''

interface OutputLine {
  id: number
  text: string
  type: 'output' | 'error' | 'info' | 'prompt'
}

let lineId = 0
function nextId() { return ++lineId }

function formatTime() {
  return new Date().toLocaleTimeString('en-US', { hour12: false })
}

// ── Saved Queries Sidebar ────────────────────────────────────────────────────

interface QuerySidebarProps {
  queries: SavedQuery[]
  open: boolean
  onSelect: (q: string) => void
  onDelete: (id: string) => void
  onClose: () => void
}

function QuerySidebar({ queries, open, onSelect, onDelete, onClose }: QuerySidebarProps) {
  return (
    <div className={`shell-query-sidebar${open ? ' shell-query-sidebar-open' : ''}`}
      data-testid="shell-query-sidebar">
      <div className="shell-query-sidebar-header">
        <span className="shell-query-sidebar-title">Saved queries</span>
        <button
          className="shell-query-sidebar-close"
          onClick={onClose}
          data-testid="shell-query-sidebar-close-button"
        >×</button>
      </div>
      {queries.length === 0 ? (
        <div className="shell-query-empty">No saved queries.</div>
      ) : (
        <ul className="shell-query-list">
          {queries.map(q => (
            <li key={q.id} className="shell-query-item" data-testid={`shell-query-item-${q.id}`}>
              <button
                className="shell-query-name"
                onClick={() => onSelect(q.query)}
                data-testid={`shell-query-select-${q.id}`}
              >
                {q.name}
              </button>
              <div className="shell-query-pipeline">{q.query}</div>
              <button
                className="shell-query-delete"
                onClick={() => onDelete(q.id)}
                data-testid={`shell-query-delete-${q.id}`}
              >
                ✕
              </button>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

// ── Save Query Modal ─────────────────────────────────────────────────────────

interface SaveModalProps {
  pipeline: string
  onSave: (name: string) => Promise<void>
  onClose: () => void
}

function SaveModal({ pipeline, onSave, onClose }: SaveModalProps) {
  const [name, setName] = useState('')
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!name.trim()) return
    setError('')
    setLoading(true)
    try {
      await onSave(name.trim())
      onClose()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to save.')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="shell-modal-overlay" onClick={onClose} data-testid="shell-save-modal-overlay">
      <div
        className="shell-modal"
        onClick={e => e.stopPropagation()}
        data-testid="shell-save-modal"
      >
        <div className="shell-modal-title">Save query</div>
        <div className="shell-modal-pipeline">{pipeline}</div>
        <form onSubmit={handleSubmit}>
          <input
            className="shell-modal-input"
            placeholder="Query name"
            value={name}
            onChange={e => setName(e.target.value)}
            autoFocus
            data-testid="shell-save-query-name-input"
          />
          {error && <div className="shell-modal-error">{error}</div>}
          <div className="shell-modal-actions">
            <button
              type="submit"
              className="shell-modal-btn shell-modal-btn-primary"
              disabled={loading || !name.trim()}
              data-testid="shell-save-query-submit-button"
            >
              {loading ? 'Saving...' : 'Save'}
            </button>
            <button
              type="button"
              className="shell-modal-btn"
              onClick={onClose}
              data-testid="shell-save-query-cancel-button"
            >
              Cancel
            </button>
          </div>
        </form>
      </div>
    </div>
  )
}

// ── Main Shell ───────────────────────────────────────────────────────────────

export default function Shell() {
  const [searchParams] = useSearchParams()
  const navigate = useNavigate()

  const [lines, setLines] = useState<OutputLine[]>([
    {
      id: nextId(),
      text: `weft shell — ${formatTime()} — type a pipeline and press Enter`,
      type: 'info',
    },
  ])
  const [input, setInput] = useState('')
  const [history, setHistory] = useState<string[]>([])
  const [historyIdx, setHistoryIdx] = useState(-1)
  const [running, setRunning] = useState(false)
  const [queries, setQueries] = useState<SavedQuery[]>([])
  const [sidebarOpen, setSidebarOpen] = useState(false)
  const [saveModal, setSaveModal] = useState(false)

  const outputRef = useRef<HTMLDivElement>(null)
  const inputRef = useRef<HTMLInputElement>(null)
  const abortRef = useRef<AbortController | null>(null)

  // Load saved queries
  useEffect(() => {
    getQueries().then(setQueries).catch(() => {})
  }, [])

  // Pre-fill from URL param
  useEffect(() => {
    const q = searchParams.get('q')
    if (q) {
      setInput(decodeURIComponent(q))
      inputRef.current?.focus()
    }
  }, [searchParams])

  // Auto-scroll
  useEffect(() => {
    if (outputRef.current) {
      outputRef.current.scrollTop = outputRef.current.scrollHeight
    }
  }, [lines])

  function pushLine(text: string, type: OutputLine['type'] = 'output') {
    setLines(prev => [...prev, { id: nextId(), text, type }])
  }

  function clearOutput() {
    setLines([{ id: nextId(), text: `cleared — ${formatTime()}`, type: 'info' }])
  }

  const runPipeline = useCallback(async (pipeline: string) => {
    if (!pipeline.trim()) return
    if (running) {
      abortRef.current?.abort()
      return
    }

    const trimmed = pipeline.trim()
    pushLine(`weft > ${trimmed}`, 'prompt')
    setHistory(prev => [trimmed, ...prev.filter(h => h !== trimmed)].slice(0, 100))
    setHistoryIdx(-1)
    setInput('')
    setRunning(true)

    const follow = trimmed.startsWith('tail -f')
    const token = localStorage.getItem('weft_token')
    const ctrl = new AbortController()
    abortRef.current = ctrl

    try {
      const res = await fetch(`${API_BASE}/api/shell/exec`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          ...(token ? { Authorization: `Bearer ${token}` } : {}),
        },
        body: JSON.stringify({ pipeline: trimmed, follow }),
        signal: ctrl.signal,
      })

      if (!res.ok) {
        const msg = await res.text().catch(() => `HTTP ${res.status}`)
        pushLine(msg, 'error')
        return
      }

      const reader = res.body?.getReader()
      if (!reader) {
        pushLine('No response body', 'error')
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
              pushLine(evt.text, 'output')
            } else if (evt.type === 'error' && evt.text != null) {
              pushLine(evt.text, 'error')
            }
          } catch {
            // skip malformed SSE
          }
        }
      }
    } catch (err) {
      if ((err as Error).name !== 'AbortError') {
        pushLine(`Error: ${(err as Error).message}`, 'error')
      }
    } finally {
      setRunning(false)
      abortRef.current = null
      inputRef.current?.focus()
    }
  }, [running])

  function handleKeyDown(e: KeyboardEvent<HTMLInputElement>) {
    if (e.key === 'Enter') {
      if (input.trim() === 'clear') {
        clearOutput()
        setInput('')
        setHistoryIdx(-1)
        return
      }
      runPipeline(input)
    } else if (e.key === 'ArrowUp') {
      e.preventDefault()
      const idx = historyIdx + 1
      if (idx < history.length) {
        setHistoryIdx(idx)
        setInput(history[idx])
      }
    } else if (e.key === 'ArrowDown') {
      e.preventDefault()
      const idx = historyIdx - 1
      if (idx < 0) {
        setHistoryIdx(-1)
        setInput('')
      } else {
        setHistoryIdx(idx)
        setInput(history[idx])
      }
    } else if (e.key === 'c' && e.ctrlKey) {
      if (running) {
        abortRef.current?.abort()
        pushLine('^C', 'info')
      }
    }
  }

  async function handleSaveQuery(name: string) {
    const q = await createQuery(name, input.trim() || history[0] || '')
    setQueries(prev => [q, ...prev])
  }

  function handleQuerySelect(q: string) {
    setInput(q)
    setSidebarOpen(false)
    inputRef.current?.focus()
  }

  async function handleQueryDelete(id: string) {
    await deleteQuery(id)
    setQueries(prev => prev.filter(q => q.id !== id))
  }

  function handleRunInShell(q: string) {
    navigate(`/shell?q=${encodeURIComponent(q)}`)
  }
  void handleRunInShell

  return (
    <div className="app-layout">
      <SidebarNav />
      <main className="app-main shell-main" data-testid="shell-main">
        {/* Terminal wrapper fills the remaining space */}
        <div className="shell-wrap">
          {/* Chrome bar */}
          <div className="shell-chrome" data-testid="shell-chrome">
            <div className="terminal-dots">
              <span className="dot red" />
              <span className="dot yellow" />
              <span className="dot green" />
            </div>
            <span className="terminal-title">weft shell — w3ft.net</span>
            <div className="shell-chrome-actions">
              <button
                className="shell-chrome-btn"
                onClick={() => setSidebarOpen(o => !o)}
                data-testid="shell-queries-toggle-button"
                title="Saved queries"
              >
                queries
              </button>
              {(input.trim() || (history.length > 0)) && (
                <button
                  className="shell-chrome-btn"
                  onClick={() => setSaveModal(true)}
                  data-testid="shell-save-query-button"
                  title="Save current pipeline"
                >
                  save
                </button>
              )}
              {running && (
                <button
                  className="shell-chrome-btn shell-chrome-btn-danger"
                  onClick={() => abortRef.current?.abort()}
                  data-testid="shell-interrupt-button"
                >
                  interrupt
                </button>
              )}
            </div>
          </div>

          {/* Body */}
          <div className="shell-body-wrap">
            {/* Output */}
            <div
              className="shell-output"
              ref={outputRef}
              onClick={() => inputRef.current?.focus()}
              data-testid="shell-output"
            >
              {lines.map(line => (
                <div
                  key={line.id}
                  className={`shell-line shell-line-${line.type}`}
                  data-testid={`shell-line-${line.type}`}
                >
                  {line.type === 'prompt' ? (
                    <>
                      <span className="shell-prompt-text">
                        {line.text.replace(/^weft > /, '')}
                      </span>
                    </>
                  ) : (
                    line.text
                  )}
                </div>
              ))}
              {running && (
                <div className="shell-line shell-line-info shell-running-indicator"
                  data-testid="shell-running-indicator">
                  <span className="shell-spinner" />
                  streaming...
                </div>
              )}
            </div>

            {/* Input row */}
            <div className="shell-input-row" data-testid="shell-input-row">
              <span className="shell-prompt-label">weft &gt;</span>
              <input
                ref={inputRef}
                type="text"
                className="shell-input"
                value={input}
                onChange={e => setInput(e.target.value)}
                onKeyDown={handleKeyDown}
                placeholder="cat app.log | grep ERROR"
                autoFocus
                autoComplete="off"
                spellCheck={false}
                data-testid="shell-pipeline-input"
                aria-label="Pipeline input"
              />
              <button
                className={`shell-run-btn${running ? ' shell-run-btn-running' : ''}`}
                onClick={() => running ? abortRef.current?.abort() : runPipeline(input)}
                data-testid="shell-run-button"
              >
                {running ? '■' : '↵'}
              </button>
            </div>
          </div>

          {/* Saved queries sidebar */}
          <QuerySidebar
            queries={queries}
            open={sidebarOpen}
            onSelect={handleQuerySelect}
            onDelete={handleQueryDelete}
            onClose={() => setSidebarOpen(false)}
          />
        </div>

        {/* Save modal */}
        {saveModal && (
          <SaveModal
            pipeline={input.trim() || history[0] || ''}
            onSave={handleSaveQuery}
            onClose={() => setSaveModal(false)}
          />
        )}
      </main>
    </div>
  )
}
