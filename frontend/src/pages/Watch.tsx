import { useEffect, useMemo, useState } from 'react'
import { useParams, useSearchParams, Link } from 'react-router-dom'
import {
  LineChart, Line, XAxis, YAxis, CartesianGrid, Tooltip,
  ReferenceLine, ReferenceArea, ResponsiveContainer,
} from 'recharts'
import SidebarNav from '../components/SidebarNav'
import Pane from '../components/Pane'
import { colorForTag, niceTimeTicks, formatTimeTick } from '../components/Sparkline'
import { CHART_COLORS } from '../components/chartColors'
import { useAuth } from '../lib/AuthContext'
import {
  getWatch, getWatchChart, getWatchResults,
  type CronWatch, type CronResult, type ChartRange, type ChartPoint, type ChartResponse,
  CHART_RANGES, CHART_RANGE_MS,
} from '../lib/api'
import './Watch.css'

// ── Plan retention → which ranges are disabled ─────────────────────────────
function retentionMsForTier(tier: string): number {
  switch (tier) {
    case 'swatch': return 30 * 24 * 60 * 60 * 1000
    case 'yard':   return 90 * 24 * 60 * 60 * 1000
    case 'bolt':   return 365 * 24 * 60 * 60 * 1000
    default:       return 24 * 60 * 60 * 1000 // trial
  }
}

// ── Series shaping ─────────────────────────────────────────────────────────

interface Series {
  tag: string
  color: string
  points: ChartPoint[]
  count: number
  avg: number
  min: number
  max: number
  latest: number
}

function buildSeries(points: ChartPoint[]): Series[] {
  const byTag = new Map<string, ChartPoint[]>()
  for (const p of points) {
    const k = p.tag ?? ''
    if (!byTag.has(k)) byTag.set(k, [])
    byTag.get(k)!.push(p)
  }

  const series: Series[] = []
  for (const [tag, pts] of byTag) {
    pts.sort((a, b) => a.ts - b.ts)
    let count = 0, sum = 0, min = Infinity, max = -Infinity
    for (const p of pts) {
      count += p.count
      sum += p.value * p.count
      if (p.min < min) min = p.min
      if (p.max > max) max = p.max
    }
    series.push({
      tag,
      color: colorForTag(tag),
      points: pts,
      count,
      avg: count > 0 ? sum / count : 0,
      min: min === Infinity ? 0 : min,
      max: max === -Infinity ? 0 : max,
      latest: pts[pts.length - 1].value,
    })
  }
  series.sort((a, b) => {
    if (a.tag === '') return -1
    if (b.tag === '') return 1
    return a.tag.localeCompare(b.tag)
  })
  return series
}

function fmtNum(v: number): string {
  if (!Number.isFinite(v)) return '—'
  if (Number.isInteger(v)) return v.toString()
  if (Math.abs(v) >= 1000) return v.toFixed(0)
  return v.toFixed(2).replace(/\.?0+$/, '')
}

function seriesKey(tag: string): string {
  return tag || '_default'
}

// ── Chart ──────────────────────────────────────────────────────────────────

const TICK_STYLE = { fontSize: 11, fill: CHART_COLORS.tick, fontFamily: "'JetBrains Mono', monospace" }

interface ChartProps {
  series: Series[]
  hidden: Set<string>
  threshold: number | null
  condition: CronWatch['AlertCondition']
  granularity: ChartResponse['granularity']
  rangeStartMs: number
  rangeEndMs: number
}

function WatchChart({ series, hidden, threshold, condition, rangeStartMs, rangeEndMs }: ChartProps) {
  const visible = series.filter(s => !hidden.has(s.tag))

  const data = useMemo(() => {
    const byTs = new Map<number, Record<string, number>>()
    for (const s of visible) {
      const key = seriesKey(s.tag)
      for (const p of s.points) {
        let row = byTs.get(p.ts)
        if (!row) {
          row = { ts: p.ts }
          byTs.set(p.ts, row)
        }
        row[key] = p.value
      }
    }
    return [...byTs.values()].sort((a, b) => a.ts - b.ts)
  }, [visible])

  let vMin = Infinity, vMax = -Infinity
  for (const s of visible) {
    for (const p of s.points) {
      if (p.value < vMin) vMin = p.value
      if (p.value > vMax) vMax = p.value
    }
  }
  const hasThresh = typeof threshold === 'number' && !Number.isNaN(threshold)
  if (hasThresh) {
    if ((threshold as number) < vMin) vMin = threshold as number
    if ((threshold as number) > vMax) vMax = threshold as number
  }
  if (!isFinite(vMin) || !isFinite(vMax)) { vMin = 0; vMax = 1 }
  if (vMin === vMax) { vMin -= 0.5; vMax += 0.5 }

  const spanMs = rangeEndMs - rangeStartMs
  const xTicks = niceTimeTicks(rangeStartMs, rangeEndMs, 8)

  return (
    <div className="watch-chart-wrap" data-testid="watch-chart-svg">
      <ResponsiveContainer width="100%" height={240}>
        <LineChart data={data} margin={{ top: 8, right: 12, bottom: 4, left: 4 }}>
          <CartesianGrid strokeDasharray="1 3" stroke={CHART_COLORS.grid} vertical={false} />
          <XAxis
            dataKey="ts"
            type="number"
            domain={[rangeStartMs, rangeEndMs]}
            ticks={xTicks}
            tickFormatter={(v: number) => formatTimeTick(v, spanMs)}
            tick={TICK_STYLE}
            axisLine={false}
            tickLine={false}
          />
          <YAxis
            domain={[vMin, vMax]}
            tickFormatter={fmtNum}
            tick={TICK_STYLE}
            axisLine={false}
            tickLine={false}
            width={48}
          />

          {hasThresh && (condition === 'above' || condition === 'below') && (
            <ReferenceArea
              y1={condition === 'above' ? vMin : (threshold as number)}
              y2={condition === 'above' ? (threshold as number) : vMax}
              fill={CHART_COLORS.safe}
              ifOverflow="hidden"
            />
          )}

          {visible.map(s => (
            <Line
              key={s.tag || '_'}
              dataKey={seriesKey(s.tag)}
              stroke={s.color}
              strokeWidth={1}
              dot={false}
              isAnimationActive={false}
              connectNulls={false}
            />
          ))}

          {hasThresh && (
            <ReferenceLine
              y={threshold as number}
              stroke={CHART_COLORS.crit}
              strokeDasharray="4 3"
              label={false}
            />
          )}

          <Tooltip
            contentStyle={{
              background: CHART_COLORS.surface,
              border: `1px solid ${CHART_COLORS.border}`,
              borderRadius: 0,
              color: CHART_COLORS.text,
              fontFamily: "'JetBrains Mono', monospace",
              fontSize: 12,
            }}
            cursor={{ stroke: CHART_COLORS.border, strokeDasharray: '2 2' }}
            labelFormatter={(v) => {
              const d = new Date(Number(v))
              return d.toUTCString().replace(' GMT', ' UTC')
            }}
            formatter={(v) => [fmtNum(Number(v)), '']}
          />
        </LineChart>
      </ResponsiveContainer>
    </div>
  )
}

// ── Page ───────────────────────────────────────────────────────────────────

export default function Watch() {
  const { name = '' } = useParams()
  const [searchParams, setSearchParams] = useSearchParams()
  const { user } = useAuth()

  const range = (searchParams.get('range') as ChartRange) ?? '24h'
  const hiddenParam = searchParams.get('hide') ?? ''
  const hidden = useMemo(
    () => new Set(hiddenParam ? hiddenParam.split(',').filter(Boolean) : []),
    [hiddenParam],
  )

  const [watch, setWatch] = useState<CronWatch | null>(null)
  const [chart, setChart] = useState<ChartResponse | null>(null)
  const [runs, setRuns] = useState<CronResult[] | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    if (!name) return
    getWatch(name)
      .then(setWatch)
      .catch(e => setError(String(e)))
  }, [name])

  useEffect(() => {
    if (!name) return
    getWatchResults(name)
      .then(setRuns)
      .catch(() => setRuns([]))
  }, [name])

  useEffect(() => {
    if (!name) return
    setLoading(true)
    getWatchChart(name, range)
      .then(r => { setChart(r); setError(null) })
      .catch(e => setError(String(e)))
      .finally(() => setLoading(false))
  }, [name, range])

  const series = useMemo(() => buildSeries(chart?.points ?? []), [chart])

  function selectRange(r: ChartRange) {
    const next = new URLSearchParams(searchParams)
    next.set('range', r)
    setSearchParams(next, { replace: true })
  }

  function toggleTag(tag: string) {
    const nextHidden = new Set(hidden)
    if (nextHidden.has(tag)) nextHidden.delete(tag)
    else nextHidden.add(tag)
    const next = new URLSearchParams(searchParams)
    if (nextHidden.size === 0) next.delete('hide')
    else next.set('hide', Array.from(nextHidden).sort().join(','))
    setSearchParams(next, { replace: true })
  }

  const retentionMs = retentionMsForTier(user?.tier ?? 'trial')
  const now = Date.now()
  const rangeStartMs = now - CHART_RANGE_MS[range]

  return (
    <div className="app-layout">
      <SidebarNav />
      <main className="app-main">
        <div className="watch-detail" data-testid="watch-detail-main">
          <div className="watch-detail-header">
            <Link to="/dashboards" className="watch-detail-back" data-testid="watch-detail-back-link">
              dashboards
            </Link>
            <span className="watch-detail-crumb-sep">/</span>
            <h1 className="watch-detail-name" data-testid="watch-detail-name">
              {name}
            </h1>
            {watch && (
              <div className="watch-detail-meta">
                <span className="watch-detail-schedule">
                  <span className="tui-dim">cron</span> {watch.Schedule}
                </span>
                <span className={`tui-tag watch-detail-status watch-detail-status-${watch.LastStatus || 'pending'}`}>
                  {watch.LastStatus || 'waiting for first run'}
                </span>
              </div>
            )}
          </div>

          {watch && (
            <pre className="watch-detail-pipeline" data-testid="watch-detail-pipeline">
              {watch.Pipeline}
            </pre>
          )}

          <Pane
            className="watch-detail-chart-card"
            title="history"
            meta={chart ? `${range} · ${chart.granularity}` : undefined}
          >
            {error && (
              <div className="watch-detail-error" data-testid="watch-detail-error">{error}</div>
            )}
            {loading && !chart && (
              <div className="watch-detail-loading">loading…</div>
            )}
            {chart && (
              <>
                <WatchChart
                  series={series}
                  hidden={hidden}
                  threshold={watch?.AlertThreshold ?? null}
                  condition={watch?.AlertCondition ?? null}
                  granularity={chart.granularity}
                  rangeStartMs={rangeStartMs}
                  rangeEndMs={now}
                />

                <div className="watch-detail-ranges" data-testid="watch-detail-ranges">
                  {CHART_RANGES.map(r => {
                    const overCap = CHART_RANGE_MS[r] > retentionMs
                    return (
                      <button
                        key={r}
                        type="button"
                        className={`watch-range-btn${r === range ? ' watch-range-btn-active' : ''}${overCap ? ' watch-range-btn-disabled' : ''}`}
                        disabled={overCap}
                        title={overCap ? `beyond ${user?.tier ?? 'trial'} plan retention` : undefined}
                        onClick={() => selectRange(r)}
                        data-testid={`watch-range-${r}-button`}
                      >
                        {r}
                      </button>
                    )
                  })}
                  <span className="watch-detail-gran" data-testid="watch-detail-granularity">
                    {chart.granularity}
                  </span>
                </div>

                <ul className="watch-detail-legend" data-testid="watch-detail-legend">
                  {series.map(s => {
                    const off = hidden.has(s.tag)
                    const label = s.tag === '' ? '(untagged)' : s.tag
                    return (
                      <li
                        key={s.tag || '_'}
                        className={`watch-detail-legend-row${off ? ' watch-detail-legend-off' : ''}`}
                        data-testid={`watch-detail-legend-row-${s.tag || '_'}`}
                      >
                        <button
                          type="button"
                          className="watch-detail-legend-toggle"
                          onClick={() => toggleTag(s.tag)}
                          data-testid={`watch-detail-legend-toggle-${s.tag || '_'}`}
                        >
                          <span className="watch-detail-legend-swatch" style={{ color: s.color }}>■</span>
                          <span className="watch-detail-legend-tag">{label}</span>
                        </button>
                        <span className="watch-detail-legend-stats">
                          <span>n <b>{s.count}</b></span>
                          <span>last <b className="tui-hi">{fmtNum(s.latest)}</b></span>
                          <span>avg <b>{fmtNum(s.avg)}</b></span>
                          <span>min <b>{fmtNum(s.min)}</b></span>
                          <span>max <b>{fmtNum(s.max)}</b></span>
                        </span>
                      </li>
                    )
                  })}
                  {series.length === 0 && (
                    <li className="watch-detail-legend-empty">no data in this range</li>
                  )}
                </ul>
              </>
            )}
          </Pane>

          <RecentRuns runs={runs} />
        </div>
      </main>
    </div>
  )
}

// ── Recent runs ────────────────────────────────────────────────────────────

const RECENT_RUNS = 10

// RecentRuns lists the latest executions like a job log: when, how long,
// exit status, parsed value. The newest run's output is shown below in a
// terminal block so you can see exactly what the pipeline printed.
function RecentRuns({ runs }: { runs: CronResult[] | null }) {
  if (runs === null) return null
  const recent = runs.slice(0, RECENT_RUNS)
  const last = recent[0]
  return (
    <Pane
      className="watch-runs"
      title="runs"
      meta={runs.length > 0 ? `last ${recent.length} of ${runs.length}` : undefined}
      data-testid="watch-runs-pane"
    >
      {recent.length === 0 ? (
        <div className="watch-detail-legend-empty">no runs yet</div>
      ) : (
        <>
          <table className="tui-table watch-runs-table" data-testid="watch-runs-table">
            <thead>
              <tr>
                <th>ran at (utc)</th>
                <th>status</th>
                <th className="watch-num">took</th>
                <th>tag</th>
                <th className="watch-num">value</th>
              </tr>
            </thead>
            <tbody>
              {recent.map(r => (
                <tr key={r.ID} data-testid={`watch-run-row-${r.ID}`}>
                  <td className="tui-dim">{new Date(r.RanAt).toISOString().slice(0, 19).replace('T', ' ')}</td>
                  <td>
                    <span className={`tui-tag ${r.ExitCode === 0 ? 'tui-tag-ok' : 'tui-tag-err'}`}>
                      {r.ExitCode === 0 ? 'ok' : `fail ${r.ExitCode}`}
                    </span>
                  </td>
                  <td className="watch-num">{r.DurationMs}ms</td>
                  <td>{r.Tag || <span className="tui-dim">-</span>}</td>
                  <td className="watch-num tui-hi">{r.Value === null ? <span className="tui-dim">-</span> : fmtNum(r.Value)}</td>
                </tr>
              ))}
            </tbody>
          </table>
          {last && (last.Stdout || last.Stderr) && (
            <div className="watch-runs-output" data-testid="watch-runs-output">
              <div className="watch-runs-output-head">output of last run</div>
              {last.Stdout && <pre className="watch-runs-stdout">{last.Stdout.trimEnd()}</pre>}
              {last.Stderr && <pre className="watch-runs-stderr">{last.Stderr.trimEnd()}</pre>}
            </div>
          )}
        </>
      )}
    </Pane>
  )
}
