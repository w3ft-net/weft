import { useState, useEffect } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import SidebarNav from '../components/SidebarNav'
import {
  getWatches, getWatchResults, getFunctions, getIngestionStatus,
  dismissWelcome as apiDismissWelcome,
  type CronWatch, type CronResult, type ShellFunction,
} from '../lib/api'
import { useAuth } from '../lib/AuthContext'
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

interface SparklineProps {
  series: SparkSeries[]
  // When the watch has an `above`/`below`/`eq`/`neq` alert, the
  // threshold renders as a thin red line. For above/below we also
  // tint the "safe" side of the chart pale green. eq/neq draw only
  // the line — there's no area-shaped safe region for them.
  threshold?: number | null
  condition?: 'above' | 'below' | 'eq' | 'neq' | null
}

function Sparkline({ series, threshold, condition }: SparklineProps) {
  const drawable = series.filter(s => s.points.length >= 2)
  if (drawable.length === 0) return null

  const w = 120
  const h = 30
  const pad = 2

  // Shared scale across every series so tags are visually comparable.
  // When a threshold is set, expand the y-range to include it so the
  // line is always in-frame (otherwise it gets clipped off-chart).
  let tMin = Infinity, tMax = -Infinity, vMin = Infinity, vMax = -Infinity
  for (const s of drawable) {
    for (const p of s.points) {
      if (p.ts < tMin) tMin = p.ts
      if (p.ts > tMax) tMax = p.ts
      if (p.value < vMin) vMin = p.value
      if (p.value > vMax) vMax = p.value
    }
  }
  const hasThresh = typeof threshold === 'number' && !Number.isNaN(threshold)
  if (hasThresh) {
    if ((threshold as number) < vMin) vMin = threshold as number
    if ((threshold as number) > vMax) vMax = threshold as number
  }
  const tRange = tMax - tMin || 1
  const vRange = vMax - vMin || 1

  const yForValue = (v: number) =>
    h - pad - ((v - vMin) / vRange) * (h - pad * 2)

  const thresholdY = hasThresh ? yForValue(threshold as number) : null

  return (
    <svg width={w} height={h} viewBox={`0 0 ${w} ${h}`} className="watch-sparkline">
      {/* Safe-side green tint. SVG y grows downward: "above" means
          values greater than threshold → small y → safe is below
          the line → rect from thresholdY to bottom. "below" is the
          mirror. eq/neq don't get a fill. */}
      {hasThresh && (condition === 'above' || condition === 'below') && thresholdY !== null && (
        <rect
          x={0}
          y={condition === 'above' ? thresholdY : 0}
          width={w}
          height={condition === 'above' ? h - thresholdY : thresholdY}
          className="watch-sparkline-safe"
        />
      )}

      {drawable.map(s => {
        const pts = s.points.map(p => {
          const x = pad + ((p.ts - tMin) / tRange) * (w - pad * 2)
          const y = yForValue(p.value)
          return `${x},${y}`
        }).join(' ')
        const last = s.points[s.points.length - 1]
        const lastX = pad + ((last.ts - tMin) / tRange) * (w - pad * 2)
        const lastY = yForValue(last.value)
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

      {thresholdY !== null && (
        <line
          x1={0}
          x2={w}
          y1={thresholdY}
          y2={thresholdY}
          className="watch-sparkline-threshold"
          data-testid="sparkline-threshold-line"
        />
      )}
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
              role="link"
              tabIndex={0}
              className={`watch-tile watch-tile-clickable${hasError ? ' watch-tile-errored' : ''}`}
              data-testid={`dashboard-watch-card-${watch.Name}`}
              onClick={() => navigate(`/watch/${encodeURIComponent(watch.Name)}`)}
              onKeyDown={(e) => {
                if (e.key === 'Enter' || e.key === ' ') {
                  e.preventDefault()
                  navigate(`/watch/${encodeURIComponent(watch.Name)}`)
                }
              }}
            >
              <div className="watch-tile-header">
                <span className="watch-tile-name">{watch.Name}</span>
                <span className="watch-tile-schedule">{watch.Schedule}</span>
              </div>

              {tagged ? (
                <div className="watch-tile-body watch-tile-body-tagged">
                  <Sparkline
                    series={series}
                    threshold={watch.AlertThreshold}
                    condition={watch.AlertCondition}
                  />
                  <SparkLegend series={series} />
                </div>
              ) : (
                <div className="watch-tile-body">
                  <div className="watch-tile-value" data-testid={`dashboard-watch-value-${watch.Name}`}>
                    {scalarLatest}
                  </div>
                  <Sparkline
                    series={series}
                    threshold={watch.AlertThreshold}
                    condition={watch.AlertCondition}
                  />
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

type Slide = {
  title: string
  body: React.ReactNode
}

// welcomeSlides is a short, paced introduction shown once per user —
// what weft is, how to feed it logs, and where to go next. Each slide
// references the manual for depth rather than trying to be a tutorial
// in its own right.
function welcomeSlides(onDismiss: () => void): Slide[] {
  return [
    {
      title: 'What is weft?',
      body: (
        <p className="welcome-text">
          weft is a log analysis service. You connect AWS accounts or
          install a small agent, and every log line lands in weft's
          time-sharded store. You query them with the weft shell using
          familiar Unix idioms — grep, awk, jq — backed by a planner
          that pushes predicates into SQL so it stays fast.
        </p>
      ),
    },
    {
      title: 'Hook up a source',
      body: (
        <>
          <p className="welcome-text">
            Start by creating a namespace, then pick an ingest path:
          </p>
          <ul className="welcome-list">
            <li>
              <Link to="/namespaces" onClick={onDismiss} data-testid="welcome-namespaces-link">
                Namespaces
              </Link>{' '}
              — create a namespace and issue an agent token.
            </li>
            <li>
              <Link to="/downloads" onClick={onDismiss} data-testid="welcome-downloads-link">
                Downloads
              </Link>{' '}
              — install <code>weftd</code> on a server you own.
            </li>
            <li>
              <Link to="/manual#connect-cloudwatch" onClick={onDismiss} data-testid="welcome-cloudwatch-link">
                CloudWatch
              </Link>{' '}
              — grant a cross-account role and point weft at a log group.
            </li>
          </ul>
        </>
      ),
    },
    {
      title: 'Try the shell',
      body: (
        <>
          <p className="welcome-text">
            Once logs are flowing, open the{' '}
            <Link to="/shell" onClick={onDismiss} data-testid="welcome-shell-link">Shell</Link>{' '}
            and try <code>cat *.log | head</code>. From there, type{' '}
            <code>help</code> for the topic index or check the{' '}
            <Link to="/manual" onClick={onDismiss} data-testid="welcome-manual-link">Manual</Link>{' '}
            for reference.
          </p>
        </>
      ),
    },
  ]
}

function WelcomeModal({ onDismiss }: { onDismiss: () => void }) {
  const [idx, setIdx] = useState(0)
  const slides = welcomeSlides(onDismiss)
  const slide = slides[idx]
  const isLast = idx === slides.length - 1

  return (
    <div className="welcome-overlay" data-testid="welcome-modal">
      <div className="welcome-card">
        <h2 className="welcome-title" data-testid="welcome-slide-title">
          {slide.title}
        </h2>
        <div className="welcome-body" data-testid={`welcome-slide-${idx + 1}`}>
          {slide.body}
        </div>

        <div className="welcome-footer">
          <div className="welcome-dots" data-testid="welcome-dots">
            {slides.map((_, i) => (
              <span
                key={i}
                className={`welcome-dot ${i === idx ? 'welcome-dot-active' : ''}`}
              />
            ))}
          </div>

          <div className="welcome-actions">
            {idx > 0 && (
              <button
                className="welcome-back"
                onClick={() => setIdx(idx - 1)}
                data-testid="welcome-back-button"
              >
                Back
              </button>
            )}
            {!isLast ? (
              <button
                className="login-submit"
                onClick={() => setIdx(idx + 1)}
                data-testid="welcome-next-button"
              >
                Next
              </button>
            ) : (
              <button
                className="login-submit"
                onClick={onDismiss}
                data-testid="welcome-dismiss-button"
              >
                Got it
              </button>
            )}
          </div>
        </div>
      </div>
    </div>
  )
}

// ── Main Dashboard ─────────────────────────────────────────────────────────

export default function Dashboard() {
  const { user } = useAuth()
  const [welcomeDismissed, setWelcomeDismissed] = useState(false)
  const [watches, setWatches] = useState<WatchWithResults[]>([])
  const [watchMeta, setWatchMeta] = useState<{ count: number; limit: number; min_interval: string }>({ count: 0, limit: 0, min_interval: '' })
  const [functions, setFunctions] = useState<ShellFunction[]>([])
  const [affectedApps, setAffectedApps] = useState<string[]>([])

  // Show the welcome modal until the user dismisses it. The server
  // remembers the choice via POST /api/account/welcome-dismiss, so
  // subsequent logins skip the modal. We also keep a local
  // `welcomeDismissed` flag so the modal can close without waiting
  // for /api/auth/me to round-trip.
  const showWelcome = user?.has_seen_welcome === false && !welcomeDismissed

  function dismissWelcome() {
    setWelcomeDismissed(true)
    // Fire-and-forget: if the POST fails the modal will re-appear on
    // next login, which is the correct failure mode.
    apiDismissWelcome().catch(() => {})
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
