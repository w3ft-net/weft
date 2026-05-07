import { useEffect, useMemo, useState } from 'react'
import { useParams, useSearchParams, Link } from 'react-router-dom'
import {
  LineChart, Line, XAxis, YAxis, CartesianGrid, Tooltip,
  ReferenceLine, ReferenceArea, ResponsiveContainer,
} from 'recharts'
import SidebarNav from '../components/SidebarNav'
import { colorForTag, niceTimeTicks, formatTimeTick } from '../components/Sparkline'
import { useAuth } from '../lib/AuthContext'
import {
  getWatch, getWatchChart,
  type CronWatch, type ChartRange, type ChartPoint, type ChartResponse,
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

const TICK_STYLE = { fontSize: 11, fill: '#484f58', fontFamily: "'JetBrains Mono', monospace" }
const GRID_STROKE = '#21262d'
const SAFE_FILL = 'rgba(109, 227, 160, 0.10)'
const THRESH_STROKE = 'rgba(239, 85, 85, 0.65)'

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
          <CartesianGrid strokeDasharray="2 3" stroke={GRID_STROKE} vertical={false} />
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
              fill={SAFE_FILL}
              ifOverflow="hidden"
            />
          )}

          {visible.map(s => (
            <Line
              key={s.tag || '_'}
              dataKey={seriesKey(s.tag)}
              stroke={s.color}
              strokeWidth={1.6}
              dot={false}
              isAnimationActive={false}
              connectNulls={false}
            />
          ))}

          {hasThresh && (
            <ReferenceLine
              y={threshold as number}
              stroke={THRESH_STROKE}
              strokeDasharray="4 3"
              label={false}
            />
          )}

          <Tooltip
            contentStyle={{
              background: '#161b22',
              border: '1px solid #21262d',
              borderRadius: 6,
              fontFamily: "'JetBrains Mono', monospace",
              fontSize: 12,
            }}
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
              ← Dashboards
            </Link>
            <h1 className="watch-detail-name" data-testid="watch-detail-name">
              {name}
            </h1>
            {watch && (
              <div className="watch-detail-meta">
                <span className="watch-detail-schedule">schedule: {watch.Schedule}</span>
                <span className={`watch-detail-status watch-detail-status-${watch.LastStatus || 'pending'}`}>
                  {watch.LastStatus ? `last: ${watch.LastStatus}` : 'waiting for first run'}
                </span>
              </div>
            )}
            {watch && (
              <pre className="watch-detail-pipeline" data-testid="watch-detail-pipeline">
                {watch.Pipeline}
              </pre>
            )}
          </div>

          <div className="watch-detail-chart-card">
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
                    granularity: {chart.granularity}
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
                          <span className="watch-detail-legend-swatch" style={{ background: s.color }} />
                          <span className="watch-detail-legend-tag">{label}</span>
                        </button>
                        <span className="watch-detail-legend-stats">
                          n={s.count} · latest {fmtNum(s.latest)} · avg {fmtNum(s.avg)} · min {fmtNum(s.min)} · max {fmtNum(s.max)}
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
          </div>
        </div>
      </main>
    </div>
  )
}
