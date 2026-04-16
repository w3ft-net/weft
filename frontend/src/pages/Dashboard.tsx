import { useState, useEffect } from 'react'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
import SidebarNav from '../components/SidebarNav'
import {
  getWatches, getWatchResults, getFunctions, getIngestionStatus,
  type CronWatch, type CronResult, type ShellFunction,
} from '../lib/api'
import './Dashboard.css'

function timeAgo(iso: string): string {
  const seconds = Math.floor((Date.now() - new Date(iso).getTime()) / 1000)
  if (seconds < 60) return `${seconds}s ago`
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes}m ago`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours}h ago`
  return `${Math.floor(hours / 24)}d ago`
}

// ── Sparkline ──────────────────────────────────────────────────────────────

function Sparkline({ values }: { values: number[] }) {
  if (values.length < 2) return null

  const w = 120
  const h = 30
  const pad = 2
  const min = Math.min(...values)
  const max = Math.max(...values)
  const range = max - min || 1

  const points = values.map((v, i) => {
    const x = pad + (i / (values.length - 1)) * (w - pad * 2)
    const y = h - pad - ((v - min) / range) * (h - pad * 2)
    return `${x},${y}`
  }).join(' ')

  return (
    <svg width={w} height={h} viewBox={`0 0 ${w} ${h}`} className="watch-sparkline">
      <polyline
        points={points}
        fill="none"
        stroke="var(--accent)"
        strokeWidth="1.5"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
      {values.length > 0 && (() => {
        const lastX = w - pad
        const lastY = h - pad - ((values[values.length - 1] - min) / range) * (h - pad * 2)
        return <circle cx={lastX} cy={lastY} r="2.5" fill="var(--accent)" />
      })()}
    </svg>
  )
}

// ── Watches Section ────────────────────────────────────────────────────────

interface WatchWithResults {
  watch: CronWatch
  results: CronResult[]
}

function WatchesCard({ watches, watchCount, watchLimit, minInterval }: {
  watches: WatchWithResults[]
  watchCount: number
  watchLimit: number
  minInterval: string
}) {
  const navigate = useNavigate()
  const atLimit = watchCount >= watchLimit
  const noWatches = watchLimit === 0

  if (noWatches) {
    return (
      <div className="dash-card" data-testid="dashboard-watches-card">
        <div className="dash-card-header">
          <h2 className="dash-card-title">Watches</h2>
        </div>
        <div className="dash-empty">
          Watches are not available on the trial plan.{' '}
          <a href="/account">Upgrade</a> to enable scheduled monitoring.
        </div>
      </div>
    )
  }

  if (watches.length === 0) {
    return (
      <div className="dash-card" data-testid="dashboard-watches-card">
        <div className="dash-card-header">
          <h2 className="dash-card-title">Watches</h2>
          <span className="watch-count-badge">{watchCount} / {watchLimit}</span>
        </div>
        <div className="dash-empty">
          No watches yet. Create one in the{' '}
          <a href="/shell" data-testid="dashboard-watches-shell-link">shell</a>
          {' '}with <code>vi /cron.d/my-watch</code>
          {minInterval && <span className="dash-empty"> (min interval: {minInterval})</span>}
        </div>
      </div>
    )
  }

  return (
    <div className="dash-card" data-testid="dashboard-watches-card">
      <div className="dash-card-header">
        <h2 className="dash-card-title">Watches</h2>
        <span className={`watch-count-badge${atLimit ? ' watch-count-at-limit' : ''}`}>
          {watchCount} / {watchLimit}
        </span>
      </div>
      <div className="watch-grid">
        {watches.map(({ watch, results }) => {
          const latestResult = results[0]
          const latestValue = latestResult?.Stdout?.trim() ?? '—'
          const sparkValues = results
            .slice(0, 20)
            .reverse()
            .map(r => parseFloat(r.Stdout?.trim()))
            .filter(n => !isNaN(n))
          const hasError = latestResult && latestResult.ExitCode !== 0
          const lastRanAgo = latestResult ? timeAgo(latestResult.RanAt) : null

          return (
            <div
              key={watch.Name}
              className={`watch-tile${hasError ? ' watch-tile-errored' : ''}`}
              data-testid={`dashboard-watch-card-${watch.Name}`}
            >
              <div className="watch-tile-header">
                <span className="watch-tile-name">{watch.Name}</span>
                <span className="watch-tile-schedule">{watch.Schedule}</span>
              </div>

              <div className="watch-tile-body">
                <div className="watch-tile-value" data-testid={`dashboard-watch-value-${watch.Name}`}>
                  {latestValue}
                </div>
                <Sparkline values={sparkValues} />
              </div>

              <div className="watch-tile-footer">
                {lastRanAgo ? (
                  <span className={`watch-tile-ago${hasError ? ' watch-tile-ago-err' : ''}`}>
                    {hasError ? 'failed ' : ''}{lastRanAgo}
                  </span>
                ) : (
                  <span className="watch-tile-ago">waiting for first run</span>
                )}
                {watch.NotifyDest && (
                  <span className="watch-tile-notify">{watch.NotifyDest}</span>
                )}
                <button
                  className="watch-tile-open"
                  onClick={() => navigate(`/shell?q=${encodeURIComponent(`cat /cron.d/${watch.Name}`)}`)}
                  data-testid={`dashboard-watch-open-${watch.Name}`}
                >
                  open in shell
                </button>
              </div>
            </div>
          )
        })}
      </div>
    </div>
  )
}

// ── Functions Section ──────────────────────────────────────────────────────

function FunctionsCard({ functions }: { functions: ShellFunction[] }) {
  const navigate = useNavigate()
  const [expanded, setExpanded] = useState<string | null>(null)

  if (functions.length === 0) {
    return (
      <div className="dash-card" data-testid="dashboard-functions-card">
        <div className="dash-card-header">
          <h2 className="dash-card-title">Functions</h2>
        </div>
        <div className="dash-empty">
          No functions yet. Create one in the{' '}
          <a href="/shell" data-testid="dashboard-functions-shell-link">shell</a>
          {' '}with <code>vi /bin/my-function</code>
        </div>
      </div>
    )
  }

  return (
    <div className="dash-card" data-testid="dashboard-functions-card">
      <div className="dash-card-header">
        <h2 className="dash-card-title">Functions</h2>
        <span className="watch-count-badge">{functions.length} in /bin</span>
      </div>
      <div className="func-list">
        {functions.map(fn => (
          <div key={fn.Name} className="func-row" data-testid={`dashboard-function-${fn.Name}`}>
            <button
              className="func-name"
              onClick={() => setExpanded(expanded === fn.Name ? null : fn.Name)}
              data-testid={`dashboard-function-toggle-${fn.Name}`}
            >
              <span className="func-name-text">{fn.Name}</span>
              <span className="func-chevron">{expanded === fn.Name ? '▾' : '▸'}</span>
            </button>

            {expanded === fn.Name && (
              <div className="func-detail">
                <pre className="func-source" data-testid={`dashboard-function-source-${fn.Name}`}>
                  {fn.Source}
                </pre>
                <div className="func-actions">
                  <button
                    className="btn-action btn-action-primary"
                    onClick={() => navigate(`/shell?q=${encodeURIComponent(fn.Name)}`)}
                    data-testid={`dashboard-function-run-${fn.Name}`}
                  >
                    Run in shell
                  </button>
                  <button
                    className="btn-action"
                    onClick={() => navigate(`/shell?q=${encodeURIComponent(`vi /bin/${fn.Name}`)}`)}
                    data-testid={`dashboard-function-edit-${fn.Name}`}
                  >
                    Edit
                  </button>
                </div>
              </div>
            )}
          </div>
        ))}
      </div>
    </div>
  )
}

// ── Welcome Modal ─────────────────────────────────────────────────────────

function WelcomeModal({ onDismiss }: { onDismiss: () => void }) {
  return (
    <div className="welcome-overlay" data-testid="welcome-modal">
      <div className="welcome-card">
        <h2 className="welcome-title">Welcome to weft</h2>
        <p className="welcome-text">
          Your account is ready. Head over to the{' '}
          <Link to="/docs" onClick={onDismiss} data-testid="welcome-docs-link">docs</Link>{' '}
          to get started with your first namespace, install the agent, and run your first query.
        </p>
        <button
          className="login-submit"
          onClick={onDismiss}
          data-testid="welcome-dismiss-button"
        >
          Got it
        </button>
      </div>
    </div>
  )
}

// ── Main Dashboard ─────────────────────────────────────────────────────────

export default function Dashboard() {
  const navigate = useNavigate()
  const [searchParams] = useSearchParams()
  const [showWelcome, setShowWelcome] = useState(false)
  const [watches, setWatches] = useState<WatchWithResults[]>([])
  const [watchMeta, setWatchMeta] = useState<{ count: number; limit: number; min_interval: string }>({ count: 0, limit: 0, min_interval: '' })
  const [functions, setFunctions] = useState<ShellFunction[]>([])
  const [affectedApps, setAffectedApps] = useState<string[]>([])

  useEffect(() => {
    if (searchParams.get('welcome') === '1' && !localStorage.getItem('weft_welcome_seen')) {
      setShowWelcome(true)
    }
  }, [searchParams])

  function dismissWelcome() {
    localStorage.setItem('weft_welcome_seen', '1')
    setShowWelcome(false)
    navigate('/dashboard', { replace: true })
  }

  useEffect(() => {
    getWatches()
      .then(async (resp) => {
        setWatchMeta({ count: resp.count, limit: resp.limit, min_interval: resp.min_interval })
        const withResults = await Promise.all(
          (resp.watches || []).map(async (w) => {
            const results = await getWatchResults(w.Name).catch(() => [] as CronResult[])
            return { watch: w, results: results || [] }
          })
        )
        setWatches(withResults)
      })
      .catch(() => {})

    getFunctions().then(fns => setFunctions(fns || [])).catch(() => {})

    getIngestionStatus()
      .then(resp => setAffectedApps(resp.affected_apps || []))
      .catch(() => {})
  }, [])

  void navigate // used in child components

  return (
    <div className="app-layout">
      <SidebarNav />
      <main className="app-main" data-testid="dashboard-main">
        {showWelcome && <WelcomeModal onDismiss={dismissWelcome} />}
        <div className="dash-content">
          <div className="dash-header">
            <h1 className="dash-page-title">Dashboard</h1>
          </div>

          {affectedApps.length > 0 && (
            <div className="dash-load-error" data-testid="dashboard-ingestion-warning">
              Ingestion delayed for some records on: {affectedApps.join(', ')}
            </div>
          )}

          <WatchesCard
            watches={watches}
            watchCount={watchMeta.count}
            watchLimit={watchMeta.limit}
            minInterval={watchMeta.min_interval}
          />
          <FunctionsCard functions={functions} />
        </div>
      </main>
    </div>
  )
}
