import { useState, useEffect } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import SidebarNav from '../components/SidebarNav'
import { Sparkline, SparkLegend, groupResultsByTag } from '../components/Sparkline'
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
          <h2 className="dash-card-title">Crons</h2>
        </div>
        <div className="dash-empty">
          Crons are not available on the trial plan.{' '}
          <a href="/account">Upgrade</a> to enable scheduled monitoring.
        </div>
      </div>
    )
  }

  if (watches.length === 0) {
    return (
      <div className="dash-card" data-testid="dashboard-watches-card">
        <div className="dash-card-header">
          <h2 className="dash-card-title">Crons</h2>
          <span className="watch-count-badge">{watchCount} / {watchLimit}</span>
        </div>
        <div className="dash-empty">
          No crons yet. Create one in the{' '}
          <a href="/shell" data-testid="dashboard-watches-shell-link">shell</a>
          {' '}with <code>vi /cron.d/my-cron</code>
          {minInterval && <span className="dash-empty"> (min interval: {minInterval})</span>}
        </div>
      </div>
    )
  }

  return (
    <div className="dash-card" data-testid="dashboard-watches-card">
      <div className="dash-card-header">
        <h2 className="dash-card-title">Crons</h2>
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
              onClick={() => navigate(`/cron/${encodeURIComponent(watch.Name)}`)}
              onKeyDown={(e) => {
                if (e.key === 'Enter' || e.key === ' ') {
                  e.preventDefault()
                  navigate(`/cron/${encodeURIComponent(watch.Name)}`)
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
  const [affectedServices, setAffectedServices] = useState<string[]>([])

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
          (resp.crons || []).map(async (w) => {
            const results = await getWatchResults(w.Name).catch(() => [] as CronResult[])
            return { watch: w, results: results || [] }
          })
        )
        setWatches(withResults)
      })
      .catch(() => {})

    getFunctions().then(fns => setFunctions(fns || [])).catch(() => {})

    getIngestionStatus()
      .then(resp => setAffectedServices(resp.affected_services || []))
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

          {affectedServices.length > 0 && (
            <div className="dash-load-error" data-testid="dashboard-ingestion-warning">
              Ingestion delayed for some records on: {affectedServices.join(', ')}
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
