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

interface SparkPoint {
  ts: number // unix millis
  value: number
}

interface SparkSeries {
  tag: string // "" for single-row (untagged) watches
  color: string
  points: SparkPoint[]
  latest: string // formatted latest value for the legend
}

// Stable palette — same tag always gets the same color.
const TAG_PALETTE = [
  'var(--accent)',
  '#7ec8ff',
  '#a29bfe',
  '#ffb36b',
  '#6de3a0',
  '#ff7aa8',
  '#ffd36b',
  '#9ef0d8',
]

function colorForTag(tag: string): string {
  if (tag === '') return 'var(--accent)'
  let h = 0
  for (let i = 0; i < tag.length; i++) {
    h = (h * 31 + tag.charCodeAt(i)) & 0xffff
  }
  return TAG_PALETTE[h % TAG_PALETTE.length]
}

function groupResultsByTag(results: CronResult[]): SparkSeries[] {
  const byTag = new Map<string, CronResult[]>()
  for (const r of results) {
    const key = r.Tag ?? ''
    if (!byTag.has(key)) byTag.set(key, [])
    byTag.get(key)!.push(r)
  }

  const series: SparkSeries[] = []
  for (const [tag, rows] of byTag) {
    const points: SparkPoint[] = []
    for (const r of rows) {
      const v = r.Value !== null && r.Value !== undefined ? r.Value : parseFloat(r.Stdout?.trim())
      if (typeof v === 'number' && !isNaN(v)) {
        points.push({ ts: new Date(r.RanAt).getTime(), value: v })
      }
    }
    if (points.length === 0) continue
    points.sort((a, b) => a.ts - b.ts)
    series.push({
      tag,
      color: colorForTag(tag),
      points,
      latest: formatSparkValue(points[points.length - 1].value),
    })
  }
  // Legend order: tag alphabetical, empty tag first so the primary
  // series anchors the chart.
  series.sort((a, b) => {
    if (a.tag === '') return -1
    if (b.tag === '') return 1
    return a.tag.localeCompare(b.tag)
  })
  return series
}

function formatSparkValue(v: number): string {
  if (Number.isInteger(v)) return v.toString()
  return v.toFixed(2).replace(/\.?0+$/, '')
}

function Sparkline({ series }: { series: SparkSeries[] }) {
  const drawable = series.filter(s => s.points.length >= 2)
  if (drawable.length === 0) return null

  const w = 120
  const h = 30
  const pad = 2

  // Shared scale across every series so tags are visually comparable.
  let tMin = Infinity, tMax = -Infinity, vMin = Infinity, vMax = -Infinity
  for (const s of drawable) {
    for (const p of s.points) {
      if (p.ts < tMin) tMin = p.ts
      if (p.ts > tMax) tMax = p.ts
      if (p.value < vMin) vMin = p.value
      if (p.value > vMax) vMax = p.value
    }
  }
  const tRange = tMax - tMin || 1
  const vRange = vMax - vMin || 1

  return (
    <svg width={w} height={h} viewBox={`0 0 ${w} ${h}`} className="watch-sparkline">
      {drawable.map(s => {
        const pts = s.points.map(p => {
          const x = pad + ((p.ts - tMin) / tRange) * (w - pad * 2)
          const y = h - pad - ((p.value - vMin) / vRange) * (h - pad * 2)
          return `${x},${y}`
        }).join(' ')
        const last = s.points[s.points.length - 1]
        const lastX = pad + ((last.ts - tMin) / tRange) * (w - pad * 2)
        const lastY = h - pad - ((last.value - vMin) / vRange) * (h - pad * 2)
        return (
          <g key={s.tag || '_'}>
            <polyline
              points={pts}
              fill="none"
              stroke={s.color}
              strokeWidth="1.5"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
            <circle cx={lastX} cy={lastY} r="2.5" fill={s.color} />
          </g>
        )
      })}
    </svg>
  )
}

function SparkLegend({ series }: { series: SparkSeries[] }) {
  return (
    <ul className="watch-legend" data-testid="watch-legend">
      {series.map(s => (
        <li
          key={s.tag || '_'}
          className="watch-legend-row"
          data-testid={`watch-legend-row-${s.tag || '_'}`}
        >
          <span className="watch-legend-swatch" style={{ background: s.color }} />
          <span className="watch-legend-tag">{s.tag}</span>
          <span className="watch-legend-value">{s.latest}</span>
        </li>
      ))}
    </ul>
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
          // Cap sparkline history at 50 rows to keep the chart readable
          // even when a multi-row watch fires many tags per tick.
          const series = groupResultsByTag(results.slice(0, 50))
          const tagged = series.some(s => s.tag !== '')
          // Single untagged series keeps the original big-number display;
          // tagged or empty series show just the chart + legend.
          const scalarLatest = !tagged && series.length === 1
            ? series[0].latest
            : latestResult?.Stdout?.trim() ?? '—'
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

              {tagged ? (
                <div className="watch-tile-body watch-tile-body-tagged">
                  <Sparkline series={series} />
                  <SparkLegend series={series} />
                </div>
              ) : (
                <div className="watch-tile-body">
                  <div className="watch-tile-value" data-testid={`dashboard-watch-value-${watch.Name}`}>
                    {scalarLatest}
                  </div>
                  <Sparkline series={series} />
                </div>
              )}

              <div className="watch-tile-footer">
                {lastRanAgo ? (
                  <span className={`watch-tile-ago${hasError ? ' watch-tile-ago-err' : ''}`}>
                    {hasError ? 'failed ' : ''}{lastRanAgo}
                  </span>
                ) : (
                  <span className="watch-tile-ago">waiting for first run</span>
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
